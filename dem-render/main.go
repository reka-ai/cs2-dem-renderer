package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	demoinfocs "github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/common"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/msg"
	"github.com/parquet-go/parquet-go"
)

type PlayerMovementState struct {
	Forward  bool
	Back     bool
	Left     bool
	Right    bool
	Walking  bool
	Jump     bool // airborne and ascending
	Fall     bool // airborne and descending
	Airborne bool
	Crouch   bool
	Attack1  bool // left mouse button (primary fire)
	Attack2  bool // right mouse button (secondary fire / ADS)
}

// Convert bitmask to PlayerMovementState
func movementStateFromActionString(stateString string) PlayerMovementState {
	result := PlayerMovementState{
		Forward: strings.Contains(stateString, "W"),
		Back:    strings.Contains(stateString, "A"),
		Left:    strings.Contains(stateString, "S"),
		Right:   strings.Contains(stateString, "D"),
		Jump:    strings.Contains(stateString, "J"),
		Fall:    strings.Contains(stateString, "V"),
		Crouch:  strings.Contains(stateString, "C"),
		Walking: strings.Contains(stateString, "R"),
		Attack1: strings.Contains(stateString, "["),
		Attack2: strings.Contains(stateString, "]"),
	}

	return result
}

// Helper function to check if a string contains a character
func containsChar(s string, c rune) bool {
	for _, ch := range s {
		if ch == c {
			return true
		}
	}
	return false
}

// Convert movement state bitmask to action string (e.g., "WADJC")
func movementStateToActionString(state PlayerMovementState) string {
	result := ""

	if state.Forward {
		result += "W"
	}
	if state.Back {
		result += "S"
	}
	if state.Left {
		result += "A"
	}
	if state.Right {
		result += "D"
	}
	if state.Jump {
		result += "J"
	}
	if state.Fall {
		result += "V"
	}
	if state.Crouch {
		result += "C"
	}
	if state.Walking {
		result += "R"
	}
	if state.Attack1 {
		result += "["
	}
	if state.Attack2 {
		result += "]"
	}

	if result == "" {
		return "-"
	}
	return result
}

// FrameDataRecord represents per-frame action data
type FrameDataRecord struct {
	Actions                 string  `parquet:"actions"`
	MouseXDelta             float32 `parquet:"mouse_x_delta"`
	MouseYDelta             float32 `parquet:"mouse_y_delta"`
	CameraExtrinsicsDefined bool    `parquet:"camera_extrinsics_defined"`
	PositionX               float32 `parquet:"position_x"`
	PositionY               float32 `parquet:"position_y"`
	PositionZ               float32 `parquet:"position_z"`
	RotationYaw             float32 `parquet:"rotation_yaw"`
	RotationPitch           float32 `parquet:"rotation_pitch"`
}

// ParquetRecord represents the schema for Action Data
type ParquetRecord struct {
	VideoFilename string            `parquet:"video_filename"`
	MatchName     string            `parquet:"match_name"`
	Map           string            `parquet:"map"`
	SteamID       uint64            `parquet:"steam_id"`
	RoundNumber   int32             `parquet:"round_number"`
	Team          uint32            `parquet:"team"`
	NumFrames     int32             `parquet:"num_frames"`
	FPS           float32           `parquet:"fps"`
	AspectRatio   float32           `parquet:"aspect_ratio"`
	Width         int32             `parquet:"width"`
	Height        int32             `parquet:"height"`
	TotalTime     float32           `parquet:"total_time"`
	Category      string            `parquet:"category"`
	FOV           float32           `parquet:"fov"`
	FrameData     []FrameDataRecord `parquet:"frame_data,list"`
}

type playerRoundInfo struct {
	UUID           string
	SteamId        uint64
	UserId         int
	Name           string
	MatchName      string
	MapName        string
	RoundNumber    int
	Team           uint32
	SpawnTick      int
	DeathTick      int
	Duration       float64
	ExpectedFrames int
	VideoFile      string
	FrameData      []FrameDataRecord
	WasScoped      bool
}

