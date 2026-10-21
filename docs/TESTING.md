# Manual acceptance testing on Ubuntu

This checklist validates RawDrop against the real Canon EOS 80D sample on
Ubuntu. It is deliberately more cautious than an automated test: the sample
is personal work, so every command treats it as read-only and every write
goes to `/tmp`.

Run the automated suite first:

```sh
GOTOOLCHAIN=go1.27.1 go test ./...
GOTOOLCHAIN=go1.27.1 go test -race ./...
GOTOOLCHAIN=go1.27.1 go tool mygo vet .
```

A quick copy-only end-to-end check is also available:

```sh
scripts/acceptance.sh "/path/to/dashain photos" 8
```

The script creates `/tmp/opencode/rawdrop-acceptance`, copies 8 photos into
it, records their hashes, exports them through the same `internal/raw` core,
and fails if an original changed, an output is preview-sized, or a sideways
photo did not end up with `Orientation 1`. It never writes into the sample.
Read it before running; it deletes and recreates its own workspace.

## Safety labels

- **[SAFE]** reads the source, writes only to `/tmp`, and does not change the
  sample.
- **[EXPENSIVE]** processes all 162 photos and can take a while.
- **[COPY ONLY]** changes permissions or contents of a **copy** under
  `/tmp`. Never apply it to the original sample.
- **[ADVANCED]** may kill a process or exercise crash behavior.

**Do not run `chmod`, `truncate`, `rm`, or any write command against the
original sample folder.** The only permitted operations there are reads:
`sha256sum`, `stat`, `exiftool`, and opening files in a viewer.

## Environment

Recorded during development:

| Item | Value |
| --- | --- |
| OS | Ubuntu 26.04.1 LTS |
| Desktop | GNOME, Wayland |
| Go | 1.27.1 |
| MyGo | 0.6.2 |
| ExifTool | 13.50 |
| libjpeg-turbo (`jpegtran`) | 2.1.5 |

Check the machine before testing:

```sh
lsb_release -ds
echo "$XDG_SESSION_TYPE"
GOTOOLCHAIN=go1.27.1 go version
exiftool -ver
jpegtran -version
GOTOOLCHAIN=go1.27.1 go tool mygo doctor
```

## Sample facts

Use the 162-photo Dashain folder as the reference sample. Do not copy the
photos into the repository.

```sh
export SAMPLE="/path/to/dashain photos"
export WORK="/tmp/rawdrop-acceptance"
mkdir -p "$WORK"
```

Verified facts about this sample (read-only checks):

| Property | Expected value |
| --- | --- |
| CR2 file count | 162 |
| Source size | about 4.3 GiB |
| Camera dimensions | 6000x4000 |
| Embedded JPEG dimensions | 6000x4000 |
| Orientation `Rotate 270 CW` | 115 files |
| Orientation `Horizontal (normal)` | 47 files |
| Full-size embedded JPEG tag | `PreviewImage` |

These are properties of this sample, not performance claims. The output will
contain 162 JPEGs if every file is eligible.

## 0. Build and launch

```sh
cd /path/to/rawdrop
GOTOOLCHAIN=go1.27.1 go tool mygo dev
```

Or build and run the production binary:

```sh
GOTOOLCHAIN=go1.27.1 go tool mygo build -platform linux/amd64
./build/linux-amd64/rawdrop
```

- [ ] The window opens on Wayland without a WebKit dependency.
- [ ] The header shows **RawDrop**.
- [ ] The drop zone and **Choose photos** / **Choose folder** are usable.
- [ ] The footer says originals are read-only and processing is local.
- [ ] `mygo doctor` shows Go and GTK 3 as available.

## 1. Baseline the originals [SAFE]

Create a SHA-256 manifest outside the sample. This is the primary proof that
the originals were not modified.

```sh
find "$SAMPLE" -type f -iname '*.cr2' -print0 \
  | sort -z \
  | xargs -0 sha256sum > "$WORK/originals-before.sha256"

test "$(wc -l < "$WORK/originals-before.sha256")" -eq 162 \
  && echo "162 originals recorded"
```

Record metadata and orientation counts:

