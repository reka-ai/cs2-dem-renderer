package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Action represents a single command to execute at a specific tick
type Action struct {
	Cmd  string `json:"cmd"`
	Tick int    `json:"tick"`
}

// Sequence represents a collection of actions for a demo playback
type Sequence struct {
	Actions []Action `json:"actions"`
}

// RenderConfig contains parameters for rendering intervals
type RenderConfig struct {
	DemoPath     string
	DemofileName string // Base name of the demofile (no extension)
	Width        int
	Height       int
	Framerate    int
	OutputDir    string
	PlayerRounds []playerRoundInfo // Round data used to write parquet files after encoding
}

// cs2Tickrate is the tick rate of CS2 demos. CS2 runs its simulation at 64 tick
// (subtick handles sub-tick input precision), so demos are always 64 tick. It is
// used only to convert seconds to ticks for action-script timing below.
const cs2Tickrate = 64

// firstActionsTick is the demo tick at which each sequence's setup commands and the
// initial demo_gototick fire. The gototick that precedes recording (startTick-6) must land
// strictly after this tick, otherwise it jumps behind the setup tick and the sequence
// replays forever — hence the clamp in recordingStartTick.
const firstActionsTick = 64

// recordingStartTick returns the demo tick at which startmovie actually fires for an
// interval. It is clamped so the preceding demo_gototick (startTick-6) stays strictly after
// firstActionsTick (see above). For players who spawn before that tick — e.g. round 1 of
// demos whose ingame tick starts near 0 — recording therefore begins a few ticks after
// SpawnTick rather than at it.
func recordingStartTick(spawnTick int) int {
	return max(firstActionsTick+7, spawnTick)
}

// leadingFramesDropped returns how many leading FrameData entries (which are indexed from
// SpawnTick) have no corresponding video frame because recording starts at
// recordingStartTick rather than at SpawnTick. It mirrors the parser's frame indexing,
// floor((tick-SpawnTick)*fps/64), so the parquet can be realigned to the recorded range.
func leadingFramesDropped(spawnTick, framerate int) int {
	delta := recordingStartTick(spawnTick) - spawnTick
	if delta <= 0 {
		return 0
	}
	return delta * framerate / cs2Tickrate // integer division floors for positive values
}

