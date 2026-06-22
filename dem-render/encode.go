package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// VideoEncoderManager manages pipe-based streaming encoding of CS2 sequences.
type VideoEncoderManager struct {
	watchDir        string
	outputDir       string
	parquetDir      string
	framerate       int
	width           int
	height          int
	roundsByUUID    map[string]playerRoundInfo
	encodedCount    int64 // atomic
	renderedSeconds int64 // atomic
	demoStartedNano int64 // atomic; UnixNano when demo confirmed started, 0 if not yet
	framesSeenNano  int64 // atomic; UnixNano when the first frame directory was detected, 0 if not yet
	mu              sync.Mutex
	wg              sync.WaitGroup
	stopWatch       chan struct{}
	activePE        *pipeEncoder
	allPEs          []*pipeEncoder
	dirIndex        int
	hangErrCh       chan error // buffered(1): receives hang error if watchDirectory detected a freeze
}

// pipeEncoder streams TGA frames to an ffmpeg process via stdin as CS2 writes them,
// deleting each frame immediately after piping to keep disk usage minimal.
type pipeEncoder struct {
	dir        string
	outputDir  string
	parquetDir string
	framerate  int
	width      int
	height     int
	index      int
	manager    *VideoEncoderManager
	stopCh     chan struct{}
	doneCh     chan struct{}
}

// NewVideoEncoderManager creates a new video encoder manager.
func NewVideoEncoderManager(watchDir string, outputDir string, parquetDir string, framerate int, width int, height int, playerRounds []playerRoundInfo) *VideoEncoderManager {
	roundsByUUID := make(map[string]playerRoundInfo, len(playerRounds))
	for _, r := range playerRounds {
		roundsByUUID[r.UUID] = r
	}
	return &VideoEncoderManager{
		watchDir:     watchDir,
		outputDir:    outputDir,
		parquetDir:   parquetDir,
		framerate:    framerate,
		width:        width,
		height:       height,
		roundsByUUID: roundsByUUID,
		stopWatch:    make(chan struct{}),
		hangErrCh:    make(chan error, 1),
	}
}

// parseTGAInfo reads a TGA file header and returns the ffmpeg pixel format string,
// the byte offset where pixel data begins, and whether a vertical flip is needed.
// Standard TGA images are stored bottom-up; vflip=true signals ffmpeg to correct this.
// Returns pixFmt="" if the pixel depth is unsupported.
func parseTGAInfo(data []byte) (pixFmt string, dataOffset int, vflip bool) {
	if len(data) < 18 {
		return "", 0, false
	}
	idLen := int(data[0])
	colorMapType := data[1]
	colorMapLen := int(binary.LittleEndian.Uint16(data[5:7]))
	colorMapEntryBits := int(data[7])
	colorMapBytes := 0
	if colorMapType != 0 {
		colorMapBytes = colorMapLen * ((colorMapEntryBits + 7) / 8)
	}
	dataOffset = 18 + idLen + colorMapBytes

	switch data[16] { // bits per pixel
	case 24:
		pixFmt = "rgb24"
	case 32:
		pixFmt = "rgba"
	default:
		return "", 0, false
	}

	// Bit 5 of image descriptor: 0 = bottom-left origin (needs vflip), 1 = top-left
	vflip = (data[17] & 0x20) == 0
	return pixFmt, dataOffset, vflip
}

// Start begins watching the output directory for new sequences.
func (m *VideoEncoderManager) Start() error {
	fmt.Println("Starting video encoder manager...")
	m.wg.Add(1)
	go m.watchDirectory()
	return nil
}

// Stop stops the encoder manager and waits for all encoding jobs to complete.
func (m *VideoEncoderManager) Stop() {
	fmt.Println("\nStopping video encoder manager...")
	close(m.stopWatch)
	m.wg.Wait() // wait for watchDirectory to exit

	// Signal the active pipe encoder to drain remaining frames and finish
	m.mu.Lock()
	if m.activePE != nil {
		m.activePE.stop()
	}
	pes := append([]*pipeEncoder(nil), m.allPEs...)
	m.mu.Unlock()

	for _, pe := range pes {
		pe.wait()
	}
	fmt.Println("All encoding jobs completed")
}

// EncodedCount returns the number of videos successfully encoded so far.
func (m *VideoEncoderManager) EncodedCount() int {
	return int(atomic.LoadInt64(&m.encodedCount))
}

// RenderedSeconds returns the total duration of successfully encoded videos in seconds.
func (m *VideoEncoderManager) RenderedSeconds() int64 {
	return atomic.LoadInt64(&m.renderedSeconds)
}