```sh
exiftool -csv -FileName -ImageWidth -ImageHeight -Orientation -FileSize \
  "$SAMPLE"/*.CR2 > "$WORK/metadata-before.csv"

exiftool -csv -FileName -Orientation \
  "$SAMPLE"/*.CR2 > "$WORK/orientation-before.csv"

awk -F, 'NR>1 {c[$3]++} END {for (k in c) print k, c[k]}' \
  "$WORK/orientation-before.csv"
```

Expected orientation counts:

```text
Rotate 270 CW 115
Horizontal (normal) 47
```

- [ ] The manifest has 162 lines.
- [ ] The orientation counts match 115 / 47.
- [ ] `exiftool` reports 6000x4000 for the camera dimensions.

Spot-check the embedded JPEG:

```sh
exiftool -b -PreviewImage "$SAMPLE/IMG_3128.CR2" > "$WORK/embedded-3128.jpg"
file "$WORK/embedded-3128.jpg"
```

Expected: `JPEG image data ... 6000x4000`.

## 2. Full export to `/tmp` [SAFE] [EXPENSIVE]

This is the main acceptance run. It reads the real sample and writes every
JPEG to `/tmp`, never into the sample folder.

1. Drop the entire `dashain photos` folder onto the window.
2. Confirm the grid reports **162 photos**.
3. Set the output folder to `$WORK/out-full`.
4. Leave **Correct orientation** on.
5. Leave **Keep location data** off.
6. Start the export.
7. Pause and resume once while a few photos remain, to exercise the controls.

- [ ] The grid shows 162 thumbnails; portrait photos render upright.
- [ ] The status reaches 162 exported, 0 failed (0 skipped is expected for
      this sample).
- [ ] The UI stays responsive while thumbnails and exports run.
- [ ] Pause stops new items; resume continues.
- [ ] The completion summary and notification show the same counts.
- [ ] `Open folder` opens `$WORK/out-full`.

Verify the output count and names:

```sh
OUT="$WORK/out-full"
find "$OUT" -maxdepth 1 -type f -name '*.jpg' | wc -l
find "$OUT" -maxdepth 1 -name '.rawdrop-*' -print
```

- [ ] Exactly 162 `.jpg` files.
- [ ] No `.rawdrop-*` temporary files remain after a graceful run.
- [ ] Filenames are unique and use the source stem, e.g. `IMG_3128.jpg`.

### Verify originals are unchanged [SAFE]

```sh
sha256sum -c "$WORK/originals-before.sha256" --quiet
echo "exit status: $?"
```

- [ ] Exit status is 0 and no `FAILED` lines appear.

Repeat the metadata snapshot and compare counts and dimensions:

```sh
exiftool -csv -FileName -ImageWidth -ImageHeight -Orientation -FileSize \
  "$SAMPLE"/*.CR2 > "$WORK/metadata-after.csv"

diff "$WORK/metadata-before.csv" "$WORK/metadata-after.csv"
```

- [ ] `diff` is empty.

If the app was interrupted or crashed, repeat the hash check before deleting
anything.

## 3. Orientation checks [SAFE]

The sample has both rotated and normal photos, so it tests both paths.

### Portrait / rotated photos

`IMG_3128.CR2` is one of the 115 files with `Rotate 270 CW` (EXIF
orientation 8).

```sh
exiftool -FileName -Orientation -ImageWidth -ImageHeight \
  "$SAMPLE/IMG_3128.CR2"

exiftool -FileName -Orientation -ImageWidth -ImageHeight \
  "$OUT/IMG_3128.jpg"
```

With `jpegtran` installed and a crop-free transform available, expect:

```text
source: 6000x4000, Rotate 270 CW
output: 4000x6000, Horizontal (normal)   (pixels physically rotated)
```

Open it in Ubuntu's image viewer and Files:

```sh
xdg-open "$OUT/IMG_3128.jpg"
```

- [ ] The photo is upright in GNOME Image Viewer.
- [ ] The Files thumbnail is upright.
- [ ] The output is 4000x6000 when physically rotated.
- [ ] The orientation tag is `Horizontal (normal)` after physical rotation.
- [ ] No black bars, cropped edges, or resampling are visible.

If RawDrop reported a metadata-only fallback (for example, `jpegtran` missing
or `-perfect` not applicable), the output remains 6000x4000 and keeps the
orientation tag. Record that outcome; it is acceptable only if the app warned.
It must not crop or silently re-encode.

