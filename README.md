# RawDrop

<img src="resources/icon.png" width="96" alt="RawDrop app icon">

RawDrop is a local Ubuntu/Linux desktop app for turning RAW photos into
full-resolution JPEGs. Drop RAW files or a folder, review the thumbnails, and
export the camera's own embedded JPEGs into a new folder. Originals are only
ever read.

The MVP is built and validated end to end with Canon `.CR2` files. Many other
RAW extensions, including Sony `.ARW` / `.SRF` / `.SR2`, are recognized, but a
file is exported only when it contains a full-resolution embedded JPEG. See
[docs/FORMAT_SUPPORT.md](docs/FORMAT_SUPPORT.md) for the recognized,
exportable, and verified distinction: recognizing an extension is a routing
hint, not a camera-compatibility claim.

## What it does

- Accepts dragged files and folders, plus **Add photos** and **Add folder**
  pickers.
- Scans folders recursively, deduplicates overlapping drops, ignores
  non-RAW files inside folders, and excludes the output folder. The scanner
  and the native picker share one recognized-extension list.
- Shows an upright thumbnail grid with per-photo status.
- Exports the camera's full-resolution embedded JPEG without resizing or
  re-encoding it.
- Shows a truthful, determinate progress line while exporting: the percentage
  and count of photos processed, plus the active file and its stage. It does
  not fabricate a within-file percentage that ExifTool or `jpegtran` cannot
  report.
- Saves the chosen destination to the journal as soon as it is picked, so the
  choice survives a crash before **Export**. Once a photo is written, the
  destination is fixed for that batch and a new batch is required to change it.
- Corrects orientation with a lossless JPEG DCT transform when possible.
- Keeps location data out by default; preserving it is an explicit opt-in.
- Writes collision-safe filenames such as `IMG_3128 (2).jpg`; existing files
  are never overwritten.
- Saves a resumable journal, validates completed outputs by SHA-256, and
  supports pause, resume, cancel, and retry.
- Runs entirely on the machine. No accounts, uploads, telemetry, or cloud
  processing.

## Quality guarantee

RawDrop extracts the JPEG that the camera already embedded in the RAW file.
It does not render the RAW sensor data, resize the image, or encode the JPEG a
second time. This is what **no additional compression loss** means here.

**JPEG is not lossless.** The camera has already compressed the image. RawDrop
avoids adding another generation of JPEG compression, but it cannot recover
detail that the camera discarded.

Image pixels are changed only when orientation requires a physical rotation.
That uses `jpegtran`'s lossless DCT transform. When that transform cannot be
applied safely, RawDrop does not crop or re-encode; it keeps the orientation
metadata instead and says so.

**The original RAW file is never modified, renamed, moved, or deleted.**
RawDrop opens sources read-only. The acceptance test compares SHA-256 hashes
of every original before and after export.

RawDrop never silently substitutes a small preview. It compares the embedded
JPEG's dimensions with the camera's full image dimensions and requires at
least 90% of the usable displayed size. That tolerance accommodates minor
camera cropping while rejecting normal preview images. If no qualifying
embedded JPEG exists, the photo is skipped with a reason.

## Requirements

- Ubuntu desktop with GTK 3 (needed by MyGo's native UI; a normal Ubuntu
  desktop already has it). Wayland and X11 are supported by MyGo.
- Go 1.27.1. The commands below pin it with `GOTOOLCHAIN=go1.27.1`.
- ExifTool, required for reading metadata and extracting embedded JPEGs.
- `jpegtran` from libjpeg-turbo, needed for lossless physical rotation. The
  app flags it as missing at launch; exports can still run with a warning and
  metadata-only orientation, but rotated photos will not be physically
  corrected.
- A destination on a local filesystem that supports hard links, such as
  ext4, btrfs, XFS, or tmpfs. The final no-replace commit uses a hard link.
  FAT/exFAT and some network shares do not support this and fail safely.

The current development machine is Ubuntu 26.04.1 with GNOME on Wayland,
ExifTool 13.50, and libjpeg-turbo 2.1.5.

## Install dependencies on Ubuntu

```sh
sudo apt update
sudo apt install -y libimage-exiftool-perl libjpeg-turbo-progs libgtk-3-0
```

Install Go 1.27.1 from <https://go.dev/dl/> if it is not already available.
Check the tools:

```sh
exiftool -ver
jpegtran -version
GOTOOLCHAIN=go1.27.1 go version
GOTOOLCHAIN=go1.27.1 go tool mygo doctor
```

## Build, run, and test

Run the native app in development mode:

```sh
GOTOOLCHAIN=go1.27.1 go tool mygo dev
```

