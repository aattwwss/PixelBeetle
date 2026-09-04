#!/usr/bin/env bash
# art.sh — spawn bots that paint random art on the PixelBeetle canvas via the
# normal claim→confirm two-phase path.
#
# Each bot picks a random template from scripts/art-templates/ (hearts,
# invaders, mushrooms, ...), recolors it with a random palette (each role
# char gets a distinct random color), and paints it at a random on-canvas
# offset. Blueprints are written to data/art/ so you can re-run or -inspect.
#
# If the template dir is empty, falls back to procedural random noise sprites
# (SIZE controls their edge length; templates are fixed 16x16).
#
# Usage:
#   scripts/art.sh [BOTS] [SIZE]
#     BOTS  number of parallel bots / images (default 3)
#     SIZE  target image edge (default 16). Templates (16px) are upscaled
#           1x-4x → 16/32/48/64. The procedural fallback uses SIZE directly.
#
# Env knobs:
#   TARGET=http://localhost:8080   game server base URL
#   GRID=256x256                   must match the server's grid
#   RPS=40                         per-bot claims/sec cap (0 = unbounded)
#   ORDER=random|scanline          paint order (default random)
#   KEEP_COLORS=1                  use each template's authored palette
#   TEMPLATE_DIR=...               where templates live
#   PREVIEW=1                      ASCII-preview each image before painting
#
# scripts/art.sh --gen-only [BOTS]  just generate blueprints, no server/bots
#
# Examples:
#   scripts/art.sh                 # 3 bots, random templates
#   scripts/art.sh 5               # 5 bots
#   KEEP_COLORS=1 PREVIEW=1 scripts/art.sh 1
set -euo pipefail
cd "$(dirname "$0")/.."

GEN_ONLY=0
if [[ "${1:-}" == "--gen-only" ]]; then GEN_ONLY=1; shift; fi

BOTS="${1:-3}"
SIZE="${2:-16}"
TARGET="${TARGET:-http://localhost:8080}"
GRID="${GRID:-256x256}"
RPS="${RPS:-40}"
ORDER="${ORDER:-random}"
TEMPLATE_DIR="${TEMPLATE_DIR:-scripts/art-templates}"
KEEP_COLORS="${KEEP_COLORS:-0}"

if [[ "$GEN_ONLY" == 0 && "$BOTS" -gt 64 ]]; then
  echo "warning: BOTS=$BOTS spawns $BOTS concurrent bot processes and takes $(( BOTS / 3 / 60 ))+ min just to stagger — to fill the canvas, prefer fewer, bigger images (e.g. BOTS=8 SIZE=64)" >&2
fi

case "$GRID" in
  *x*) GW=${GRID%x*}; GH=${GRID#*x} ;;
  *) echo "bad GRID '$GRID', want WxH like 256x256" >&2; exit 1 ;;
esac
(( SIZE >= 6 && SIZE <= 64 )) || { echo "SIZE must be 6..64" >&2; exit 1; }

# Template upscale factor: SIZE/16 clamped to 1..4 (SIZE 32 → 2x → 32px art).
TPL_SCALE=$(( SIZE / 16 ))
(( TPL_SCALE < 1 )) && TPL_SCALE=1
(( TPL_SCALE > 4 )) && TPL_SCALE=4

if [[ "$GEN_ONLY" == 0 ]]; then
  if ! curl -sf "$TARGET/healthz" >/dev/null; then
    echo "no server at $TARGET — start it first (see scripts/server.sh or your own go run)" >&2
    exit 1
  fi
  echo "building bot..."
  go build -o bin/bot ./cmd/bot
fi

mkdir -p data/art data/logs