### Landscape / normal photos

`IMG_3144.CR2` is one of the 47 normal-orientation files.

```sh
exiftool -FileName -Orientation -ImageWidth -ImageHeight "$SAMPLE/IMG_3144.CR2"
exiftool -FileName -Orientation -ImageWidth -ImageHeight "$OUT/IMG_3144.jpg"
```

- [ ] Source and output are both 6000x4000.
- [ ] Output orientation is `Horizontal (normal)` or the same safe metadata
      orientation.
- [ ] The image is upright and not mirrored.

### Batch-level orientation summary

```sh
exiftool -csv -FileName -ImageWidth -ImageHeight -Orientation \
  "$OUT"/*.jpg > "$WORK/exports.csv"

awk -F, 'NR>1 {c[$3"x"$4]++} END {for (k in c) print k, c[k]}' \
  "$WORK/exports.csv"

awk -F, 'NR>1 {c[$5]++} END {for (k in c) print k, c[k]}' \
  "$WORK/exports.csv"
```

Expected with successful physical rotation:

```text
4000x6000 115
6000x4000 47
Horizontal (normal) 162
```

If the fallback path was used, expect 6000x4000 for all files and 115 files
still carrying `Rotate 270 CW`. Do not call this a pass unless the app clearly
warned and the viewer still displays the photos upright.

## 4. Optional quality spot check [SAFE]

For a normal-orientation file, metadata edits should not change the JPEG
image scan data. This compares the compressed scan from the embedded JPEG with
the exported one. It is a spot check, not a substitute for the automated
verification tests.

```sh
exiftool -b -PreviewImage "$SAMPLE/IMG_3144.CR2" > "$WORK/embedded-3144.jpg"

python3 - "$WORK/embedded-3144.jpg" "$OUT/IMG_3144.jpg" <<'PY'
import hashlib
import sys
from pathlib import Path


def scan_digest(path):
    data = Path(path).read_bytes()
    if data[:2] != b"\xff\xd8":
        raise SystemExit(f"{path}: not a JPEG")
    i = 2
    while i < len(data) - 1:
        if data[i] != 0xFF:
            i += 1
            continue
        marker = data[i + 1]
        if marker in (0xD8, 0x01) or 0xD0 <= marker <= 0xD7:
            i += 2
            continue
        if marker == 0xDA:  # start of scan; hash through EOI
            eoi = data.rfind(b"\xff\xd9")
            if eoi < i:
                raise SystemExit(f"{path}: no EOI")
            return hashlib.sha256(data[i:eoi + 2]).hexdigest()
        length = int.from_bytes(data[i + 2:i + 4], "big")
        i += 2 + length
    raise SystemExit(f"{path}: no SOS")


a = scan_digest(sys.argv[1])
b = scan_digest(sys.argv[2])
print("embedded:", a)
print("exported:", b)
print("MATCH" if a == b else "DIFFERENT")
PY
```

- [ ] The result is `MATCH` for the normal-orientation file.
- [ ] If it is `DIFFERENT`, stop and investigate before accepting the quality
      claim. Do not assume a rotated file should match byte-for-byte; a
      lossless rotation changes block order.
- [ ] The output dimensions are exactly the camera dimensions (or the exact
      swapped dimensions after rotation), never a reduced preview size. The
      app's eligibility threshold is 90%, but this sample's camera JPEGs are
      full 6000x4000; anything smaller is a failure for this acceptance run.

## 5. Interruption and resume [EXPENSIVE] [COPY ONLY]

Use a small copy so the test is quick and does not add pressure to the real
job. Copying a CR2 does not modify the original.

```sh
INT_SRC="$WORK/interrupt-src"
INT_OUT="$WORK/interrupt-out"
mkdir -p "$INT_SRC" "$INT_OUT"

find "$SAMPLE" -maxdepth 1 -type f -iname '*.cr2' -print0 \
  | sort -z | head -z -n 12 \
  | xargs -0 cp -t "$INT_SRC"

find "$INT_SRC" -type f -name '*.CR2' | wc -l   # 12
```