// createActionsJSONForIntervals creates a JSON file with commands for all intervals
func createActionsJSONForIntervals(demoPath string, intervals []playerRoundInfo, outputDir string, framerate int) (string, error) {
	var sequences []Sequence

	// Prepend a warm-up sequence that idles for 2 minutes before any recording begins.
	// On cold-start instances CS2 compiles shaders and streams textures during this
	// window, so by the time the first real sequence fires the game world is fully loaded.
	warmupTick := firstActionsTick + 2*60*cs2Tickrate
	sequences = append(sequences, Sequence{
		Actions: []Action{
			{Cmd: "go_to_next_sequence", Tick: warmupTick},
			// Fallback quit in case go_to_next_sequence fails to execute (e.g. plugin stuck).
			{Cmd: "quit", Tick: warmupTick + 1000},
		},
	})

	// Setup commands (only in the first sequence)
	setupCommands := []string{
		"sv_cheats 1",
		"volume 1",
		"cl_hud_telemetry_frametime_show 0",
		"cl_hud_telemetry_net_misdelivery_show 0",
		"cl_hud_telemetry_ping_show 0",
		"cl_hud_telemetry_serverrecvmargin_graph_show 0",
		"r_show_build_info 0",
		"cl_draw_only_deathnotices 1",
		"spec_show_xray 0",
		"cl_drawhud 0",
		"r_drawviewmodel 0",
		"cl_demo_predict 0",
		"cl_trueview_show_status 0",
		"",
		fmt.Sprintf("host_framerate %d", framerate),
	}

	// Create a separate sequence for each interval
	for i, interval := range intervals {

		var actions []Action

		// Add setup commands only to the first sequence
		if i == 0 {
			for _, cmd := range setupCommands {
				actions = append(actions, Action{Cmd: cmd, Tick: firstActionsTick})
			}
		}

		actions = append(actions, Action{Cmd: "pause_playback", Tick: firstActionsTick})

		// recordingStartTick clamps startTick so gotoTick (startTick-6) stays strictly
		// after firstActionsTick; otherwise the demo_gototick jumps back past the setup
		// tick and the sequence replays forever. leadingFramesDropped accounts for the
		// frames lost when this clamp pushes recording past SpawnTick.
		startTick := recordingStartTick(interval.SpawnTick)
		endTick := interval.DeathTick - 1
		sequenceName := interval.UUID + "_"

		// Go to tick before the interval starts
		actions = append(actions, Action{Cmd: fmt.Sprintf("demo_gototick %d", startTick-6), Tick: firstActionsTick})

		// Set spectator mode and player
		actions = append(actions, Action{Cmd: "spec_mode 1", Tick: startTick - 4})
		actions = append(actions, Action{Cmd: fmt.Sprintf("spec_player %d", interval.UserId+1), Tick: startTick - 2})

		// Start recording at the start tick (CS2 will output to symlinked movie directory)
		actions = append(actions, Action{Cmd: fmt.Sprintf("startmovie %s", sequenceName), Tick: startTick})

		// Stop recording at the end tick
		actions = append(actions, Action{Cmd: "endmovie", Tick: endTick})

		// Determine if this is the last interval to process
		isLastInterval := (i == len(intervals)-1)

		// Add go_to_next_sequence or quit based on whether it's the last interval
		if isLastInterval {
			actions = append(actions, Action{Cmd: "quit", Tick: endTick + cs2Tickrate})
		} else {
			actions = append(actions, Action{Cmd: "go_to_next_sequence", Tick: endTick + 3})
			// Fallback quit in case go_to_next_sequence fails to execute (e.g. plugin stuck).
			// If go_to_next_sequence succeeds, the plugin pops this sequence so the quit is never reached.
			actions = append(actions, Action{Cmd: "quit", Tick: endTick + 3 + 1000})
		}

		sequences = append(sequences, Sequence{Actions: actions})
	}

	// Create JSON structure (array of sequences)
	jsonData := sequences

	// Write JSON file next to the demo
	jsonPath := demoPath + ".json"

	file, err := os.Create(jsonPath)
	if err != nil {
		return "", fmt.Errorf("failed to create JSON file: %w", err)
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(jsonData); err != nil {
		return "", fmt.Errorf("failed to encode JSON: %w", err)
	}

	fmt.Printf("Created actions file: %s\n", jsonPath)
	return jsonPath, nil
}

// setupMovieSymlink creates a symlink from CS2's movie directory to the target directory
func setupMovieSymlink(targetDir string) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	movieDir := filepath.Join(homeDir, ".steam/steam/steamapps/common/Counter-Strike Global Offensive/game/csgo/dem-render/movie")

	// Check if movie directory exists and is not already a symlink
	info, err := os.Lstat(movieDir)
	if err == nil {
		// If it's a symlink, remove it
		if info.Mode()&os.ModeSymlink != 0 {
			if err := os.Remove(movieDir); err != nil {
				return fmt.Errorf("failed to remove existing symlink: %w", err)
			}
		} else if info.IsDir() {
			// If it's a regular directory, rename it as backup
			backupDir := movieDir + ".backup"
			if err := os.Rename(movieDir, backupDir); err != nil {
				return fmt.Errorf("failed to backup existing directory: %w", err)
			}
			fmt.Printf("Backed up existing movie directory to: %s\n", backupDir)
		}
	}

	// Create the symlink
	if err := os.Symlink(targetDir, movieDir); err != nil {
		return fmt.Errorf("failed to create symlink: %w", err)
	}

	fmt.Printf("Created symlink: %s -> %s\n", movieDir, targetDir)
	return nil
}

// findSteamRuntime finds the Steam Linux Runtime script
func findSteamRuntime() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}

	possiblePaths := []string{
		filepath.Join(homeDir, ".steam/steam/steamapps/common/SteamLinuxRuntime_sniper/_v2-entry-point"),
		filepath.Join(homeDir, ".local/share/Steam/steamapps/common/SteamLinuxRuntime_sniper/_v2-entry-point"),
	}

	for _, path := range possiblePaths {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}

	return "", fmt.Errorf("could not find Steam Linux Runtime script in: %v", possiblePaths)
}

// findCS2Script finds the CS2 launch script
func findCS2Script() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}

	possiblePaths := []string{
		filepath.Join(homeDir, ".local/share/Steam/steamapps/common/Counter-Strike Global Offensive/game/cs2.sh"),
		filepath.Join(homeDir, ".steam/steam/steamapps/common/Counter-Strike Global Offensive/game/cs2.sh"),
	}

	for _, path := range possiblePaths {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}

	return "", fmt.Errorf("could not find cs2.sh in: %v", possiblePaths)
}

