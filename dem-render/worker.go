package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// cs2EnvError wraps a render failure that is caused by the local CS2/Steam environment
// rather than a bad demofile. The worker retries on this error.
type cs2EnvError struct{ err error }

func (e cs2EnvError) Error() string { return e.err.Error() }
func (e cs2EnvError) Unwrap() error { return e.err }

// cs2HangError wraps a render failure caused by a CS2 hang that is not safe to retry
// without restarting CS2 — covers plugin loops and broken pipe storms.
type cs2HangError struct{ err error }

func (e cs2HangError) Error() string { return e.err.Error() }
func (e cs2HangError) Unwrap() error { return e.err }

// errZeroIntervals is returned when CS2 ran successfully but encoded 0 videos.
var errZeroIntervals = errors.New("zero intervals encoded")

// runWorker is the entry point for `dem-render worker`.
// It scans a directory for .dem files, renders each one, and writes a .log file per demo.
// Demos that already have a .log file are skipped (idempotent).
func runWorker(args []string) {
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	fs.Usage = func() { printUsage(os.Stderr) }
	inputDir := fs.String("input", envOrDefault("CS_INPUT_DIR", ""), "Directory containing .dem files to process (or CS_INPUT_DIR env)")
	outputDir := fs.String("output", envOrDefault("CS_OUTPUT_DIR", ""), "Directory for output videos and parquets (or CS_OUTPUT_DIR env)")
	buildConfig := addRenderFlags(fs)
	fs.Parse(args)
	baseConfig := buildConfig()

	if *inputDir == "" {
		log.Fatal("worker: --input or CS_INPUT_DIR is required")
	}
	if *outputDir == "" {
		log.Fatal("worker: --output or CS_OUTPUT_DIR is required")
	}

	if err := os.MkdirAll(*outputDir, 0755); err != nil {
		log.Fatalf("worker: failed to create output dir: %v", err)
	}

	log.Printf("worker started (input=%s, output=%s)", *inputDir, *outputDir)
	cleanupOrphanedTempFiles()
	waitForSteam()

	entries, err := os.ReadDir(*inputDir)
	if err != nil {
		log.Fatalf("worker: failed to read input dir: %v", err)
	}

	var processed, skipped, failed int
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".dem") {
			continue
		}

		demoPath := filepath.Join(*inputDir, entry.Name())
		baseName := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		logPath := filepath.Join(*outputDir, baseName+".log")

		// Skip if already processed
		if _, err := os.Stat(logPath); err == nil {
			log.Printf("worker: skipping %s (log already exists)", entry.Name())
			skipped++
			continue
		}

		log.Printf("worker: processing %s", entry.Name())
		if err := processDemoFile(demoPath, baseName, *outputDir, logPath, baseConfig); err != nil {
			log.Printf("worker: failed to process %s: %v", entry.Name(), err)
			writeErrorLog(logPath, err)
			failed++
		} else {
			processed++
		}
	}

	log.Printf("worker: done — processed=%d skipped=%d failed=%d", processed, skipped, failed)
}

// processDemoFile renders a single .dem file and writes a .log file with results.
// cfg carries the shared render knobs (resolution, fps); the
// per-demo fields (path, name, output dir) are filled in here.
func processDemoFile(demoPath, baseName, outputDir, logPath string, cfg RenderConfig) error {
	if err := validateCS2DemoFile(demoPath); err != nil {
		return fmt.Errorf("validation: %w", err)
	}

	renderConfig := cfg
	renderConfig.DemoPath = demoPath
	renderConfig.DemofileName = baseName
	renderConfig.OutputDir = outputDir

	playerRoundData, _, err := parseDemoIntervals(demoPath, renderConfig.Framerate)
	if err != nil {
		return fmt.Errorf("parse demo: %w", err)
	}

	if len(playerRoundData) == 0 {
		return writeResultLog(logPath, baseName, 0, 0, "no renderable intervals found (all scoped or empty)")
	}

	renderConfig.PlayerRounds = playerRoundData

	encodedCount, renderedSeconds, err := RenderIntervals(playerRoundData, renderConfig)
	if err != nil {
		if strings.Contains(err.Error(), "demo incompatible") ||
			strings.Contains(err.Error(), "Workshop addon that could not be downloaded") {
			return fmt.Errorf("render: %w", err)
		}
		return cs2EnvError{fmt.Errorf("render: %w", err)}
	}

	if encodedCount == 0 {
		return errZeroIntervals
	}

	return writeResultLog(logPath, baseName, encodedCount, renderedSeconds, "")
}

