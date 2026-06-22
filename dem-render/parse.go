package main

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	demoinfocs "github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/common"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/events"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/msg"
)

// teamUint converts a demoinfocs team value to 0 (T) or 1 (CT).
func teamUint(t common.Team) uint32 {
	if t == common.TeamCounterTerrorists {
		return 1
	}
	return 0
}

// normalizeMapName converts a CS2 map name (e.g. "de_dust2") to the short form ("dust2").
func normalizeMapName(raw string) string {
	name := strings.ToLower(raw)
	// Strip common prefixes used by CS2 (de_, cs_, ar_, etc.)
	for _, prefix := range []string{"de_", "cs_", "ar_", "gg_", "dz_"} {
		if strings.HasPrefix(name, prefix) {
			name = name[len(prefix):]
			break
		}
	}
	return name
}

// collectRoundIntervals is the first demo parse pass.
// It returns player round intervals and the total match duration.
func collectRoundIntervals(demoPath string, matchName string, framerate int) ([]playerRoundInfo, time.Duration, error) {
	var matchDuration time.Duration
	var currentRound int
	var mapName string
	playerSpawnData := make(map[uint64]int)
	playerTeamData := make(map[uint64]uint32)
	playerRoundData := make([]playerRoundInfo, 0, 30*20+10)

	err := demoinfocs.ParseFile(demoPath, func(p demoinfocs.Parser) error {
		p.RegisterNetMessageHandler(func(m *msg.CDemoFileHeader) {
			if mapName == "" {
				mapName = normalizeMapName(m.GetMapName())
			}
		})

		p.RegisterEventHandler(func(e events.RoundFreezetimeChanged) {
			// RoundFreezetimeChanged fires on BOTH edges of the freezetime period —
			// once when freezetime begins (NewIsFreezetime=true) and again when it
			// ends (NewIsFreezetime=false). Counting every fire double-counts rounds,
			// producing round numbers ~2x the true count, so we act on a single edge.
			//
			// We use the freeze-END edge: SpawnTick is then the tick players are released
			// and can move, so intervals exclude the buy/freeze period (matching the
			// cs2-10k pipeline) and start well past the action-script warmup ticks. Using
			// the freeze-START edge instead would begin recording during the buy period
			// (muted freeze-time coloring) at near-zero round-1 ticks. No kills are counted
			// during freezetime, so the slightly later round increment is harmless.
			if e.NewIsFreezetime {
				return
			}
			currentRound++
			for _, player := range p.GameState().Participants().Playing() {
				if player != nil {
					playerSpawnData[player.SteamID64] = p.GameState().IngameTick()
					playerTeamData[player.SteamID64] = teamUint(player.Team)
				}
			}
		})

		p.RegisterEventHandler(func(e events.RoundEnd) {
			matchDuration = p.CurrentTime()

			currentTick := p.GameState().IngameTick()
			for steamId, spawnTick := range playerSpawnData {
				if currentTick == spawnTick {
					continue
				}

				var playerName string
				var userId int
				var team uint32
				for _, player := range p.GameState().Participants().Playing() {
					if player != nil && player.SteamID64 == steamId {
						playerName = player.Name
						team = teamUint(player.Team)
						if player.UserID <= math.MaxUint16 {
							userId = player.UserID & 0xff
						}
						break
					}
				}

				aliveTime := float64(currentTick-spawnTick) * p.TickTime().Seconds()
				expectedFrames := int(math.Ceil(aliveTime * float64(framerate)))
				uuidStr := uuid.New().String()

				playerRoundData = append(playerRoundData, playerRoundInfo{
					UUID:           uuidStr,
					SteamId:        steamId,
					UserId:         userId,
					Name:           playerName,
					MatchName:      matchName,
					MapName:        mapName,
					RoundNumber:    currentRound,
					Team:           team,
					SpawnTick:      spawnTick,
					DeathTick:      currentTick,
					Duration:       aliveTime,
					ExpectedFrames: expectedFrames,
					VideoFile:      uuidStr + ".mp4",
				})
			}
			playerSpawnData = make(map[uint64]int)
			playerTeamData = make(map[uint64]uint32)
		})

		p.RegisterEventHandler(func(e events.Kill) {
			if !p.GameState().IsMatchStarted() || p.GameState().IsFreezetimePeriod() {
				return
			}
			if e.Victim == nil {
				return
			}
			steamId := e.Victim.SteamID64
			spawnTick, exists := playerSpawnData[steamId]
			if !exists {
				return
			}

			deathTick := p.GameState().IngameTick()
			aliveTime := float64(deathTick-spawnTick) * p.TickTime().Seconds()
			expectedFrames := int(math.Ceil(aliveTime * float64(framerate)))

			var userId int
			if e.Victim.UserID <= math.MaxUint16 {
				userId = e.Victim.UserID & 0xff
			}

			uuidStr := uuid.New().String()
			playerRoundData = append(playerRoundData, playerRoundInfo{
				UUID:           uuidStr,
				SteamId:        steamId,
				UserId:         userId,
				Name:           e.Victim.Name,
				MatchName:      matchName,
				MapName:        mapName,
				RoundNumber:    currentRound,
				Team:           teamUint(e.Victim.Team),
				SpawnTick:      spawnTick,
				DeathTick:      deathTick,
				Duration:       aliveTime,
				ExpectedFrames: expectedFrames,
				VideoFile:      uuidStr + ".mp4",
			})
			delete(playerSpawnData, steamId)
			delete(playerTeamData, steamId)
		})

		return nil
	})
	if err != nil {
		return nil, 0, fmt.Errorf("parse demo: %w", err)
	}

	return playerRoundData, matchDuration, nil
}