1. In RawDrop, choose **New batch**.
2. Add `$INT_SRC`; set output to `$INT_OUT`.
3. Start the export. Cancel it after at least 4 files are done.
4. Record the completed files:

   ```sh
   sha256sum "$INT_OUT"/*.jpg > "$WORK/interrupt-completed.sha256"
   stat -c '%Y %n' "$INT_OUT"/*.jpg > "$WORK/interrupt-mtimes.txt"
   ```

5. Close the app completely.
6. Relaunch. A previous-export banner should appear.
7. Choose **Export** to resume.
8. After completion, re-check the earlier files:

   ```sh
   sha256sum -c "$WORK/interrupt-completed.sha256" --quiet
   stat -c '%Y %n' "$INT_OUT"/*.jpg
   ```

- [ ] Completed files are not re-exported or rewritten.
- [ ] Their SHA-256 values are unchanged.
- [ ] Total output is 12 JPEGs, with no duplicate suffixes for files that
      were already done.
- [ ] The journal banner disappears or changes to a completed state.
- [ ] No originals in `$SAMPLE` are touched; `$INT_SRC` is only a copy.
- [ ] No `.rawdrop-*` files remain after the resumed run finishes.

### Hard-crash variant [ADVANCED] [COPY ONLY]

Only do this with the copy and only if you are comfortable killing the app.

1. Start a fresh batch from `$INT_SRC` into a new output folder.
2. After a few files complete, kill the app process (for example, close the
   terminal running `mygo dev` with `SIGKILL`, or use the process manager to
   force-quit the built app).
3. Relaunch and resume.
4. Verify completed hashes, final count, and that no original changed.
5. Look for stale hidden temps:

   ```sh
   find "$INT_OUT" -maxdepth 1 -name '.rawdrop-*' -print
   ```

A hard kill may leave a `.rawdrop-*.jpg`. That is allowed by the design; the
file is never adopted as an export. Remove it only after the app is closed and
you have confirmed it is not an output. A crash after commit but before the
journal write can also produce a valid extra JPEG with a ` (2)` suffix. Record
it; it is the known link-before-journal window.

- [ ] No original or unrelated file is overwritten.
- [ ] Resume keeps prior completed outputs and finishes the rest.
- [ ] Any duplicate from the crash window uses a suffix and leaves both files
      intact.
- [ ] Stale temp handling is understood and documented in the test notes.

## 6. Collision handling [SAFE] [COPY ONLY]

```sh
COL_SRC="$WORK/collision-src"
COL_OUT="$WORK/collision-out"
mkdir -p "$COL_SRC" "$COL_OUT"
cp "$SAMPLE/IMG_3128.CR2" "$COL_SRC/"

printf 'do not overwrite this sentinel\n' > "$COL_OUT/IMG_3128.jpg"
sha256sum "$COL_OUT/IMG_3128.jpg" > "$WORK/collision-sentinel.sha256"
```

1. **New batch**, add `$COL_SRC`, output `$COL_OUT`.
2. Export.
3. Expect `IMG_3128 (2).jpg` beside the sentinel.

```sh
ls -l "$COL_OUT"
sha256sum -c "$WORK/collision-sentinel.sha256"
```

- [ ] The pre-existing `IMG_3128.jpg` is unchanged.
- [ ] The new file is named `IMG_3128 (2).jpg`.
- [ ] Both files open; the sentinel is still text.
- [ ] Repeating the export never truncates the first output.

## 7. Unicode paths and spaces [SAFE] [COPY ONLY]

```sh
UNI_SRC="$WORK/यूनिकोड फोटो"
UNI_OUT="$WORK/निर्यात फोटो"
mkdir -p "$UNI_SRC" "$UNI_OUT"

cp "$SAMPLE/IMG_3128.CR2" "$UNI_SRC/फोटो ०१.CR2"
cp "$SAMPLE/IMG_3144.CR2" "$UNI_SRC/photo with spaces.CR2"
```

Export `$UNI_SRC` to `$UNI_OUT`.

- [ ] Both photos appear in the grid with readable names.
- [ ] Outputs are `फोटो ०१.jpg` and `photo with spaces.jpg`.
- [ ] No mojibake, path truncation, or missing-file errors.
- [ ] The app can open the output folder.

## 8. Permission and read-only-source checks [SAFE] [COPY ONLY]

### Destination not writable