Build a production Linux app and Debian package:

```sh
GOTOOLCHAIN=go1.27.1 go tool mygo build -platform linux/amd64
```

The output is written under `build/linux-amd64/`, with the executable at
`build/linux-amd64/rawdrop` (the `out` directory is set in `mygo.json`).
Packages created by MyGo include the executable, a desktop entry, the icon, a
`.tar.gz` with an install script, and a `.deb`.

Run the automated tests and the MyGo UI checks:

```sh
GOTOOLCHAIN=go1.27.1 go test ./...
GOTOOLCHAIN=go1.27.1 go test -race ./...
GOTOOLCHAIN=go1.27.1 go tool mygo vet .
```

For the manual acceptance run against the 162-photo Canon sample, see
[docs/TESTING.md](docs/TESTING.md). For the internal design, see
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

### Command-line exporter

The repository also contains a headless exporter for scripting and quick
checks. It uses the same `internal/raw` core as the app:

```sh
GOTOOLCHAIN=go1.27.1 go run ./cmd/rawdrop-cli \
  -o /tmp/rawdrop-out \
  "/path/to/RAW photos"

# Keep GPS, or write orientation metadata without physical rotation:
GOTOOLCHAIN=go1.27.1 go run ./cmd/rawdrop-cli \
  --keep-gps --no-rotate -o /tmp/rawdrop-out photo.CR2
```

The CLI still refuses to overwrite an existing output and never modifies
sources. It does not use the desktop journal, so it has no resume behavior.

A short end-to-end check against copies of real photos is available:

```sh
scripts/acceptance.sh "/path/to/dashain photos" 8
```

It copies the selected photos to `/tmp/opencode/rawdrop-acceptance`, records
their hashes, exports them, and verifies that the originals are unchanged and
the outputs are upright full-size JPEGs. Read the script before running it;
its workspace is deleted and recreated on each run.

## How to use

1. Drop RAW files or a folder onto the window, or use **Choose photos** /
   **Choose folder**.
2. Check the photo grid and the skipped/failed counts.
3. Choose an output folder. With one source folder the suggested destination
   is a new `JPEG - Full Resolution` folder inside it. With mixed sources,
   RawDrop falls back to `~/Pictures/RawDrop Exports` (or a shared parent
   folder when one exists). The destination is saved as soon as you pick it
   and is created automatically if it does not exist yet. After the first
   photo is written, it is fixed for the batch; start a new batch to change it.
4. Leave **Correct orientation** on unless you have a reason not to. Leave
   **Keep location data** off unless you need GPS tags.
5. Select **Export**. Use **Pause**, **Resume**, and **Cancel** as needed.
6. When finished, use **Open folder** or double-click a tile to open an
   exported photo.

Exporting does not start merely because files were dropped; the batch is
prepared first and waits for **Export**.

## Troubleshooting

### `exiftool` was not found

Install it and check that it is on `PATH`:

```sh
sudo apt install libimage-exiftool-perl
exiftool -ver
```

RawDrop also honors `RAWDROP_EXIFTOOL=/path/to/exiftool` for a non-standard
install. Without ExifTool, RawDrop cannot inspect or extract RAW files.

### `jpegtran` is missing

Install libjpeg-turbo's tools:

```sh
sudo apt install libjpeg-turbo-progs
jpegtran -version
```

Without `jpegtran`, RawDrop cannot perform the lossless physical rotation.
It will not crop or re-encode to fake it. The app shows a missing-tool banner
at startup, and an affected export finishes with a warning while keeping the
EXIF orientation tag. In that case, rely on an orientation-aware viewer.

### A photo is skipped because it has no full-resolution embedded JPEG

Some cameras store only a small preview inside the RAW file. RawDrop refuses
to export that preview as if it were the full photo. Inspect the embedded
images with ExifTool:

```sh
# Read-only: the source is not changed.
exiftool -PreviewImage -b photo.CR2 > /tmp/embedded.jpg
file /tmp/embedded.jpg
```

Compare the JPEG dimensions with the camera's image dimensions
(`exiftool -ImageWidth -ImageHeight photo.CR2`). RawDrop requires the embedded
JPEG to reach at least 90% of the camera's usable displayed size. On the
verified Canon EOS 80D sample, the full-size image is exposed as
`PreviewImage` and is 6000x4000. Tag names vary by camera; RawDrop checks the
actual dimensions rather than trusting one tag name.

### A RAW file is recognized but cannot be exported