// collectFrameData is the second demo parse pass.
// It fills in per-frame button/position data for each interval in-place.
func collectFrameData(demoPath string, playerRoundData []playerRoundInfo, framerate int) error {
	err := demoinfocs.ParseFile(demoPath, func(p demoinfocs.Parser) error {
		// PlayerJump fires on the exact tick the jump key is pressed.
		// We mark that frame J here so FrameDone can preserve it even when
		// Z-delta is still zero on the launch tick.
		p.RegisterEventHandler(func(e events.PlayerJump) {
			if e.Player == nil {
				return
			}
			currentTick := p.GameState().IngameTick()
			steamId := e.Player.SteamID64

			for i := range playerRoundData {
				interval := &playerRoundData[i]
				if interval.SteamId != steamId || currentTick < interval.SpawnTick || currentTick > interval.DeathTick {
					continue
				}
				ticksElapsed := currentTick - interval.SpawnTick
				elapsedTime := float64(ticksElapsed) * p.TickTime().Seconds()
				frameIndex := int(math.Floor(elapsedTime * float64(framerate)))

				if frameIndex >= 0 && frameIndex < interval.ExpectedFrames {
					actions := interval.FrameData[frameIndex].Actions
					if actions == "-" {
						interval.FrameData[frameIndex].Actions = "J"
					} else if !containsChar(actions, 'J') {
						interval.FrameData[frameIndex].Actions = actions + "J"
					}
				}
				break
			}
		})

		p.RegisterEventHandler(func(e events.FrameDone) {
			currentTick := p.GameState().IngameTick()

			for i := range playerRoundData {
				interval := &playerRoundData[i]
				if currentTick < interval.SpawnTick || currentTick > interval.DeathTick {
					continue
				}

				ticksElapsed := currentTick - interval.SpawnTick
				elapsedTime := float64(ticksElapsed) * p.TickTime().Seconds()
				frameIndex := int(math.Floor(elapsedTime * float64(framerate)))

				if frameIndex < 0 || frameIndex >= interval.ExpectedFrames {
					continue
				}

				for _, player := range p.GameState().Participants().Playing() {
					if player == nil || player.SteamID64 != interval.SteamId {
						continue
					}
					if player.IsScoped() {
						interval.WasScoped = true
					}
					data := getFrameData(player)

					if frameIndex > 0 {
						prev := interval.FrameData[frameIndex-1]
						data.MouseXDelta = data.RotationYaw - prev.RotationYaw
						data.MouseYDelta = data.RotationPitch - prev.RotationPitch

						if data.MouseXDelta > 180 {
							data.MouseXDelta -= 360
						} else if data.MouseXDelta < -180 {
							data.MouseXDelta += 360
						}
					}

					// Determine airborne state. PlayerJump marks the launch frame (J) even
					// when Z-delta is still zero. For subsequent airborne frames we use
					// Z-delta to distinguish jumping (ascending → J) from falling (descending → V).
					// This avoids the false positives that a plain Z-delta check produced on
					// stairs and ramps, where the player is never actually airborne.
					if containsChar(interval.FrameData[frameIndex].Actions, 'J') {
						// Launch frame already tagged by PlayerJump event — preserve it.
						if data.Actions == "-" {
							data.Actions = "J"
						} else if !containsChar(data.Actions, 'J') {
							data.Actions += "J"
						}
					} else if player.IsAirborne() {
						ascending := frameIndex > 0 && data.PositionZ > interval.FrameData[frameIndex-1].PositionZ
						if ascending {
							if data.Actions == "-" {
								data.Actions = "J"
							} else if !containsChar(data.Actions, 'J') {
								data.Actions += "J"
							}
						} else {
							if data.Actions == "-" {
								data.Actions = "V"
							} else if !containsChar(data.Actions, 'V') {
								data.Actions += "V"
							}
						}
					}

					interval.FrameData[frameIndex] = data
					break
				}
			}
		})

		return nil
	})
	return err
}