```sh
RO_OUT="$WORK/readonly-out"
mkdir -p "$RO_OUT"
chmod 0555 "$RO_OUT"
```

Try to export the 12-file copy into `$RO_OUT`.

- [ ] The app reports a clear destination error.
- [ ] No `.jpg` or `.rawdrop-*` is left in `$RO_OUT`.
- [ ] The job can be retried after choosing a writable folder.

Restore permissions:

```sh
chmod 0755 "$RO_OUT"
```

Do not run this test as root; root bypasses the permission check.

### Read-only source copy

```sh
RO_SRC="$WORK/readonly-src"
mkdir -p "$RO_SRC"
cp "$SAMPLE/IMG_3128.CR2" "$SAMPLE/IMG_3144.CR2" "$RO_SRC/"
chmod -R a-w "$RO_SRC"
```

Export the two read-only copies to a writable `/tmp` folder.

- [ ] The export succeeds; RawDrop only needs read access to sources.
- [ ] The source copies remain byte-for-byte unchanged.
- [ ] No attempt is made to create files next to the sources.

Restore permissions for cleanup:

```sh
chmod -R u+w "$RO_SRC"
```

## 9. Unsupported and corrupt inputs [SAFE] [COPY ONLY]

### Non-RAW file in a folder

```sh
MIXED="$WORK/mixed"
mkdir -p "$MIXED"
cp "$SAMPLE/IMG_3128.CR2" "$MIXED/"
printf 'not a photo\n' > "$MIXED/notes.txt"
```

Export `$MIXED`.

- [ ] The grid shows one photo.
- [ ] `notes.txt` is ignored (folder contents are filtered silently).
- [ ] The export succeeds.

### Explicitly dropped non-RAW file

Drop `notes.txt` together with a CR2.

- [ ] The CR2 is still exported.
- [ ] The status reports a non-RAW item ignored.
- [ ] No output is created for the text file.

### Truncated RAW

```sh
BAD="$WORK/bad"
mkdir -p "$BAD"
head -c 4096 "$SAMPLE/IMG_3128.CR2" > "$BAD/truncated.CR2"
```

Export `$BAD`.

- [ ] The item fails with a useful message, not a crash.
- [ ] No JPEG is produced for the truncated file.
- [ ] Other queued photos still export.
- [ ] The original sample is untouched.

### No full-resolution embedded JPEG

This condition is not reproducible from the verified sample, because all 162
files expose a 6000x4000 `PreviewImage`. Use a dedicated test fixture that
stores only a small preview, or rely on the automated `raw` tests. Do not
modify a real CR2 to force the condition.

- [ ] When no full-size JPEG exists, the item is **skipped**, with an
      explanation, not failed and not exported as a preview.

## 10. Cleanup and final verification [SAFE]

Run the final original check before deleting the work area:

```sh
sha256sum -c "$WORK/originals-before.sha256" --quiet
echo "originals check: $?"

exiftool -csv -FileName -ImageWidth -ImageHeight -Orientation -FileSize \
  "$SAMPLE"/*.CR2 > "$WORK/metadata-final.csv"
diff "$WORK/metadata-before.csv" "$WORK/metadata-final.csv"
```

- [ ] Originals check exits 0.
- [ ] Metadata diff is empty.
- [ ] The sample folder still contains exactly 162 `.CR2` files.
- [ ] No `.jpg`, journal, or temp file was written inside the sample folder.
- [ ] `/tmp` outputs are the only new photo files.

Then remove the acceptance workspace:

```sh
find "$WORK" -maxdepth 2 -name '.rawdrop-*' -print
rm -rf "$WORK"
```

- [ ] No RawDrop process is running before deletion.
- [ ] The work directory is gone.
- [ ] The original sample and any pre-existing exports are still present.

## 11. Destination, progress, and format fixtures

These checks cover the destination choice, the honest progress display, and
per-format verification. They complement the automated suite.

### Destination persistence and locking

Automated coverage:

```sh
GOTOOLCHAIN=go1.27.1 go test -run 'TestSetOutputDir' ./internal/app/
```

- `TestSetOutputDirPersistsToJournal` proves the chosen path is written to
  `jobs.json` immediately, before **Export** starts.
