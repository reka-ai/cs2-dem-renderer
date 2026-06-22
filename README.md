<img width="1278" height="640" alt="CS Github" src="https://github.com/user-attachments/assets/b27c9914-01da-48c2-8d86-e46b569896a0" />

[[Blogpost]](https://reka.ai/news/cs2-10k-a-large-scale-egocentric-counter-strike-2-dataset) [[🤗 Dataset]](https://huggingface.co/datasets/RekaAI/CS2-10k) [[🎮 Viewer]](https://huggingface.co/spaces/RekaAI/CS2-10k-viewer)

Renders Counter-Strike 2 `.dem` demo files to per-player-round videos with synchronized frame-level metadata (keyboard inputs, mouse deltas, player position). Includes a browser-based viewer for exploring the output.

Built for Linux but can work on Windows with minimal modifications.

## How it works

1. **Parse** (two-pass): extracts per-player spawn/death tick intervals and per-frame button inputs from the demo.
2. **Render**: generates a JSON action sequence, launches CS2 via Steam with the demo loaded, and lets the server plugin execute the action sequence.
3. **Encode**: streams raw TGA frames piped from CS2's movie output directory to ffmpeg (VAAPI HEVC), writing `.mp4` files in real time.
4. **Parquet**: writes per-round metadata alongside each video.

## Requirements

- Go 1.21+
- CMake 3.14+
- C++ compiler
- ffmpeg with VAAPI support (`hevc_vaapi`)
- Counter-Strike 2 installed via Steam

## Building

### 1. Build the CS2 server plugin

Counter-Strike 2 updates break compatibility with the server plugin with the main branch built for version 1.41.6.5. See the branches for other game versions.

```bash
cmake ./cs2-server-plugin/ -B plugin-build -DCMAKE_BUILD_TYPE=Release
cmake --build plugin-build
```

Install the plugin:

```bash
./install-plugin.sh plugin-build/libserver.so
```

### 2. Build the renderer

```bash
cd dem-render && go build -o dem-render .
```

## Usage

Steam must be running before invoking the renderer. Run `dem-render --help` for addional arguments. Arguments must come before the demo path in single-demo mode.

### Single demo file

```bash
dem-render [flags] <path-to-demo.dem>
```

Produces one `<uuid>.mp4` + `<uuid>.parquet` pair per player round. By default they land next to the `dem-render` binary; use `--output` to choose a directory.

### Worker mode — batch directory

Process a directory of `.dem` files. Already-processed demos are skipped (a `.log` file marks completion).

```bash
dem-render worker --input /path/to/demos --output /path/to/output
```

Or via environment variables:

```bash
CS_INPUT_DIR=/path/to/demos CS_OUTPUT_DIR=/path/to/output dem-render worker
```

Each demo produces:
- `<uuid>.mp4` + `<uuid>.parquet` per player round in the output directory
- `<demoname>.log` — success/error log for that demo

## Output format

Each `.parquet` file contains one row with these columns:

| Column | Type | Description |
|---|---|---|
| `video_filename` | str | Matching `.mp4` filename |
| `match_name` | str | Demo filename stem |
| `map` | str | Map name (e.g. `dust2`) |
| `steam_id` | uint64 | Player Steam ID |
| `round_number` | int | Round number |
| `team` | uint | 0 = T, 1 = CT |
| `num_frames` | int | Frame count |
| `fps` | float | Framerate (default 48) |
| `width`, `height` | int | Resolution (default 1280×720) |
| `total_time` | float | Clip duration in seconds |
| `frame_data` | list | Per-frame records (see below) |

Each frame records: `actions`, `mouse_x_delta`, `mouse_y_delta`, `position_x`, `position_y`, `position_z`, `rotation_yaw`, `rotation_pitch`.

`actions` are a string of characters: `W` `A` `S` `D` `J` (jump) `V` (fall) `C` (crouch) `R` (walk) `[` (left-click) or `-` for no action.

## Viewer

<img width="1265" height="655" alt="viewer_preview" src="https://github.com/user-attachments/assets/3362e00b-c1e2-4e29-a7a1-ea8e125b4d2d" />

Open `viewer/index.html` directly in your browser (requires the File System Access API).

Click **⚙** to select a local or remote dataset source.

**Features:**
- Match browser with map filter
- Synchronized video + action playback
- Mouse delta chart (yaw / pitch)
- XY position minimap

## Credits

- [demoinfocs-golang](https://github.com/markus-wa/demoinfocs-golang) by Markus Walther — CS2/CSGO demo parser library used for all demo parsing
- [CS Demo Manager](https://github.com/akiver/cs-demo-manager) by akiver — reference for server plugin implementation
- [hl2sdk](https://github.com/alliedmodders/hl2sdk/) by AlliedModders — Source 2 SDK headers used by the server plugin
- [nlohmann/json](https://github.com/nlohmann/json) by Niels Lohmann — JSON library used by the server plugin to parse the action sequence