// checkSteamPrereqs verifies that the environment is ready to launch Steam/CS2.
// It checks that a display is available and that Steam is installed.
func checkSteamPrereqs() error {
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return fmt.Errorf("no display available: DISPLAY is not set. Start Xvfb first:\n  Xvfb :99 -screen 0 1920x1080x24 &\n  export DISPLAY=:99")
	}

	if _, err := exec.LookPath("steam"); err != nil {
		return fmt.Errorf("steam not found in PATH: %w", err)
	}

	// Check if Steam is already running
	if err := exec.Command("pgrep", "-x", "steam").Run(); err != nil {
		return fmt.Errorf("steam is not running. Start Steam first:\n  DISPLAY=%s steam &\n  (wait for Steam to fully load before running dem-render)", os.Getenv("DISPLAY"))
	}

	waitForShaderCompilation()

	return nil
}

// waitForShaderCompilation blocks until Steam's fossilize_replay shader pre-compilation is done.
func waitForShaderCompilation() {
	if exec.Command("pgrep", "-x", "fossilize_replay").Run() != nil {
		return // not compiling, nothing to wait for
	}

	fmt.Println("Steam is pre-compiling shaders (fossilize_replay), waiting for completion...")
	start := time.Now()
	for {
		time.Sleep(5 * time.Second)
		if exec.Command("pgrep", "-x", "fossilize_replay").Run() != nil {
			fmt.Printf("Shader compilation done (took %.0fs)\n", time.Since(start).Seconds())
			return
		}
		fmt.Printf("  Still compiling shaders... (%.0fs elapsed)\n", time.Since(start).Seconds())
	}
}

// launchCS2 launches CS2 with the demo file using steam -applaunch
func launchCS2(demoPath string, width int, height int, logFile string) (*exec.Cmd, error) {
	if err := checkSteamPrereqs(); err != nil {
		return nil, err
	}

	// Kill any stale CS2 process from a previous crashed or restarted run.
	exec.Command("pkill", "-x", "cs2").Run()

	demoPath, err := filepath.Abs(demoPath)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path: %w", err)
	}

	if _, err := os.Stat(demoPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("demo file not found: %s", demoPath)
	}

	// Find steam command
	steamCmd, err := exec.LookPath("steam")
	if err != nil {
		return nil, fmt.Errorf("steam command not found: %w", err)
	}

	// Build command arguments for steam -applaunch
	args := []string{
		"-offline",
		"-applaunch",
		"730",
		"-insecure",
		"-novid",
		"-high",
		"-forcenovsync",
		"-condebug",
		"+demo_allow_game_mismatch",
		"1",
		"+playdemo",
		demoPath,
		"-width",
		fmt.Sprintf("%d", width),
		"-height",
		fmt.Sprintf("%d", height),
		"-fullscreen",
	}

	fmt.Printf("Launching CS2 with demo: %s\n", demoPath)
	fmt.Printf("Resolution: %dx%d\n", width, height)

	// Create log file if not specified
	if logFile == "" {
		timestamp := time.Now().Format("20060102_150405")
		logFile = fmt.Sprintf("cs2_recording_%s.log", timestamp)
	}

	fmt.Printf("Logging game output to: %s\n", logFile)

	// Create log file
	log, err := os.Create(logFile)
	if err != nil {
		return nil, fmt.Errorf("failed to create log file: %w", err)
	}

	// Write log header
	fmt.Fprintf(log, "CS2 Recording Log\n")
	fmt.Fprintf(log, "Started: %s\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(log, "Demo: %s\n", demoPath)
	fmt.Fprintf(log, "Command: %s %v\n", steamCmd, args)
	fmt.Fprintf(log, "%s\n\n", "================================================================================")
	log.Sync()

	fmt.Printf("Command: %s %v\n", steamCmd, args)

	// Launch via steam -applaunch
	cmd := exec.Command(steamCmd, args...)
	cmd.Stdout = log
	cmd.Stderr = log

	if err := cmd.Start(); err != nil {
		log.Close()
		return nil, fmt.Errorf("failed to start CS2: %w", err)
	}

	fmt.Println("\nGame launch command sent to Steam!")
	fmt.Println("Waiting for CS2 to start...")

	// Wait for steam command to complete (returns immediately)
	cmd.Wait()

	// Now wait for CS2 process to actually start
	fmt.Println("Waiting for CS2 process to appear...")
	var cs2Pid int
	for range 30 { // Wait up to 30 seconds for CS2 to start
		time.Sleep(1 * time.Second)

		// Check if cs2 process exists
		checkCmd := exec.Command("pgrep", "-x", "cs2")
		output, err := checkCmd.Output()
		if err == nil && len(output) > 0 {
			fmt.Sscanf(string(output), "%d", &cs2Pid)
			fmt.Printf("CS2 process started (PID: %d)\n", cs2Pid)
			break
		}
	}

	if cs2Pid == 0 {
		log.Close()
		return nil, fmt.Errorf("CS2 failed to start within 30 seconds")
	}

	fmt.Println("Recording will start automatically.")
	fmt.Println("The game will quit when recording is complete.")
	fmt.Printf("Monitor progress: tail -f %s\n", logFile)

	// Close log file when CS2 exits
	go func() {
		for {
			time.Sleep(2 * time.Second)
			checkCmd := exec.Command("pgrep", "-x", "cs2")
			if err := checkCmd.Run(); err != nil {
				// CS2 process no longer exists
				fmt.Println("\nCS2 has exited")
				log.Close()
				break
			}
		}
	}()

	// Return a command that monitors the CS2 process and checks exit status
	monitorScript := fmt.Sprintf(`
		while kill -0 %d 2>/dev/null; do
			sleep 1
		done
		if [ -f /tmp/dumps/assert_*.dmp ] && [ "$(find /tmp/dumps -name 'assert_*.dmp' -mmin -1)" ]; then
			echo "CS2 crashed - crash dump detected"
			exit 1
		fi
		exit 0
	`, cs2Pid)

	monitorCmd := exec.Command("sh", "-c", monitorScript)
	if err := monitorCmd.Start(); err != nil {
		log.Close()
		return nil, fmt.Errorf("failed to start monitor command: %w", err)
	}

	return monitorCmd, nil
}