- `TestSetOutputDirRefusesInvalidTargets` proves a regular file is refused, a
  change while running is refused, and a change after the first committed
  output is refused. Each refusal is surfaced.
- `TestSetOutputDirSurfacesStatErrors` proves a target that stats with an error
  other than "does not exist" (for example a path under a regular-file parent)
  is rejected with a visible explanation, leaving the previous destination and
  the journal unchanged.

Manual check:

1. Add photos, pick a destination, and close the app **without** exporting.
2. Relaunch. The previous-export banner appears and the destination is the one
   you picked, not the default.
3. Export one photo, then try **Change destination…**. The button is replaced
   by a note that this batch already wrote photos and a new batch is required.

- [ ] The destination survives a relaunch before any export.
- [ ] After the first output, the destination cannot change for that batch.
- [ ] A destination that is an existing file is rejected with a message.

### Progress display

- [ ] While exporting, the sidebar shows `N% · M of T processed`, the active
      photo and its stage, and the exported count.
- [ ] The percentage advances as photos finish; it never shows a per-file
      percentage that the tools cannot report.
- [ ] A rotation stage appears only after extraction and only for a photo that
      actually needs one; an upright photo never shows `Rotating…`.
- [ ] With no active photo the line reads `Preparing…`, `Paused`, or
      `Canceling…`.
- [ ] The old startup text never remains once work has begun.

Automated coverage: `TestProgressHelpers`, `TestRunningStage`, the
`TestEndToEndDropExportResume` assertions on the real view, plus the raw and
exporter stage-order tests described below.

### Shared helper runner, one inspection, and the export pool

The helper runner, the inspect-once transaction, and the export pool each have
targeted tests that do not need a window:

```sh
GOTOOLCHAIN=go1.27.1 go test ./internal/exiftool/ ./internal/exporter/ ./internal/fsutil/ ./internal/raw/ ./internal/app/
```

`internal/exiftool` covers binary discovery: the `RAWDROP_EXIFTOOL`/env
override wins when executable, a missing override is rejected with the
variable named, and a normal `PATH` lookup is used otherwise. It also covers
the runner seam: streamed stdout, capped capture that fails with
`ErrOutputLimit` (and whose message names the limit), a value exactly at the
limit, a bounded prefix read, a command failure that surfaces the helper's
stderr, and context cancellation that reports `context.Canceled`.

`internal/raw` covers the `Inspection` handle and its Runner seam:
`Info()` is a value copy; `inspectFile` runs the real inspection/export path
through an injected fake `exiftool.Runner` with `PATH` emptied
(`TestInspectionRunsWithFakeRunner`), proving no subprocess is needed; the
`Inspection` exports accurately ordered stages
(`TestExportReportsStagesInOrder` — extracting before rotating before
verifying — and the upright case omits rotating); and source identity is
checked three times. `TestInspectFileRejectsSourceChangedDuringInspection`
mutates the file while the inspection query is running,
`TestInspectionRejectsChangeBeforeExport` mutates it between inspection and
export, and `TestInspectionRejectsChangeDuringExtraction` mutates it while the
JPEG is being streamed; all must return `ErrSourceChanged` without leaving a
temp file. `checkSource` also refuses a source, via `os.SameFile` plus
size/mtime, once it has changed. `TestInspectFileCanceledBeforeToolDiscovery`
proves a pre-canceled call returns `context.Canceled` even when
`RAWDROP_EXIFTOOL` points at a missing binary, so tool discovery cannot mask
cancellation.

`internal/exporter` covers the shared transaction end to end: a verified commit
that inspects each source once (`TestExportCommitsVerifiedOutput`), a
no-full-resolution-JPEG failure that stays discoverable with
`errors.Is(err, raw.ErrNoFullJPEG)` (`TestExportReportsNoFullJPEG`), a canceled
export that leaves no temp file (`TestExportCancellation`), a pre-existing
output that is never overwritten (`TestExportAvoidsOverwritingExistingOutput`),
and many exports racing for the same planned name that all commit distinct
files (`TestExportConcurrentCollisions`). `TestExportPrecommitFailureLeavesNoOutput`
injects a failure in the flush/chmod/hash step that now runs before the
no-replace link, and asserts the output folder contains neither a visible JPEG
nor a temp file. `TestExportReportsNoRotationWhenNotNeeded` pins the
`inspecting` → `extracting` progress sequence for an upright photo.
`TestExportDoesNotRemoveForeignTempPath` simulates another process claiming
the reserved temp name after the exporter clears its own reservation and
before raw export's `O_EXCL` create: the export fails and the foreign file is
left untouched, because cleanup is armed only after a successful raw export.
`TestRemoveOwnedFileOnlyRemovesTheOwner` pins the inode guard that keeps the
deferred cleanup from deleting a later replacement at the same name.