// writeParquetFile writes a single parquet file for a player round, using numFrames as the
// authoritative frame count. FrameData is aligned to the rendered video: leading entries
// that precede the actual recording start are dropped, then the remainder is truncated to
// numFrames. Without dropping the leading frames, early-spawn rounds (where startmovie is
// clamped past SpawnTick) would have annotations that lead the video by that offset.
func writeParquetFile(round playerRoundInfo, numFrames int, dir string, framerate, width, height int) error {
	frameData := round.FrameData
	if drop := leadingFramesDropped(round.SpawnTick, framerate); drop > 0 {
		if drop < len(frameData) {
			frameData = frameData[drop:]
		} else {
			frameData = nil
		}
	}
	if numFrames < len(frameData) {
		frameData = frameData[:numFrames]
	}

	record := ParquetRecord{
		VideoFilename: round.VideoFile,
		MatchName:     round.MatchName,
		Map:           round.MapName,
		SteamID:       round.SteamId,
		RoundNumber:   int32(round.RoundNumber),
		Team:          round.Team,
		NumFrames:     int32(numFrames),
		FPS:           float32(framerate),
		AspectRatio:   float32(width) / float32(height),
		Width:         int32(width),
		Height:        int32(height),
		// Derive duration from the rendered frame count so total_time always matches the
		// video and frame_data length. round.Duration is the full alive time, which can be
		// slightly longer than the recorded clip when startmovie is clamped past SpawnTick.
		TotalTime: float32(numFrames) / float32(framerate),
		Category:  "gaming",
		FOV:       90.0,
		FrameData: frameData,
	}

	filename := filepath.Join(dir, round.UUID+".parquet")
	file, err := os.Create(filename)
	if err != nil {
		return fmt.Errorf("failed to create file %s: %w", filename, err)
	}

	writer := parquet.NewGenericWriter[ParquetRecord](file)
	_, err = writer.Write([]ParquetRecord{record})
	if err != nil {
		file.Close()
		return fmt.Errorf("failed to write to %s: %w", filename, err)
	}

	err = writer.Close()
	if err != nil {
		file.Close()
		return fmt.Errorf("failed to close writer for %s: %w", filename, err)
	}

	return file.Close()
}

func getFrameData(player *common.Player) FrameDataRecord {
	data := FrameDataRecord{
		Actions:                 "-",
		CameraExtrinsicsDefined: true,
	}

	if player == nil {
		return data
	}

	// Get button mask from the pawn entity, not controller
	pawn := player.PlayerPawnEntity()
	if pawn == nil {
		return data
	}

	buttonValue, ok := pawn.PropertyValue("m_pMovementServices.m_nButtonDownMaskPrev")
	if !ok {
		return data
	}

	// Use UInt64() method to get the uint64 value
	buttonMask := buttonValue.UInt64()

	// CS2 button bit flags (from Source engine)
	const (
		IN_ATTACK    = 1 << 0  // [ left mouse button / primary fire (bit 0)
		IN_ATTACK2   = 1 << 1  // ] right mouse button / secondary fire / ADS (bit 1)
		IN_FORWARD   = 1 << 3  // W (bit 3)
		IN_BACK      = 1 << 4  // S (bit 4)
		IN_MOVELEFT  = 1 << 9  // A (bit 9)
		IN_MOVERIGHT = 1 << 10 // D (bit 10)
	)

	// Build movement state
	movementState := PlayerMovementState{
		Forward:  (buttonMask & IN_FORWARD) != 0,
		Back:     (buttonMask & IN_BACK) != 0,
		Left:     (buttonMask & IN_MOVELEFT) != 0,
		Right:    (buttonMask & IN_MOVERIGHT) != 0,
		Crouch:   player.IsDucking(),
		Airborne: player.IsAirborne(),
		Attack1:  (buttonMask & IN_ATTACK) != 0,
		Attack2:  (buttonMask & IN_ATTACK2) != 0,
	}

	// Check walking state
	walkValue, ok := pawn.PropertyValue("m_bIsWalking")
	if ok {
		movementState.Walking = walkValue.BoolVal()
	}

	// Convert movement state to action string
	data.Actions = movementStateToActionString(movementState)

	// Get player position using the library's Position() method
	pos := player.Position()
	data.PositionX = float32(pos.X)
	data.PositionY = float32(pos.Y)
	data.PositionZ = float32(pos.Z)

	// Get current rotation (yaw and pitch)
	data.RotationYaw = float32(player.ViewDirectionX())
	data.RotationPitch = float32(player.ViewDirectionY())

	return data
}