The scanner accepts many RAW extensions, but only Canon `.CR2` has been
validated end to end. Export requires a full-resolution embedded JPEG and a
supported metadata layout. A recognized extension is not a promise that every
camera model works. The file fails or is skipped with a message instead of
producing a low-quality result. See
[docs/FORMAT_SUPPORT.md](docs/FORMAT_SUPPORT.md) for which formats are
recognized, exportable, or verified.

### The destination is rejected or the commit fails

Choose a local folder on a filesystem that supports hard links. The hidden
`.rawdrop-*.jpg` temporary file is created in the destination so the final
commit can be atomic. If the app cannot hard-link there, it fails without
overwriting anything. A folder in your home directory on ext4/btrfs/XFS is
the safest choice.

### A previous export is offered at launch

That is the resumable journal. Choose **Export** to continue. Completed
outputs are validated by size and SHA-256; missing or modified outputs are
queued again. **New batch** clears the journal but never deletes exported
files or originals.

Only one RawDrop instance owns the journal at a time. Close the other
instance before resuming.

### Hidden temporary files remain after a crash

A hard kill can leave a `.rawdrop-*.jpg` file in the output folder. It is an
incomplete temporary file, never treated as an export. Once RawDrop is closed,
verify that no export is running and delete stale temporary files manually.
The app never deletes or overwrites unrelated files.

### The app does not start

Run `GOTOOLCHAIN=go1.27.1 go tool mygo doctor` and check that GTK 3 is
available. On a minimal Ubuntu installation, install `libgtk-3-0`. Also check
that you are running the app inside a graphical session, not a bare TTY.

## Privacy

RawDrop has no accounts, uploads, telemetry, or cloud services. It invokes
ExifTool and (when needed) `jpegtran` as local processes and reads/writes only
the paths you choose.

The local job journal stores source and output paths, file sizes, statuses,
settings, and SHA-256 hashes in MyGo's application-data directory. Thumbnails
are kept in memory and are not written to the journal. Location metadata is
not copied unless you enable **Keep location data**.

## Project layout

```text
rawdrop/
  main.go                  # entry point; creates the window and attaches the app
  main_test.go             # UI tests
  go.mod, go.sum           # Go 1.27.1, MyGo v0.6.2
  mygo.json                # MyGo app name, identifier, version, output directory
  resources/icon.png       # 1024x1024 app icon
  cmd/rawdrop-cli/         # headless exporter using the same processing core
  scripts/acceptance.sh    # copy-only end-to-end check on real photos
  docs/
    ARCHITECTURE.md        # processing flow, safe writes, recovery, threading
    FORMAT_SUPPORT.md      # recognized / exportable / verified camera matrix
    TESTING.md             # Ubuntu manual acceptance checklist
  internal/
    app/                   # native UI, export pool, orchestration, snapshots
    exiftool/              # shared helper discovery and bounded execution
    exporter/              # one-photo transaction shared by app and CLI
    fsutil/                # no-replace commit, unique names, sync helpers
    journal/               # versioned JSON journal and single-instance lock
    model/                 # job, item, settings, status and RAW extension types
    raw/                   # tag interpretation, extraction, verification, rotation
    scan/                  # recursive traversal, dedupe, output naming
    thumb/                 # bounded, orientation-aware grid thumbnails
  IMPLEMENTATION_PLAN.md   # original design and milestones
```

The processing packages do not depend on MyGo. Only `internal/app` imports the
UI framework, which keeps the core testable without a window.

## Current limitations

- **Camera coverage:** Canon `.CR2` is the validated path. Many other RAW
  extensions (including Sony `.ARW` / `.SRF` / `.SR2`) are recognized but not
  yet verified against a real sample; export still requires a full-resolution
  embedded JPEG. See [docs/FORMAT_SUPPORT.md](docs/FORMAT_SUPPORT.md) for the
  per-format status.
- **Output format:** JPEG only. There is no RAW rendering, resizing, JPEG
  re-encoding, or PNG/TIFF/WebP output.
- **Orientation:** physical correction depends on `jpegtran` and on the JPEG's
  block dimensions allowing a crop-free `-perfect` transform. Otherwise the
  orientation tag is retained and no crop is applied.
- **Platform:** Ubuntu/Linux only for the MVP. Windows and macOS builds are
  not packaged or validated.
- **Batch scope:** one active job/journal at a time; no folder watching or
  automatic export.
- **Location data:** GPS is off by default. Enabling it intentionally copies
  location metadata into the exported JPEG.

## Third-party tools

RawDrop invokes ExifTool and, when needed, `jpegtran`. It does not vendor or
redistribute either tool. Install them through Ubuntu's package manager. Their
licenses and notices apply to those tools.