// checkDemRenderForLoop polls the plugin log every 30 seconds and kills CS2 if any
// startmovie command is seen more than once — which means the plugin is stuck replaying
// the same sequence in an infinite loop.
func checkDemRenderForLoop(ctx context.Context, logPath string) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(30 * time.Second):
		}

		data, err := os.ReadFile(logPath)
		if err != nil {
			continue
		}

		counts := make(map[string]int)
		for _, line := range strings.Split(string(data), "\n") {
			if idx := strings.Index(line, "Executing: startmovie "); idx >= 0 {
				name := strings.TrimSpace(line[idx+len("Executing: startmovie "):])
				counts[name]++
				if counts[name] > 2 {
					log.Printf("plugin loop detected: startmovie %q fired %d times — killing CS2", name, counts[name])
					exec.Command("pkill", "-x", "cs2").Run()
					return fmt.Errorf("plugin stuck in replay loop for sequence %q", name)
				}
			}
		}
	}
}

// checkForBrokenPipe periodically samples the last 50 journal lines and returns an error if a
// broken pipe storm is detected (>=10 occurrences).
func checkForBrokenPipe(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(30 * time.Second):
		}

		out, err := exec.Command("journalctl", "-n", "50", "--no-pager").Output()
		if err != nil {
			continue
		}

		count := strings.Count(string(out), "broken pipe")
		if count >= 10 {
			log.Printf("checkForBrokenPipe: detected %d broken pipe occurrences in last 50 journal lines — killing CS2", count)
			exec.Command("pkill", "-x", "cs2").Run()
			return fmt.Errorf("broken pipe storm: %d occurrences in journalctl output", count)
		}
	}
}

// detectCS2Crash reports whether the CS2 console.log contains the engine crash marker.
// "Engine2PreBreakpadDumpFunction" is written by the engine's Breakpad pre-dump routine
// immediately before the process dies, so its presence after CS2 exits means CS2 crashed
// rather than quitting cleanly. condebug recreates console.log per launch, so reading it
// after the process exits reflects this run.
func detectCS2Crash(consolePath string) bool {
	data, err := os.ReadFile(consolePath)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), "Engine2PreBreakpadDumpFunction")
}