func validateCS2DemoFile(demoPath string) error {
	// Check if file exists
	fileInfo, err := os.Stat(demoPath)
	if err != nil {
		return fmt.Errorf("demo file not found: %w", err)
	}

	if fileInfo.IsDir() {
		return fmt.Errorf("path is a directory, not a file: %s", demoPath)
	}

	// Open and read the first few bytes to validate it's a demo file
	file, err := os.Open(demoPath)
	if err != nil {
		return fmt.Errorf("failed to open demo file: %w", err)
	}
	defer file.Close()

	// CS2 demo files start with "PBDEMS2" (8 bytes) or similar header
	header := make([]byte, 8)
	n, err := file.Read(header)
	if err != nil || n < 8 {
		return fmt.Errorf("failed to read demo file header: %w", err)
	}

	// Check for Source 2 demo header (PBDEMS2 or PBDEMS2\x00)
	headerStr := string(header[:7])
	if headerStr != "PBDEMS2" {
		return fmt.Errorf("invalid demo file: not a CS2 demo file (header: %q)", headerStr)
	}

	return nil
}

// checkCS2Version reads the CDemoFileHeader protobuf message from the demo and validates
// that it is a CS2 demo.
func checkCS2Version(demoPath string) error {
	f, err := os.Open(demoPath)
	if err != nil {
		return fmt.Errorf("failed to open demo: %w", err)
	}
	defer f.Close()

	p := demoinfocs.NewParser(f)
	defer p.Close()

	var (
		patchVersion   int32
		gameDirectory  string
		headerReceived bool
	)

	p.RegisterNetMessageHandler(func(m *msg.CDemoFileHeader) {
		patchVersion = m.GetPatchVersion()
		gameDirectory = m.GetGameDirectory()
		headerReceived = true
		p.Cancel()
	})

	if err := p.ParseToEnd(); err != nil && !errors.Is(err, demoinfocs.ErrCancelled) {
		return fmt.Errorf("failed to parse demo header: %w", err)
	}

	if !headerReceived {
		return fmt.Errorf("CDemoFileHeader not found in demo")
	}

	if filepath.Base(gameDirectory) != "csgo" {
		// 	return fmt.Errorf("unsupported game directory %q: expected CS2 demo (csgo)", gameDirectory)
	}

	fmt.Printf("CS2 version check passed: patch_version=%d, game_dir=%s\n",
		patchVersion, gameDirectory)

	return nil
}

const usageText = `dem-render — render CS2 .dem demo files to per-player-round videos + parquet metadata

Usage:
  dem-render [flags] <demo-file-path>     Render a single demo
  dem-render worker [flags]               Batch-render a directory of demos
  dem-render --help                       Show this help

Render flags (both modes):
  --width N        Output video width in pixels (default 1280)
  --height N       Output video height in pixels (default 720)
  --fps N          Output framerate (default 48)

Single-demo flags:
  --output DIR     Output directory for .mp4/.parquet files (default: the dem-render binary's directory)

Worker flags:
  --input DIR      Directory of .dem files to process (or CS_INPUT_DIR env)
  --output DIR     Directory for output videos/parquets (or CS_OUTPUT_DIR env)

Worker skips any demo that already has a <demoname>.log in the output directory.
`

func printUsage(w *os.File) {
	fmt.Fprint(w, usageText)
}

