#!/usr/bin/env bash
# Acceptance test for RawDrop's processing core against real RAW files.
#
# It copies a sample of RAW photos into a scratch directory (originals are
# never touched), records their hashes, exports them with the same core the
# app uses, and then checks with ExifTool that:
#   * every original is byte-identical afterwards,
#   * every output is a full-resolution JPEG (no downscaled preview),
#   * sideways photos end up upright with Orientation 1 (when corrected),
#   * ExifTool can read each output back.
#
# Usage:
#   scripts/acceptance.sh [SAMPLE_DIR] [COUNT]
# Defaults: the Canon test folder and 8 photos.
#
# Everything happens under /tmp/opencode/rawdrop-acceptance.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SAMPLE_DIR="${1:-/home/kshitij/Pictures/dashain photos}"
COUNT="${2:-8}"
WORK="${RAWDROP_ACCEPTANCE_DIR:-/tmp/opencode/rawdrop-acceptance}"
GO="${GOTOOLCHAIN:-go1.27.1}"

if [[ ! -d "$SAMPLE_DIR" ]]; then
  echo "sample folder not found: $SAMPLE_DIR" >&2
  exit 2
fi

rm -rf "$WORK"
mkdir -p "$WORK/in" "$WORK/out"

# Pick a mix: sideways photos (orientation 6/8, which must come out portrait)
# and already-upright ones (orientation 1, often legitimately landscape).
mapfile -t SIDEWAYS < <(exiftool -q -p '$Orientation#|$Directory/$FileName' "$SAMPLE_DIR"/*.CR2 2>/dev/null | awk -F'|' '$1==6 || $1==8 {print $2}' | head -n $(( (COUNT + 1) / 2 )))
mapfile -t UPRIGHT < <(exiftool -q -p '$Orientation#|$Directory/$FileName' "$SAMPLE_DIR"/*.CR2 2>/dev/null | awk -F'|' '$1==1 {print $2}' | head -n $(( COUNT / 2 )))
FILES=("${SIDEWAYS[@]}" "${UPRIGHT[@]}")
if [[ ${#FILES[@]} -eq 0 ]]; then
  echo "no sample CR2 files found in $SAMPLE_DIR" >&2
  exit 2
fi

for f in "${FILES[@]}"; do
  cp -- "$f" "$WORK/in/"
done

echo "== sample =="
echo "copied ${#FILES[@]} file(s) to $WORK/in"

# Record original hashes and sizes.
(cd "$WORK/in" && sha256sum ./*.CR2 | sort -k2 > "$WORK/original.sha256")

echo
echo "== export (same core as the app) =="
cd "$ROOT"
GOTOOLCHAIN="$GO" go run ./cmd/rawdrop-cli -o "$WORK/out" "$WORK/in"

echo
echo "== originals untouched =="
(cd "$WORK/in" && sha256sum -c "$WORK/original.sha256" --quiet)
echo "OK: all originals byte-identical"

echo
echo "== output checks =="
fail=0
portrait=0
landscape=0
shopt -s nullglob
OUT=("$WORK/out"/*.jpg)
if [[ ${#OUT[@]} -eq 0 ]]; then
  echo "FAIL: no JPEG outputs" >&2
  exit 1
fi
for jpg in "${OUT[@]}"; do
  # -f prints "-" for missing tags; an absent Orientation means upright.
  out=$(exiftool -m -q -f -p '$ImageWidth $ImageHeight $Orientation#' "$jpg" 2>/dev/null || true)
  read -r w h orient <<<"$out" || true
  if [[ -z "${w:-}" || -z "${h:-}" || "${w:-}" == "-" || "${h:-}" == "-" ]]; then
    echo "FAIL: could not read dimensions of $jpg (exiftool said: ${out:-nothing})" >&2
    fail=1
    continue
  fi
  if [[ "${orient:-}" == "-" || -z "${orient:-}" ]]; then
    orient=1
  fi
  if (( w < 4000 || h < 3000 )); then
    echo "FAIL: $jpg is only ${w}x${h} (preview-sized?)" >&2
    fail=1
  fi
  if [[ "$orient" != "1" ]]; then
    echo "FAIL: $jpg still has Orientation $orient" >&2
    fail=1
  fi
  if (( h > w )); then portrait=$((portrait + 1)); else landscape=$((landscape + 1)); fi
done
echo "checked ${#OUT[@]} output(s): full resolution, Orientation 1 (${portrait} portrait, ${landscape} landscape)"
if (( ${#SIDEWAYS[@]} > 0 && portrait == 0 )); then
  echo "FAIL: ${#SIDEWAYS[@]} sideways source photo(s) were not turned upright" >&2
  fail=1
fi

echo
echo "== source metadata spot check =="
exiftool -m -q -f -p 'SourceOrientation=$Orientation#' "$WORK/in"/*.CR2 | sort -u
exiftool -m -q -f -p 'OutputOrientation=$Orientation#' "$WORK/out"/*.jpg | sort -u

if (( fail )); then
  echo
  echo "ACCEPTANCE FAILED" >&2
  exit 1
fi
echo
echo "ACCEPTANCE PASSED — see $WORK/out"