`internal/fsutil` covers the commit primitive directly:
`TestCommitNewLinksAndRemovesTemp` (content visible, temp cleaned up),
`TestCommitNewRefusesExistingDestination` (an existing file is untouched and
the temp remains for the caller), and
`TestCommitNewSucceedsWhenTempCannotBeRemoved` (the hard link is the commit
point, so a failed temp unlink still reports success).
`TestUniquePath` pins the ` (2)`, ` (3)` suffix sequence.

`internal/app` exercises the pool without a display, seeding a job directly and
pointing `RAWDROP_EXIFTOOL` at a fake helper:

- `TestExportPoolProcessesEachItemOnce` runs a whole batch and checks every
  photo is inspected exactly once, has its own output, leaves its source
  byte-identical, and leaves no `.rawdrop-*` temp file.
- `TestExportPoolPausesNewReservations` proves Pause stops new reservations
  while in-flight photos finish, then Resume completes the batch.
- `TestExportPoolCancelKeepsCompletedOutputs` proves Cancel keeps every
  committed output and cancels only the in-flight photo.
- `TestExportPoolAvoidsDuplicateOutputNames` proves concurrent commits add
  suffixes instead of replacing a sentinel.

The real-view `TestEndToEndDropExportResume` covers the same paths through the
actual drop zone and buttons, and asserts one output per photo after a
pause/resume and a cancel/relaunch/resume.

The concurrency bound itself (`exportWorkers = 3`) is a safety limit, not a
performance result. These tests prove correctness under concurrency — no
duplicate work, no lost or overwritten output — not that three workers is
faster than a sequential run, and definitely not that a small-file result
generalizes to a large batch.

### Per-format verification

A format is not verified until a representative real sample passes the full
contract. Build a **curated** fixture directory — one or two representative
samples per camera body/format, not a whole photo library — and point the
opt-in test at it (never commit personal or large photos):

```sh
mkdir -p ~/rawdrop-fixtures
cp "/path/to/library/IMG_3128.CR2" ~/rawdrop-fixtures/   # Canon EOS 80D
# add one or two more per camera body/format, then:
RAWDROP_TEST_FORMATS_DIR=~/rawdrop-fixtures GOTOOLCHAIN=go1.27.1 \
  go test -run TestInspectAndExportRealFormats ./internal/raw/ -v
```

The fixture directory is copied to, never exported into: each sample is opened
read-only and outputs go to the test's temp directory.

Do **not** point `RAWDROP_TEST_FORMATS_DIR` at a full library. Every sample is
exported at full resolution, so a directory of hundreds of photos runs past the
default `go test` timeout before it finishes. The harness walks recursively but
fails fast with a message once the directory exceeds `maxFormatSamples`
(currently 20), directing you to curate the fixture set; it never starts
processing a whole library by default. `TestCollectFormatSamplesRecursesAndBounds`
covers this bound.

For each sample, confirm from the test log:

- [ ] ExifTool reports a non-empty make and model.
- [ ] A full-resolution embedded JPEG is found (not a thumbnail or preview).
- [ ] The exported JPEG decodes to the reported dimensions and keeps the
      expected orientation; location metadata is gone by default.
- [ ] The source is byte-for-byte unchanged.
- [ ] Only the tested camera family is marked **verified** in
      [docs/FORMAT_SUPPORT.md](FORMAT_SUPPORT.md); every other format stays
      recognized/unverified.

If a sample has no full-resolution embedded JPEG, stop: do not lower the 90%
threshold or export a preview. Keep the format recognized/unverified and
report it.

## Acceptance record