// checkConsoleForIncompatible polls the CS2 console.log every 10 seconds until the
// context is cancelled or a demo failure message is found. Kills CS2 if found.
//
// Note: the "Discarding pending request 'Playing Demo'" line is NOT used as an
// incompatibility signal — current CS2 emits it during the normal demo double-load
// sequence (queue → discard → re-queue → play), so it appears on successful runs too.
// A genuinely incompatible demo never produces "[Demo] playing demo from" or any frames,
// so it is caught by the demo-started timeout in checkDemoStarted instead.
func checkConsoleForIncompatible(ctx context.Context, consolePath string) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(10 * time.Second):
		}

		data, err := os.ReadFile(consolePath)
		if err != nil {
			continue
		}
		if strings.Contains(string(data), "[Client] Disconnected from server:") {
			exec.Command("pkill", "-x", "cs2").Run()
			return fmt.Errorf("demo aborted: CS2 was unexpectedly disconnected from the demo server")
		}
		if strings.Contains(string(data), "Switch to loop 'addondownload' failed") {
			exec.Command("pkill", "-x", "cs2").Run()
			return fmt.Errorf("demo requires a Workshop addon that could not be downloaded (offline/insecure mode)")
		}
	}
}

// checkDemoStarted polls until the demo is confirmed started, via either of two signals:
//   - the CS2 console.log contains "[Demo] playing demo from", or
//   - framesSeen() reports true (the encoder has detected real rendered frames).
//
// The frames signal is the authoritative one: frames only appear once CS2 is actually
// recording, so it does not depend on the console marker string, which CS2 updates can
// change or drop. onStarted is called once when the demo starts (may be nil). framesSeen
// may be nil. If neither signal fires within timeout, killAndWaitCS2 is called and an
// error is returned.
func checkDemoStarted(ctx context.Context, consolePath string, timeout time.Duration, onStarted func(), framesSeen func() bool) error {
	deadline := time.Now().Add(timeout)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
		}

		if framesSeen != nil && framesSeen() {
			log.Printf("demo started successfully (frames detected)")
			if onStarted != nil {
				onStarted()
			}
			return nil
		}

		data, err := os.ReadFile(consolePath)
		if err == nil && strings.Contains(string(data), "[Demo] playing demo from") {
			log.Printf("console: demo started successfully")
			if onStarted != nil {
				onStarted()
			}
			return nil
		}

		if time.Now().After(deadline) {
			if killErr := killAndWaitCS2(); killErr != nil {
				return fmt.Errorf("demo did not start within %s and CS2 could not be killed: %w", timeout, killErr)
			}
			return fmt.Errorf("demo did not start within %s — CS2 may have failed to load", timeout)
		}
	}
}