// NotifyDemoStarted records the time the demo confirmed started. Called by RenderIntervals
// once checkDemoStarted succeeds so watchDirectory can enforce the no-frames timeout.
func (m *VideoEncoderManager) NotifyDemoStarted() {
	atomic.StoreInt64(&m.demoStartedNano, time.Now().UnixNano())
}

// FramesDetected reports whether the encoder has seen at least one CS2 frame directory.
// This is the most reliable proof that the demo actually started playing, since frames
// only appear once CS2 is recording — independent of any console.log marker string.
func (m *VideoEncoderManager) FramesDetected() bool {
	return atomic.LoadInt64(&m.framesSeenNano) != 0
}

// HangErr returns the hang error if watchDirectory detected a CS2 freeze, or nil otherwise.
func (m *VideoEncoderManager) HangErr() error {
	select {
	case err := <-m.hangErrCh:
		return err
	default:
		return nil
	}
}

// startNewPipeEncoder stops the current active encoder (if any) and starts a new one for dir.
func (m *VideoEncoderManager) startNewPipeEncoder(dir string) {
	m.mu.Lock()
	if m.activePE != nil {
		m.activePE.stop() // signal previous to drain; it finishes in background
	}
	index := m.dirIndex
	m.dirIndex++
	pe := &pipeEncoder{
		dir:        dir,
		outputDir:  m.outputDir,
		parquetDir: m.parquetDir,
		framerate:  m.framerate,
		width:      m.width,
		height:     m.height,
		index:      index,
		manager:    m,
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
	}
	m.activePE = pe
	m.allPEs = append(m.allPEs, pe)
	m.mu.Unlock()

	go pe.run()
}

// watchDirectory monitors the watch directory for new CS2 timestamp directories.
func (m *VideoEncoderManager) watchDirectory() {
	defer m.wg.Done()

	fmt.Printf("Watching directory: %s\n", m.watchDir)

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	seenDirs := make(map[string]bool)
	const hangTimeout = 2 * time.Minute
	var lastActivityTime time.Time

	for {
		select {
		case <-m.stopWatch:
			return
		case <-ticker.C:
			entries, err := os.ReadDir(m.watchDir)
			if err != nil {
				continue
			}

			for _, entry := range entries {
				if !entry.IsDir() || seenDirs[entry.Name()] {
					continue
				}
				if !isValidTimestampDir(entry.Name()) {
					continue
				}

				dirPath := filepath.Join(m.watchDir, entry.Name())
				seenDirs[entry.Name()] = true
				fmt.Printf("Detected new timestamp directory: %s\n", entry.Name())
				lastActivityTime = time.Now()
				atomic.CompareAndSwapInt64(&m.framesSeenNano, 0, lastActivityTime.UnixNano())

				m.startNewPipeEncoder(dirPath)
			}

			// Hang detection: if no new TGA frames written within hangTimeout, kill CS2.
			if !lastActivityTime.IsZero() {
				if mtime, found := newestTGAMtime(m.watchDir); found && mtime.After(lastActivityTime) {
					lastActivityTime = mtime
				}
				if time.Since(lastActivityTime) > hangTimeout {
					log.Printf("CS2 hang detected: no new frames written in the last %.0f seconds — killing CS2", hangTimeout.Seconds())
					select {
					case m.hangErrCh <- fmt.Errorf("CS2 hang: no new frames in the last %.0f seconds", hangTimeout.Seconds()):
					default:
					}
					killAndWaitCS2()
					return
				}
			} else if nano := atomic.LoadInt64(&m.demoStartedNano); nano != 0 {
				// Demo confirmed started but no timestamp directory (no frames) yet.
				// If this persists for 10 minutes the JSON plugin is stuck and CS2 must be rebooted.
				const noFramesTimeout = 10 * time.Minute
				if time.Since(time.Unix(0, nano)) > noFramesTimeout {
					log.Printf("CS2 hang detected: demo started but no frames appeared within %.0f minutes — killing CS2", noFramesTimeout.Minutes())
					select {
					case m.hangErrCh <- fmt.Errorf("CS2 hang: demo started but no frames appeared within %.0f minutes", noFramesTimeout.Minutes()):
					default:
					}
					killAndWaitCS2()
					return
				}
			}
		}
	}
}