// parseDemoIntervals runs the first and second demo parsing passes and returns
// the filtered player round intervals.
func parseDemoIntervals(demoPath string, framerate int) ([]playerRoundInfo, time.Duration, error) {
	matchName := strings.TrimSuffix(filepath.Base(demoPath), filepath.Ext(demoPath))

	playerRoundData, matchDuration, err := collectRoundIntervals(demoPath, matchName, framerate)
	if err != nil {
		return nil, 0, fmt.Errorf("first pass: %w", err)
	}

	for i := range playerRoundData {
		playerRoundData[i].FrameData = make([]FrameDataRecord, playerRoundData[i].ExpectedFrames)
	}

	if err := collectFrameData(demoPath, playerRoundData, framerate); err != nil {
		return nil, 0, fmt.Errorf("second pass: %w", err)
	}

	// Filter out scoped intervals.
	filtered := playerRoundData[:0]
	for _, r := range playerRoundData {
		if !r.WasScoped {
			filtered = append(filtered, r)
		}
	}
	return filtered, matchDuration, nil
}

// writeResultLog writes a success .log file for a processed demo.
func writeResultLog(logPath, demoName string, encodedCount int, renderedSeconds int64, note string) error {
	f, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("create log: %w", err)
	}
	defer f.Close()
	fmt.Fprintf(f, "demo: %s\n", demoName)
	fmt.Fprintf(f, "status: ok\n")
	fmt.Fprintf(f, "encoded_videos: %d\n", encodedCount)
	fmt.Fprintf(f, "rendered_seconds: %d\n", renderedSeconds)
	if note != "" {
		fmt.Fprintf(f, "note: %s\n", note)
	}
	fmt.Fprintf(f, "finished_at: %s\n", time.Now().Format(time.RFC3339))
	return nil
}

// writeErrorLog writes an error .log file for a failed demo.
func writeErrorLog(logPath string, renderErr error) {
	f, err := os.Create(logPath)
	if err != nil {
		log.Printf("worker: failed to create error log %s: %v", logPath, err)
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "status: error\n")
	fmt.Fprintf(f, "error: %s\n", renderErr.Error())
	fmt.Fprintf(f, "finished_at: %s\n", time.Now().Format(time.RFC3339))
}

// waitForSteam blocks until Steam has finished initializing by tailing the
// steam.service journalctl output and waiting for the GPU topology response to
// be written. Falls back to a 60s sleep if journalctl is unavailable.
func waitForSteam() {
	const readyMarker = "Saving response to: /tmp/steam"

	log.Printf("worker: waiting for Steam to become ready (watching journalctl -u steam.service)...")

	cmd := exec.Command("journalctl", "-f", "-u", "steam.service", "--no-pager", "-n", "0")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("worker: waitForSteam: pipe error: %v — sleeping 60s instead", err)
		time.Sleep(60 * time.Second)
		return
	}
	if err := cmd.Start(); err != nil {
		log.Printf("worker: waitForSteam: failed to start journalctl: %v — sleeping 60s instead", err)
		time.Sleep(60 * time.Second)
		return
	}
	defer cmd.Process.Kill()

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), readyMarker) {
			log.Printf("worker: Steam is ready")
			return
		}
	}

	log.Printf("worker: journalctl ended without seeing ready marker — proceeding anyway")
}

// cleanupOrphanedTempFiles removes cs2_dem_*.dem and cs2_out_* directories left in /tmp
// by previous worker runs that were killed before their deferred cleanup could run.
func cleanupOrphanedTempFiles() {
	tmpDir := os.TempDir()
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		log.Printf("worker: cleanup: failed to read %s: %v", tmpDir, err)
		return
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "cs2_dem_") || strings.HasPrefix(name, "cs2_out_") {
			path := filepath.Join(tmpDir, name)
			if err := os.RemoveAll(path); err != nil {
				log.Printf("worker: cleanup: failed to remove %s: %v", path, err)
			} else {
				log.Printf("worker: cleanup: removed orphaned temp file %s", path)
			}
		}
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