// RenderIntervals renders all player spawn-death intervals from the demo in a single CS2 session.
// Returns the number of videos successfully encoded and total rendered duration in seconds.
func RenderIntervals(intervals []playerRoundInfo, config RenderConfig) (int, int64, error) {
	// Create output directory if it doesn't exist
	if config.OutputDir == "" {
		// Use directory where executable is located
		execPath, err := os.Executable()
		if err != nil {
			return 0, 0, fmt.Errorf("failed to get executable path: %w", err)
		}
		config.OutputDir = filepath.Dir(execPath)
	}

	if err := os.MkdirAll(config.OutputDir, 0755); err != nil {
		return 0, 0, fmt.Errorf("failed to create output directory: %w", err)
	}

	fmt.Printf("\nRendering %d intervals to: %s\n", len(intervals), config.OutputDir)

	// Determine CS2's movie output directory and clean it before launch so leftover
	// timestamp dirs from previous runs don't confuse the encoder.
	homeDir, _ := os.UserHomeDir()
	cs2Base := filepath.Join(homeDir, ".steam/steam/steamapps/common/Counter-Strike Global Offensive/game/csgo")
	cs2MovieDir := filepath.Join(cs2Base, "dem-render/movie")
	for _, movieDir := range []string{cs2MovieDir, filepath.Join(cs2Base, "movie")} {
		os.MkdirAll(movieDir, 0755)
		if entries, err := os.ReadDir(movieDir); err == nil {
			for _, e := range entries {
				os.RemoveAll(filepath.Join(movieDir, e.Name()))
			}
		}
	}
	defer os.RemoveAll(cs2MovieDir)

	// Parquet files are written to the output directory
	parquetDir := config.OutputDir

	// Start video encoder manager watching CS2's movie directory.
	encoder := EncodeVideosWithOutput(cs2MovieDir, config.OutputDir, parquetDir, config.Framerate, config.Width, config.Height, config.PlayerRounds)

	// Create a single actions JSON for all intervals
	jsonPath, err := createActionsJSONForIntervals(
		config.DemoPath,
		intervals,
		config.OutputDir,
		config.Framerate,
	)
	if err != nil {
		if encoder != nil {
			encoder.Stop()
		}
		return 0, 0, fmt.Errorf("error creating actions JSON: %w", err)
	}
	defer os.Remove(jsonPath)

	// Launch CS2 once for all intervals
	timestamp := time.Now().Format("20060102_150405")
	logFile := fmt.Sprintf("cs2_recording_%s.log", timestamp)

	cmd, err := launchCS2(config.DemoPath, config.Width, config.Height, logFile)
	if err != nil {
		if encoder != nil {
			encoder.Stop()
		}
		return 0, 0, fmt.Errorf("error launching CS2: %w", err)
	}

	// CS2 writes the +condebug console log into the active game subdirectory, which is
	// the same dem-render/ dir the movie frames go to (csgo/dem-render/console.log) — not
	// csgo/console.log. Reading the wrong path silently disables every console-log check.
	consolePath := filepath.Join(cs2Base, "dem-render", "console.log")
	pluginLogPath := filepath.Join(homeDir, ".steam/steam/steamapps/common/Counter-Strike Global Offensive/game/bin/linuxsteamrt64/dem-render.log")
	checkCtx, cancelCheck := context.WithCancel(context.Background())

	// Verify the demo starts playing within 3 minutes; fail the job if it doesn't.
	demoStartedCh := make(chan error, 1)
	go func() {
		demoStartedCh <- checkDemoStarted(checkCtx, consolePath, 3*time.Minute, encoder.NotifyDemoStarted, encoder.FramesDetected)
	}()

	// Poll console.log every 10s for demo incompatibility; cancel when CS2 exits.
	incompatibleCh := make(chan error, 1)
	go func() {
		incompatibleCh <- checkConsoleForIncompatible(checkCtx, consolePath)
	}()

	// Poll dem-render.log for repeated startmovie commands indicating a stuck replay loop.
	loopCh := make(chan error, 1)
	go func() {
		loopCh <- checkDemRenderForLoop(checkCtx, pluginLogPath)
	}()

	// Poll journalctl for a broken pipe storm.
	brokenPipeCh := make(chan error, 1)
	go func() {
		brokenPipeCh <- checkForBrokenPipe(checkCtx)
	}()

	// Wait for CS2 to finish rendering all intervals
	if err := cmd.Wait(); err != nil {
		log.Printf("CS2 exited with error: %v", err)
	}
	cancelCheck()

	if checkErr := <-demoStartedCh; checkErr != nil {
		return 0, 0, checkErr
	}
	if checkErr := <-incompatibleCh; checkErr != nil {
		return 0, 0, checkErr
	}
	if checkErr := <-loopCh; checkErr != nil {
		return 0, 0, cs2HangError{checkErr}
	}
	if checkErr := <-brokenPipeCh; checkErr != nil {
		return 0, 0, cs2HangError{checkErr}
	}

	var encodedCount int
	var renderedSeconds int64
	if encoder != nil {
		encoder.Stop()
		if hangErr := encoder.HangErr(); hangErr != nil {
			return 0, 0, cs2HangError{hangErr}
		}
		encodedCount = encoder.EncodedCount()
		renderedSeconds = encoder.RenderedSeconds()
	}

	// If CS2 produced fewer videos than expected and its console.log shows the engine
	// crash marker, CS2 crashed mid-render. Without this, a crash that dropped most
	// intervals looks like a clean exit and is wrongly reported as success.
	if encodedCount < len(intervals) && detectCS2Crash(consolePath) {
		return encodedCount, renderedSeconds, cs2HangError{fmt.Errorf("CS2 crashed during rendering — only %d of %d intervals encoded", encodedCount, len(intervals))}
	}

	fmt.Printf("\nAll intervals rendered successfully (%d videos encoded)\n", encodedCount)
	return encodedCount, renderedSeconds, nil
}