// isValidTimestampDir checks if a directory name matches CS2's timestamp pattern (2025_12_15_13_11_50).
func isValidTimestampDir(dirName string) bool {
	parts := strings.Split(dirName, "_")
	if len(parts) != 6 {
		return false
	}
	for _, part := range parts {
		if len(part) == 0 {
			return false
		}
		for _, c := range part {
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	return true
}

// newestTGAMtime returns the most recent modification time of any .tga file
// found in the immediate subdirectories of dir.
func newestTGAMtime(dir string) (time.Time, bool) {
	var newest time.Time
	entries, err := os.ReadDir(dir)
	if err != nil {
		return newest, false
	}
	found := false
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		subEntries, err := os.ReadDir(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		for _, se := range subEntries {
			if se.IsDir() || !strings.HasSuffix(se.Name(), ".tga") {
				continue
			}
			info, err := se.Info()
			if err == nil && info.ModTime().After(newest) {
				newest = info.ModTime()
				found = true
			}
		}
	}
	return newest, found
}

// killAndWaitCS2 sends SIGTERM to cs2 and waits up to 30 seconds for it to exit.
// Returns an error if CS2 is still alive after the grace period.
func killAndWaitCS2() error {
	log.Println("Sending kill signal to CS2 process...")
	exec.Command("pkill", "-x", "cs2").Run()
	for i := 0; i < 30; i++ {
		time.Sleep(1 * time.Second)
		if exec.Command("pgrep", "-x", "cs2").Run() != nil {
			log.Println("CS2 process has exited")
			return nil
		}
	}
	return fmt.Errorf("CS2 did not exit within 30 seconds")
}

// stop signals the pipeEncoder to drain remaining frames and shut down. Safe to call multiple times.
func (pe *pipeEncoder) stop() {
	select {
	case <-pe.stopCh:
	default:
		close(pe.stopCh)
	}
}

// wait blocks until the pipeEncoder has finished encoding and post-processing.
func (pe *pipeEncoder) wait() {
	<-pe.doneCh
}

// run is the main goroutine for a pipeEncoder. It waits until at least two TGA frames are
// fully written, reads frame 0 to detect the TGA pixel format, then starts ffmpeg in
// rawvideo input mode (no probing required) and streams all frames.
func (pe *pipeEncoder) run() {
	defer close(pe.doneCh)

	// Precompute expected TGA file size from the known resolution.
	// CS2 outputs 32-bit RGBA TGA files: 18-byte header + width*height*4 bytes of pixel data.
	expectedFileSize := int64(pe.width*pe.height*4) + 18

	uuid := pe.waitForSecondTGA(expectedFileSize)
	if uuid == "" {
		return // stopped before two frames appeared
	}

	// Read frame 0 now (guaranteed complete) to detect pixel format and data offset.
	frame0 := filepath.Join(pe.dir, fmt.Sprintf("%s_%08d.tga", uuid, 0))
	frame0Data, err := os.ReadFile(frame0)
	if err != nil {
		log.Printf("pipeEncoder: failed to read frame 0 for %s: %v", uuid, err)
		return
	}
	pixFmt, dataOffset, vflip := parseTGAInfo(frame0Data)
	if pixFmt == "" {
		log.Printf("pipeEncoder: unsupported TGA pixel depth for %s", uuid)
		return
	}

	outputVideo := filepath.Join(pe.outputDir, uuid+".mp4")
	fmt.Printf("Starting VAAPI encode: %s -> %s (fmt=%s vflip=%v)\n", uuid, outputVideo, pixFmt, vflip)

	// Build filter chain: vflip on CPU (if needed) → convert to nv12 → upload to GPU
	vfFilters := []string{}
	if vflip {
		vfFilters = append(vfFilters, "vflip")
	}
	vfFilters = append(vfFilters, "format=nv12", "hwupload")

	ffmpegArgs := []string{
		"-vaapi_device", "/dev/dri/renderD128",
		"-f", "rawvideo",
		"-pixel_format", pixFmt,
		"-s", fmt.Sprintf("%dx%d", pe.width, pe.height),
		"-framerate", fmt.Sprintf("%d", pe.framerate),
		"-i", "pipe:0",
		"-vf", strings.Join(vfFilters, ","),
		"-c:v", "hevc_vaapi",
		"-qp", "23",
		"-threads", "1",
		"-y", outputVideo,
	}

	cmd := exec.Command("ffmpeg", ffmpegArgs...)
	cmd.Env = append(os.Environ(), "LIBVA_DRIVER_NAME=radeonsi")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Printf("pipeEncoder: StdinPipe error for %s: %v", uuid, err)
		return
	}
	if err := cmd.Start(); err != nil {
		log.Printf("pipeEncoder: failed to start ffmpeg for %s: %v", uuid, err)
		return
	}

	// Pipe frame 0 pixel data immediately (header already stripped).
	if _, err := stdin.Write(frame0Data[dataOffset:]); err != nil {
		log.Printf("pipeEncoder: failed to pipe frame 0 for %s: %v", uuid, err)
		stdin.Close()
		cmd.Wait()
		return
	}
	os.Remove(frame0)

	// Stream remaining frames starting at 1, still using the one-frame lag.
	frameCount := pe.streamFrames(uuid, stdin, 1, dataOffset, expectedFileSize)
	stdin.Close()

	if err := cmd.Wait(); err != nil {
		log.Printf("pipeEncoder: ffmpeg error for %s: %v", uuid, err)
		return
	}

	fmt.Printf("Successfully encoded: %s (%d frames)\n", outputVideo, frameCount)
	atomic.AddInt64(&pe.manager.encodedCount, 1)
	if round, ok := pe.manager.roundsByUUID[uuid]; ok {
		atomic.AddInt64(&pe.manager.renderedSeconds, int64(round.Duration))
	}

	if round, ok := pe.manager.roundsByUUID[uuid]; ok {
		parquetFile := filepath.Join(pe.parquetDir, uuid+".parquet")
		if err := writeParquetFile(round, frameCount, pe.parquetDir, pe.framerate, pe.width, pe.height); err != nil {
			log.Printf("parquet write error for %s: %v", uuid, err)
		} else {
			fmt.Printf("Wrote parquet: %s (%d frames)\n", parquetFile, frameCount)
		}
	} else {
		log.Printf("warning: no round data found for UUID %s, skipping parquet", uuid)
	}
}

// waitForSecondTGA polls the directory until both frame 0 and frame 1 are fully written,
// returning the UUID. Returns "" if the stop signal fires before two frames appear.
func (pe *pipeEncoder) waitForSecondTGA(expectedFileSize int64) string {
	var uuid string
	for {
		select {
		case <-pe.stopCh:
			return ""
		default:
		}
		if uuid == "" {
			entries, err := os.ReadDir(pe.dir)
			if err == nil {
				for _, e := range entries {
					if !e.IsDir() && strings.HasSuffix(e.Name(), ".tga") {
						// Filename: <uuid>_<framenum>.tga — UUID contains only hyphens, not underscores
						parts := strings.SplitN(e.Name(), "_", 2)
						if len(parts) == 2 {
							uuid = parts[0]
							break
						}
					}
				}
			}
		}
		if uuid != "" {
			frame0 := filepath.Join(pe.dir, fmt.Sprintf("%s_%08d.tga", uuid, 0))
			frame1 := filepath.Join(pe.dir, fmt.Sprintf("%s_%08d.tga", uuid, 1))
			info0, err0 := os.Stat(frame0)
			info1, err1 := os.Stat(frame1)
			if err0 == nil && err1 == nil && info0.Size() >= expectedFileSize && info1.Size() >= expectedFileSize {
				return uuid
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// streamFrames pipes raw TGA pixel data (header stripped at dataOffset) to ffmpeg's stdin
// starting at startFrame, deleting each file after piping. Uses a one-frame lag: pipes
// frame N only after frame N+1 is fully written (size == expectedFileSize), ensuring CS2
// has finished writing frame N before we read it. On stop signal, drains all remaining
// fully-written frames then returns.
// Returns the index of the next frame after the last one piped.
func (pe *pipeEncoder) streamFrames(uuid string, stdin io.Writer, startFrame int, dataOffset int, expectedFileSize int64) int {
	nextFrame := startFrame

	framePath := func(n int) string {
		return filepath.Join(pe.dir, fmt.Sprintf("%s_%08d.tga", uuid, n))
	}

	isFullyWritten := func(path string) bool {
		info, err := os.Stat(path)
		return err == nil && info.Size() >= expectedFileSize
	}

	pipeFrame := func(n int) {
		path := framePath(n)
		data, err := os.ReadFile(path)
		if err != nil {
			log.Printf("pipeEncoder: failed to read frame %d: %v", n, err)
			return
		}
		if _, err := stdin.Write(data[dataOffset:]); err != nil {
			log.Printf("pipeEncoder: failed to pipe frame %d to ffmpeg: %v", n, err)
			return
		}
		os.Remove(path)
	}

	for {
		select {
		case <-pe.stopCh:
			// Give CS2 a moment to finish writing the last frame, then drain all
			// remaining fully-written frames.
			time.Sleep(500 * time.Millisecond)
			for {
				if !isFullyWritten(framePath(nextFrame)) {
					break
				}
				pipeFrame(nextFrame)
				nextFrame++
			}
			return nextFrame
		default:
		}

		// Only pipe frame N after frame N+1 is fully written — proves frame N is complete.
		if isFullyWritten(framePath(nextFrame + 1)) {
			pipeFrame(nextFrame)
			nextFrame++
		} else {
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// EncodeVideosWithOutput starts the encoder manager watching watchDir, writing output to outputDir.
func EncodeVideosWithOutput(watchDir string, outputDir string, parquetDir string, framerate int, width int, height int, playerRounds []playerRoundInfo) *VideoEncoderManager {
	manager := NewVideoEncoderManager(watchDir, outputDir, parquetDir, framerate, width, height, playerRounds)
	if err := manager.Start(); err != nil {
		log.Printf("Error starting video encoder: %v", err)
		return nil
	}
	return manager
}