// addRenderFlags registers the render-configuration flags shared by single and worker
// modes on fs and returns a builder that produces a RenderConfig once fs has been parsed.
func addRenderFlags(fs *flag.FlagSet) func() RenderConfig {
	width := fs.Int("width", 1280, "Output video width in pixels")
	height := fs.Int("height", 720, "Output video height in pixels")
	fps := fs.Int("fps", 48, "Output framerate (frames per second)")
	return func() RenderConfig {
		return RenderConfig{
			Width:     *width,
			Height:    *height,
			Framerate: *fps,
		}
	}
}

func main() {
	if len(os.Args) < 2 {
		printUsage(os.Stderr)
		os.Exit(1)
	}

	switch os.Args[1] {
	case "-h", "--help", "help":
		printUsage(os.Stdout)
		return
	case "worker":
		runWorker(os.Args[2:])
		return
	}

	runSingle(os.Args[1:])
}

// runSingle renders a single demo file. Usage: dem-render [flags] <demo-file-path>
func runSingle(args []string) {
	fs := flag.NewFlagSet("dem-render", flag.ExitOnError)
	fs.Usage = func() { printUsage(os.Stderr) }
	output := fs.String("output", "", "Output directory for .mp4/.parquet files (default: the dem-render binary's directory)")
	buildConfig := addRenderFlags(fs)
	fs.Parse(args)

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "error: missing <demo-file-path>")
		printUsage(os.Stderr)
		os.Exit(1)
	}
	demoPath := fs.Arg(0)

	// Validate that this is a CS2 demo file
	if err := validateCS2DemoFile(demoPath); err != nil {
		log.Fatalf("Demo file validation failed: %v", err)
	}

	fmt.Println("Demo file validated successfully")

	// Check CS2 version from the demo header
	if err := checkCS2Version(demoPath); err != nil {
		log.Fatalf("CS2 version check failed: %v", err)
	}

	matchName := strings.TrimSuffix(filepath.Base(demoPath), filepath.Ext(demoPath))

	renderConfig := buildConfig()
	renderConfig.DemoPath = demoPath
	renderConfig.DemofileName = matchName
	renderConfig.OutputDir = *output

	playerRoundData, matchDuration, err := collectRoundIntervals(demoPath, matchName, renderConfig.Framerate)
	if err != nil {
		log.Panic("failed to parse demo: ", err)
	}

	// Pre-allocate FrameData arrays
	for i := range playerRoundData {
		playerRoundData[i].FrameData = make([]FrameDataRecord, playerRoundData[i].ExpectedFrames)
	}

	fmt.Println("Collecting button inputs and jumps...")
	if err := collectFrameData(demoPath, playerRoundData, renderConfig.Framerate); err != nil {
		log.Panic("failed to parse demo for inputs: ", err)
	}

	// Filter out intervals where the player was scoped at any point
	filtered := playerRoundData[:0]
	for _, interval := range playerRoundData {
		if !interval.WasScoped {
			filtered = append(filtered, interval)
		}
	}
	playerRoundData = filtered

	if matchDuration > 0 {
		hours := matchDuration.Hours()
		fmt.Printf("\nMatch duration: %.2f hours (%.0f minutes)\n", hours, matchDuration.Minutes())
	}

	var totalAliveTime float64 = 0.0
	for _, playerMatch := range playerRoundData {
		totalAliveTime += playerMatch.Duration
	}
	fmt.Printf("Total player alive time: (%.0f minutes)\n", totalAliveTime/60.0)

	fmt.Println("\nTotal time alive per player:\n")
	for _, playerMatch := range playerRoundData {
		fmt.Printf("%s - %d - %d - %s: spawn tick: %d, death tick: %d, alive time: (%.2f seconds), expected frames: %d\n",
			playerMatch.UUID, playerMatch.UserId, playerMatch.SteamId, playerMatch.Name, playerMatch.SpawnTick, playerMatch.DeathTick, playerMatch.Duration, playerMatch.ExpectedFrames)
	}

	renderConfig.PlayerRounds = playerRoundData

	if _, _, err := RenderIntervals(playerRoundData, renderConfig); err != nil {
		log.Fatalf("Render failed: %v", err)
	}
}
