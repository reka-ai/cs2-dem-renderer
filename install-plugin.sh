#!/bin/bash

if [ $# -ne 1 ]; then
    echo "Usage: $0 <path-to-plugin.so>"
    exit 1
fi

PLUGIN_PATH="$1"

if [ ! -f "$PLUGIN_PATH" ]; then
    echo "Error: Plugin file not found: $PLUGIN_PATH"
    exit 1
fi

# Detect CS2 installation directory
CS2_DIR=""
for candidate in \
    "$HOME/.steam/steam/steamapps/common/Counter-Strike Global Offensive" \
    "$HOME/.local/share/Steam/steamapps/common/Counter-Strike Global Offensive"; do
    if [ -d "$candidate" ]; then
        CS2_DIR="$candidate"
        break
    fi
done

if [ -z "$CS2_DIR" ]; then
    echo "Error: Counter-Strike 2 directory not found"
    exit 1
fi

echo "Found CS2 at: $CS2_DIR"

GAMEINFO_FILE="$CS2_DIR/game/csgo/gameinfo.gi"
if [ ! -f "$GAMEINFO_FILE" ]; then
    echo "Error: gameinfo.gi not found: $GAMEINFO_FILE"
    exit 1
fi

# Copy plugin
PLUGIN_DIR="$CS2_DIR/game/csgo/dem-render/bin/linuxsteamrt64"
mkdir -p "$PLUGIN_DIR"
echo "Copying plugin to $PLUGIN_DIR/libserver.so"
cp "$PLUGIN_PATH" "$PLUGIN_DIR/libserver.so"

# Patch gameinfo.gi using Python for reliable structured text editing.
# The SearchPaths block in gameinfo.gi looks like:
#
#   SearchPaths
#   {
#       Game_LowViolence  csgo_lv
#       Game  csgo
#       ...
#   }
#
# We need to insert "Game  csgo/dem-render" as the first entry inside that
# block. A previous sed-based version of this script may have inserted the
# line between "SearchPaths" and "{" (outside the block) — this Python
# snippet removes any such misplaced entry and inserts it correctly.
python3 - "$GAMEINFO_FILE" << 'PYEOF'
import sys, re

path = sys.argv[1]
with open(path, 'r') as f:
    content = f.read()

entry_line = '\t\t\tGame\tcsgo/dem-render\n'

# Remove any misplaced entry that sits between "SearchPaths" and its opening "{"
content = re.sub(
    r'(SearchPaths[ \t]*\n)(\s*Game\s+csgo/dem-render\s*\n)',
    r'\1',
    content
)

# Insert as the first entry inside the SearchPaths { } block if not already present
if 'csgo/dem-render' not in content:
    content = re.sub(
        r'(SearchPaths[ \t]*\n[ \t]*\{[ \t]*\n)',
        r'\1' + entry_line,
        content
    )
    print("Added 'Game csgo/dem-render' to SearchPaths block in gameinfo.gi")
else:
    print("gameinfo.gi already contains 'csgo/dem-render' entry")

with open(path, 'w') as f:
    f.write(content)
PYEOF

echo "Plugin installation complete!"