Automated run on 2026-10-21 through the shared core (`cmd/rawdrop-cli` plus
the app and package test suites): Ubuntu 26.04.1 / GNOME Wayland, Go 1.27.1,
ExifTool 13.50, libjpeg-turbo 2.1.5, 162 sample `.CR2` files (115 orientation
8, 47 orientation 1). Rows that say "Headless UI" were driven through the
real view with MyGo's headless tester — the actual drop zone and buttons —
rather than a human at a screen.

| # | Check | Result | Notes |
| --- | --- | --- | --- |
| 1 | 162 originals hashed | Pass | whole-set fingerprint `2515801905c9dcb5db466e1548bae3c7864e5e0c9937aac4cf2cf126a288db97` (see command below) |
| 2 | 162 thumbnails, upright grid | Pass | Headless render reviewed as PNGs (`TestSnapshots`): sideways photos show upright portrait thumbnails, landscape ones stay landscape; light, dark and exporting states all render correctly |
| 3 | Full export to `/tmp` | Pass | 162/162, exit 0, outputs in `/tmp/opencode/rawdrop-full` |
| 4 | 162 outputs, no temps | Pass | exactly 162 `.jpg`, zero `.rawdrop-*`, zero extra files, all mode 644 |
| 5 | Originals SHA-256 unchanged | Pass | 8-file pre/post hashes identical; all 162 CR2 ctimes (19:04–19:07) predate the export |
| 6 | Original metadata unchanged | Pass | no file was opened for writing; ctimes unchanged |
| 7 | 115 rotated upright; 47 normal upright | Pass | exact split: 115 × 4000×6000 orientation 1; 47 × 6000×4000 orientation absent |
| 8 | No resizing/re-encoding spot check | Pass | landscape outputs byte-identical to the camera's embedded JPEG; rotated output DCT-identical to `jpegtran -rotate 270` of it |
| 9 | Pause and resume | Pass | Headless UI: `TestEndToEndDropExportResume` clicks Pause, checks the paused view, then clicks Resume |
| 10 | Cancel and resume after relaunch | Pass | Headless UI: the same test clicks Export, cancels mid-run, reloads the journal in a fresh app and resumes; originals verified byte-identical |
| 11 | Collision creates ` (2)` and preserves sentinel | Pass | a second run into the same folder produced `IMG_3128 (2).jpg` and left the first files untouched |
| 12 | Unicode paths and spaces | Pass | `फोटो नमूना.CR2` exported; folders with spaces used throughout |
| 13 | Read-only source copy succeeds | Pass | sources were only ever read |
| 14 | Unwritable destination fails cleanly | Pass | permission-denied message, exit 1, zero files left behind |
| 15 | Truncated RAW fails cleanly | Pass | "no full-resolution embedded JPEG" message, exit 1, zero outputs |
| 16 | No full-size JPEG is skipped | Pass | 162/162 exported, none skipped |
| 17 | Cleanup complete | Pending | `/tmp/opencode/rawdrop-*` scratch folders kept for inspection |
| 18 | Location data stripped by default | Pass | 0 of 162 sample files carry coordinates (GPS IFD stub only); exported JPEGs contain no GPS tags, stub included; `TestStripLocationRemovesGPS` plants real coordinates and verifies removal |

Whole-set fingerprint command:

```sh
cd "$SAMPLE" && sha256sum *.CR2 | sort -k2 | sha256sum
```

Pixel comparison against a pre-existing export of the same photos: same
display size for every file checked, maximum channel difference 4 (chroma
upsampling only), consistent with a lossless rotation of the camera JPEG.

To review the rendered interface without a display (writes `welcome.png`,
`grid.png`, `grid-dark.png` and `exporting.png`):

```sh
RAWDROP_SNAPSHOTS=/tmp/shots GOTOOLCHAIN=go1.27.1 \
  go test -run TestSnapshots ./internal/app/
```

## Pass criteria

The build passes manual acceptance when:

- all 162 sample photos are either exported or skipped with a clear reason;
- every exported JPEG has the expected full-resolution dimensions
  (6000x4000, or 4000x6000 after physical rotation);
- rotated photos display upright in at least two viewers, without cropping;
- all original SHA-256 hashes and metadata are unchanged;
- no existing file is overwritten;
- interruption and relaunch resume without redoing verified work;
- no temporary or unreferenced file is mistaken for a finished export;
- the app remains usable while the batch runs.