# Template inventory (may be empty → procedural fallback below).
shopt -s nullglob
tpls=("$TEMPLATE_DIR"/*.txt)
shopt -u nullglob
if (( ${#tpls[@]} == 0 )); then
  echo "no templates in $TEMPLATE_DIR — falling back to random noise sprites"
fi

PALETTE=("#ffffff" "#e4e4e4" "#888888" "#222222" "#ffb470" "#9a6324" \
         "#800000" "#ba2d2d" "#ffd600" "#808000" "#469990" "#42d4f4" \
         "#4363d8" "#000075" "#f032e6" "#fabed4")

# gen_sprite SIZE FILE — procedural fallback: random horizontally-mirrored
# noise ("space invader" gestalt), chars o/x mapped to 2 random colors.
gen_sprite() {
  local size=$1 file=$2
  local half=$(( (size + 1) / 2 ))
  local body="${PALETTE[RANDOM % ${#PALETTE[@]}]}"
  local accent="${PALETTE[RANDOM % ${#PALETTE[@]}]}"
  {
    echo "legend: o=$body x=$accent"
    for (( r = 0; r < size; r++ )); do
      local cell=() row="" c m
      for (( c = 0; c < half; c++ )); do
        if (( RANDOM % 100 < 42 )); then
          if (( RANDOM % 100 < 15 )); then cell[$c]=x; else cell[$c]=o; fi
        else
          cell[$c]=.
        fi
      done
      for (( c = 0; c < size; c++ )); do
        m=$(( c < size / 2 ? c : size - 1 - c ))
        row+=${cell[$m]}
      done
      echo "$row"
    done
  } > "$file"
}

# gen_art FILE — copy a random template, recoloring each role char with a
# random distinct palette color (the template's legend defines the roles).
gen_art() {
  local out=$1
  local tpl="${tpls[RANDOM % ${#tpls[@]}]}"
  if [[ "$KEEP_COLORS" == 1 ]]; then
    cp "$tpl" "$out"
    return
  fi
  local chars=() tok
  if [[ "$(head -1 "$tpl")" != legend:* ]]; then
    echo "template '$tpl' must start with a 'legend:' line" >&2
    exit 1
  fi
  while read -r tok; do chars+=("${tok%%=*}"); done \
    < <(head -1 "$tpl" | sed 's/^legend://' | tr ' ' '\n' | grep -v '^$')
  # a legend color must exist per char — the palette has 16 entries
  if (( ${#chars[@]} > ${#PALETTE[@]} )); then
    echo "template '$tpl' has ${#chars[@]} legend chars; max ${#PALETTE[@]}" >&2
    exit 1
  fi
  local idxs
  mapfile -t idxs < <(seq 0 $((${#PALETTE[@]} - 1)) | shuf)
  local newlegend="legend:" j=0
  for c in "${chars[@]}"; do
    newlegend+=" $c=${PALETTE[${idxs[$j]}]}"
    j=$((j + 1))
  done
  # legend + body, upscaled by TPL_SCALE via nearest-neighbor (each cell and
  # row repeated TPL_SCALE times) so SIZE actually controls template size.
  { echo "$newlegend"; tail -n +2 "$tpl"; } | \
    awk -v n="$TPL_SCALE" 'NR==1 { print; next }
      { grown=""; for (i=1; i<=length($0); i++) { c=substr($0,i,1); for (k=0;k<n;k++) grown=grown c }
        for (k=0;k<n;k++) print grown }' > "$out"
}

pids=()
# Kill children on interrupt — must be armed BEFORE the launch loop, or a
# crash/Ctrl-C mid-loop orphans every bot already spawned.
trap 'echo "stopping bots..."; kill "${pids[@]}" 2>/dev/null || true; exit 130' INT TERM

for (( i = 1; i <= BOTS; i++ )); do
  f="data/art/img-$i-$(date +%s%N).txt"
  if (( ${#tpls[@]} > 0 )); then gen_art "$f"; else gen_sprite "$SIZE" "$f"; fi

  # random top-left offset that keeps the whole image on-canvas
  local_w=$(( 16 * TPL_SCALE )); [[ ${tpls[*]} ]] || local_w=$SIZE
  if (( local_w > GW || local_w > GH )); then
    echo "image ${local_w}px does not fit grid $GRID — lower SIZE" >&2
    exit 1
  fi
  ox=$(( RANDOM % (GW - local_w + 1) ))
  oy=$(( RANDOM % (GH - local_w + 1) ))

  echo "bot $i: $f (${local_w}px) at ${ox},${oy}"
  if [[ "$GEN_ONLY" == 1 ]]; then continue; fi
  if [[ "${PREVIEW:-0}" == 1 ]]; then
    ./bin/bot -grid "$GRID" -paint "$f" -paint-offset "$ox,$oy" -inspect | sed 's/^/  /'
  fi

  ./bin/bot -target "$TARGET" -grid "$GRID" \
    -paint "$f" -paint-offset "$ox,$oy" \
    -paint-order "$ORDER" -rps "$RPS" \
    -metrics-addr "" \
    > "data/logs/art-$i.log" 2>&1 &
  pids+=($!)
  sleep 0.4   # stagger starts so the bots don't stampede the same second
done

if [[ "$GEN_ONLY" == 1 ]]; then echo "generated $BOTS blueprint(s) in data/art/"; exit 0; fi

fail=0
for (( i = 0; i < BOTS; i++ )); do
  if wait "${pids[$i]}"; then
    echo "bot $((i+1)): done"
  else
    echo "bot $((i+1)): FAILED (see data/logs/art-$((i+1)).log)" >&2
    fail=1
  fi
done
exit $fail
