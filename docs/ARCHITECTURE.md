# RawDrop architecture

This document describes how RawDrop moves a dropped RAW file to a verified
JPEG without changing the original. It covers the package layout, the
single-item pipeline, the no-replace write protocol, the job journal, the
state machine, crash recovery, and the threading rules for MyGo.

## Design constraints

1. **The original is read-only.** No code path renames, moves, writes, or
   deletes a source file.
2. **The camera JPEG is copied, not re-encoded.** RawDrop extracts the
   full-resolution embedded JPEG and never renders the RAW sensor data.
3. **A preview is never a fallback.** The embedded JPEG must match the
   camera's full image dimensions. Otherwise the item is skipped.
4. **Existing files are never overwritten.** Final commits use an atomic
   no-replace hard link.
5. **The app stays offline.** Processing is local; helpers are local
   processes.
6. **The UI thread stays responsive.** File I/O, ExifTool, jpegtran, image
   decoding, hashing, and journal writes happen outside `View`.
7. **A crash may lose progress, but not data.** Completed outputs and
   originals are left intact; unfinished work can be resumed.

## Package layout

```text
rawdrop/
├── main.go                    creates the MyGo window, wires app.Attach
├── main_test.go               UI-level tests with ui.NewTester
├── go.mod                     module rawdrop, Go 1.27.1, MyGo v0.6.2
├── mygo.json                  native app metadata and build output
├── resources/icon.png         1024x1024 app icon
├── cmd/rawdrop-cli/main.go    headless exporter using the same core
├── scripts/acceptance.sh      copy-only end-to-end check on real photos
└── internal/
    ├── app/
    │   ├── app.go             state, scanning entry, export worker pool, journal
    │   └── view.go            native view, snapshots, actions
    ├── exiftool/
    │   └── exiftool.go        shared helper runner: discovery, bounded execution
    ├── exporter/
    │   └── exporter.go        one-photo transaction shared by app and CLI
    ├── fsutil/
    │   └── fsutil.go          CommitNew, UniquePath, CopyFile, SyncDir
    ├── journal/
    │   ├── journal.go         versioned jobs.json, atomic Save/Load/Clear
    │   └── lock_*.go          single-instance lock
    ├── model/
    │   └── model.go           Job, Item, Settings, Status, RAW extensions
    ├── raw/                   tag interpretation, extraction, rotation, verify
    ├── scan/
    │   └── scan.go            traversal, dedupe, output name planning
    └── thumb/
        ├── thumb.go           bounded upright thumbnails for the grid
        ├── exif.go            JPEG EXIF orientation reader
        └── tool.go            bounded thumbnail runner over internal/exiftool
```

`internal/exiftool` is the one place that knows how to find and run a helper
binary: discovery (honoring `RAWDROP_EXIFTOOL`), streamed extraction, capped
capture, and prefix reads. It knows nothing about RAW formats or ExifTool's
tags. `raw` and `thumb` interpret tags and bytes; `raw` keeps jpegtran
discovery and rotation because those are specific to lossless rotation.

`internal/exporter` is the shared one-photo transaction: inspect once through
`raw.InspectFile`, extract, apply the metadata policy, verify, and commit into
the destination with a no-replace link and a final hash. It imports neither
MyGo nor the job/journal model, so both the desktop app and the headless CLI
use the same code without either owning the other.

Dependency direction:

```text
main                              cmd/rawdrop-cli
  └── app ──┬── model                 └── exporter ──┬── raw ── exiftool
            ├── scan ────── model                    └── fsutil
            ├── exporter ─── raw, fsutil
            ├── raw ─── exiftool
            ├── thumb ─── exiftool
            ├── journal ─── model, fsutil
            └── fsutil
```

`model`, `scan`, `raw`, `thumb`, `exiftool`, `exporter`, `journal`, and
`fsutil` do not import MyGo. Only `internal/app` does. This keeps the
processing core testable without a window and leaves a path to other desktop
frontends.

### Headless CLI

`cmd/rawdrop-cli` is a second caller of the core. It uses `scan.Collect` and
`exporter.Export`, the same one-photo transaction the app uses, but has no
MyGo window and no journal. It processes inputs sequentially, applies the same
no-replace commit, and is useful for scripts and acceptance runs. It does not
resume an interrupted batch. `scripts/acceptance.sh` drives this CLI against
copies of real photos and checks hashes, dimensions, and orientation.

## End-to-end flow

```text
 User drops files/folders, or picks them
                  │
                  ▼
        ┌───────────────────┐
        │ app.AddPaths      │
        │ unique abs paths  │
        └─────────┬─────────┘
                  │  scan.Collect(ctx, inputs, outputDir)
                  ▼
        ┌───────────────────┐      ┌──────────────────────────┐
        │ scan              │      │ model.Job + Items        │
        │ walk, dedupe,     │─────▶│ queued, stable IDs,      │
        │ exclude output,   │      │ planned .jpg names       │
        │ plan names        │      └────────────┬─────────────┘
        └───────────────────┘                   │
                                                ▼
                                     ┌──────────────────────┐
                                     │ journal.Store.Save   │
                                     │ jobs.json, version 1 │
                                     └──────────┬───────────┘
                                                │
              ┌─────────────────────────────────┴────────────────┐
              │                                                  │
              ▼                                                  ▼
   ┌─────────────────────┐                          ┌────────────────────────┐
   │ thumb workers (4)   │                          │ export workers (3)      │
   │ exiftool + decode   │                          │ exporter.Export per     │
   │ 320 px, in memory   │                          │ photo; pause/cancel     │
   └──────────┬──────────┘                          └───────────┬────────────┘
              │ Window.Update                                   │
              ▼                                                 ▼
   ┌─────────────────────┐                          ┌────────────────────────┐
   │ native photo grid   │                          │ exporter.Export        │
   └─────────────────────┘                          │  inspect once          │
                                                    │  extract + metadata    │
                                                    │  verify                │
                                                    │  CommitNew + SHA-256   │
                                                    │ journal (app only)     │
                                                    └───────────┬────────────┘
                                                                ▼
                                                    ┌────────────────────────┐
                                                    │ new output folder      │
                                                    │ IMG_3128.jpg, ...      │
                                                    │ originals untouched    │
                                                    └────────────────────────┘
```

## 1. Scan and plan

`App.AddPaths` takes dropped paths or picker results, converts them to clean
absolute paths, sorts and deduplicates them, and chooses a default output
folder when none is set.

`scan.Collect` then:

- accepts explicit files only when their extension is a known RAW extension;
- walks explicit folders recursively without following directory symlinks;
- ignores non-RAW files found during a walk;
- records explicitly dropped non-RAW files as `Unsupported`;
- records unreadable directories and excluded output paths in `SkippedDirs`;
- excludes the output directory and everything beneath it;
- deduplicates overlapping inputs by cleaned absolute path;
- assigns stable item IDs as `SHA-256(cleaned absolute source path)`;
- assigns deterministic `.jpg` names, adding ` (2)`, ` (3)`, … for duplicate
  basenames, including names that would collide with an existing planned name.

Scanning is bounded by a 10-minute context. The result is a job plan, not an
export: dropping files never starts writing JPEGs.

The recognized extension set is defined once in `model.rawExtensionList` and
shared with the native picker, so recognition and routing cannot drift.
Recognizing an extension is a routing hint, not a compatibility claim; see
section 11 and `docs/FORMAT_SUPPORT.md`.

The item ID is stable for the same input path, but it is not a content
identity. `Item` also records size and modification time from the scan for
display and diagnosis. The exporter re-reads the source at export time and
validates the actual embedded JPEG, so a source replaced between planning and
export is processed as it exists then; RawDrop still only reads it. The
journal's SHA-256 is for the **output**, not the source.

## 2. Per-item pipeline

A bounded pool of export workers runs the same one-photo transaction
(`internal/exporter`) for independent photos; see section 7 for the pool's
concurrency and pause/cancel behavior. Each item proceeds through these
stages:

```text
queued
  │
  ├─▶ inspecting   ExifTool: camera dimensions, orientation, embedded JPEG
  │        │
  │        ├─ no full-resolution embedded JPEG ──▶ skipped
  │        ▼
  ├─▶ extracting   extract embedded JPEG to a temp file in the destination
  │        │       (metadata policy applied to the temp file here)
  │        │
  │        ├─ orientation needed ──▶ rotating
  │        │                          jpegtran DCT transform when possible
  │        ▼
  ├─▶ verifying    JPEG integrity, decoded dimensions, expected size
  │        │
  │        ▼
  ├─▶ commit       hard-link temp to a unique final name
  │        │
  │        ▼
  └─▶ done         SHA-256 final file, journal the result

Any active state may become failed. A canceled context produces canceled.
A missing/full-size-preview condition produces skipped, not failed.
```

The exporter inspects the source exactly once. `raw.InspectFile` returns an
`Inspection` handle that keeps the metadata **and** the embedded-JPEG
candidates, and the same handle performs the extraction. There is no separate
second ExifTool read for Inspect and Export. Before extraction the handle
re-stats the source and refuses to use candidate byte offsets if the file's
size or modification time changed, so a source replaced between inspect and
extract fails (or is re-inspected by the caller) instead of extracting from a
stale offset.

`raw.Export` and `raw.Inspect` remain as thin compatibility wrappers over this
handle for callers that do not need to share an inspection.

### Inspection

`raw.InspectFile` invokes ExifTool locally and reads:

- the camera's full image dimensions;
- the embedded JPEG dimensions and orientation;
- the embedded JPEG payload used for export.

The important check is dimensional: a candidate must reach at least 90% of
the camera's usable displayed dimensions. The tolerance accommodates minor
camera cropping, while still rejecting normal preview images. RawDrop does not
assume that an extension, a tag name, or a file size proves this. Tag selection
can differ between RAW formats; the verified Canon EOS 80D `.CR2` sample
exposes a 6000x4000 JPEG as `PreviewImage`. Candidates are tried in
`PreviewImage`, `JpgFromRaw`, then `OtherImage` order, and each is validated
after extraction.

If no candidate qualifies, the item becomes `skipped` with a reason. The
thumbnail code may use a smaller `ThumbnailImage` for the grid; that image is
never eligible for export.

### Extraction and orientation

The exporter creates a uniquely named temporary file on the **destination**
filesystem, not in the system temp directory. The name starts with
`.rawdrop-` and ends in `.jpg`. This guarantees the final hard link does not
cross filesystems.

The embedded JPEG is written to that temporary file. Before verification,
ExifTool removes GPS and geotag metadata unless **Keep location data** is
enabled. Other metadata and the color profile already present in the embedded
JPEG are left in place; RawDrop does not synthesize a new metadata set from the
RAW file.

If orientation correction is enabled and the source needs it:

- `jpegtran` applies a lossless DCT transform (for example, `-rotate 270`) to
  the temporary JPEG;
- the crop-free `-perfect` mode is used when possible;
- the orientation tag is reset only after the physical rotation succeeds.

If the transform cannot be applied without cropping or re-encoding (including
when `jpegtran` is unavailable), RawDrop retains the orientation metadata and
records a warning. It does not silently crop, resize, or encode the image a
second time.

### Verification

Before commit, the temporary file must:

- be a complete JPEG with a frame header, a scan, and an end-of-image marker;
- decode successfully and have positive dimensions;
- match the dimensions expected after any physical rotation;
- carry the expected EXIF orientation;
- contain no GPS/geotag metadata when location preservation is off.

The write path has no resize or JPEG-encode step: the embedded JPEG is streamed
out by ExifTool, and an orientation change is a `jpegtran -perfect` DCT
transform. The final SHA-256 is computed after commit and stored in the
journal. The hash is the resume-time integrity check for the committed output.

## 3. Safe-write algorithm

The final write is deliberately not a normal `rename` over the destination.
`rename` replaces an existing file silently, which would violate the
no-overwrite contract.

```text
export_one(item):                          # internal/exporter.Export
    # 1. inspect the source exactly once; the handle also extracts
    insp = raw.InspectFile(source)

    # 2. prepare a temporary file on the same filesystem as the destination
    temp = create_unique_temp(dest_dir, ".rawdrop-*.jpg")
    remove(temp)                          # the exporter requires a new path
    defer remove(temp)                    # graceful failure/cancel cleanup

    # 3. extract and apply the metadata policy into temp; verify before commit
    insp.Export(temp, options)            # verify_jpeg happens inside here

    # 4. choose a name, then commit with an atomic no-replace operation
    name = planned_output_name or stem + ".jpg"
    for attempt in 1 .. max_name_attempts:
        dest = fsutil.UniquePath(dest_dir, name)   # planning aid only
        fsutil.CommitNew(temp, dest):
            if lstat(dest) exists: return EEXIST
            os.Link(temp, dest)               # atomic; refuses to replace
            os.Remove(temp)
        on EEXIST: continue                   # a concurrent writer took the name
        on success or other error: break

    # 5. make the directory entry durable and the file normally readable
    chmod(dest, 0644)
    fsutil.SyncDir(dest_dir)

    # 6. hash the committed file; the app records status=done in the journal
    hash = sha256(dest)
    return Result{output_path, hash, dimensions, orientation, warning, ...}
```

Invariants:

- **Temp and destination are on the same filesystem.** The temp file is
  created inside the chosen output folder.
- **Verification happens before the final name appears.** A failed extraction
  or verification never creates a visible `.jpg`.
- **The hard link is the authority.** `UniquePath` is only a race-reducing
  suggestion. `os.Link` fails with `EEXIST` if another writer — a process, or
  one of the pool's own workers — created the name after the check. The
  exporter retries with the next suffix (bounded) rather than replacing
  anything.
- **The file is flushed and closed before its final name appears.** The
  extracted JPEG is fsynced, all metadata/rotation helpers close the temp
  file, and the destination directory is synced after the link.
- **The journal is written last.** A file is not reported as done until its
  final path and hash are recorded.
- **No cross-device link.** A destination on another filesystem is not
  silently copied into place; the temp file is already there, so `os.Link`
  works or the item fails.
- **Hard-link requirement.** Filesystems without hard links (FAT/exFAT, some
  network mounts) cannot use this commit protocol. The error is reported; no
  partial final file is left by the failed link.

The no-replace link is also what makes collisions safe. A pre-existing
`IMG_3128.jpg` is never opened for writing; the second export becomes
`IMG_3128 (2).jpg`.

## 4. Journal and single-instance ownership

The journal lives in MyGo's application-data directory:

```text
<user data>/jobs.json     versioned model.Job JSON
<user data>/jobs.lock     advisory lock held while the app owns the job
```

`jobs.json` contains:

- schema version (currently `1`);
- job ID, inputs, settings, creation/update times;
- every item: source, planned output name, status, message/warning,
  dimensions, orientation, rotated flag;
- completed output path and SHA-256.

`journal.Store.Save` writes atomically:

```text
jobs.json.tmp-*  → write → chmod 0644 → fsync → close
                 → rename over jobs.json
                 → fsync directory
```

A reader therefore sees either the old complete JSON or the new complete JSON,
never a half-written document. `Load` rejects malformed JSON, missing/invalid
versions, future schema versions, null items, and items without a source.
`Clear` removes the journal and syncs the directory.

The store is **not** internally concurrent-safe; RawDrop serializes `Load`,
`Save`, and `Clear` under `App.mu`. The lock file prevents two app instances
from owning the same resumable job. A second instance does not resume that
journal while the first holds the lock.

Journal writes are best-effort in the current controller: a failed save does
not abort an otherwise valid export. The consequence is a lost resume point,
never a modified original or an overwritten output.

## 5. State machine

### Job phase

```text
             add paths                 export
   empty ───────────────▶ ready ───────────────▶ running
     ▲                     ▲  │                    │
     │                     │  │                    ├── pause flag (not a phase)
     │                     │  │                    │       │
     │                     │  └──── cancel ◀───────┘       │
     │                     │          (returns ready)      │
     │                     │                               ├── all items terminal
     │                     │                               ▼
     │                     │                            results
     │                     └──────── new batch ────────────┘
     └──────────────────────────────────────────────────────
```

- `empty`: no job.
- `ready`: a plan exists, including a loaded previous job.
- `running`: an export pool is active. `paused` is an orthogonal flag.
- `results`: the current batch reached a terminal outcome and shows counts,
  **Retry failed**, **Open folder**, and **New batch**.

A previous job with unfinished items loads into `ready`; a fully finished job
loads into `results`.

### Item status

```text
                    ┌──────────────▶ skipped
                    │             (no full-size embedded JPEG)
                    │
queued ─▶ inspecting ─▶ extracting ─▶ rotating ─▶ verifying ─▶ done
   ▲            │            │            │            │
   │            └────────────┴────────────┴────────────┴──▶ failed
   │                                                          │
   ├──────────────── retry (queued/failed/canceled) ◀─────────┘
   │
   └──────────────── canceled (cancellation)
```

| Status | Meaning | Retried by Export? |
| --- | --- | --- |
| `queued` | Planned, not attempted | Yes |
| `inspecting` | Reading metadata and eligibility | Requeued after restart |
| `extracting` | Writing the embedded JPEG to temp | Requeued after restart |
| `rotating` | Applying a lossless DCT transform | Requeued after restart |
| `verifying` | Checking JPEG integrity/dimensions | Requeued after restart |
| `done` | Committed, hashed, and journaled | No; validated at startup |
| `skipped` | Unsupported or no full-resolution JPEG | No |
| `failed` | Extraction/verify/commit error | Yes, via Export/Retry failed |
| `canceled` | Stopped by the user or window close | Yes |

Only `done`, `skipped`, `failed`, and `canceled` are terminal. Pause stops
scheduling new items but lets in-flight items finish; it does not mark them
canceled. Cancel cancels the context, terminates running helpers, keeps every
committed file, and marks unfinished items canceled.

`verifying` is part of the model's vocabulary, but the exporter validates the
JPEG inside `Inspection.Export` before committing. The exporter reports only
three progress stages — `inspecting`, `extracting`, and `rotating` — so the UI
may move from `rotating` directly to `done` without persisting a separate
`verifying` update.

`done` is revalidated on load: the output must exist, not be a directory, and
match the stored SHA-256 (when a hash was recorded). A missing or modified
output returns to `queued` and is exported again under a new collision-safe
name. A user-modified output is never overwritten.

## 6. Crash recovery

At launch, `app.Attach` opens the journal, takes the lock, loads the job, and
normalizes every item:

```text
done       → keep only if output exists and hash matches;
             otherwise queued with "Output missing or modified"
skipped    → keep
failed     → keep visible; Export can retry
canceled   → keep visible; Export can retry
everything else (inspecting/extracting/rotating/verifying/queued)
           → queued with "Resuming after interruption"
```

Then the app shows a previous-export banner and waits for the user to start
the export; it does not write photos without an explicit action.

Crash scenarios and their outcomes:

| Crash point | What survives | Recovery |
| --- | --- | --- |
| During scan | Partial plan may be journaled; no outputs | Reload; unfinished items are queued |
| During inspect/extract | Temp file may remain; source untouched | Temp is ignored; item is requeued |
| After temp verify, before link | Verified temp, no final name | Temp is ignored; item is requeued |
| After hard link, before journal | Complete JPEG at final name, item still active | Item is requeued; a second export gets ` (2)`; the orphan is never overwritten |
| After journal write | `done` + output + hash | Startup validates the hash and skips re-export |
| During journal write | Old complete JSON or new complete JSON | Atomic rename prevents a half-written journal |
| While thumbnails run | In-memory thumbnails lost; journal intact | Thumbnails are rebuilt; no export effect |

The link-before-journal window is the one known duplicate window. It is a
deliberate trade: the app chooses to leave an unreferenced valid JPEG and
export again, rather than overwrite or delete a file it cannot prove it owns.
The duplicate has a suffix and is easy to identify.

A hard kill can also leave a `.rawdrop-*.jpg` temp file. It is hidden, never
loaded as an output, and never renamed over anything. Once the app is closed,
it can be removed manually. Graceful cancel and normal failures remove the
current temp with a deferred cleanup.

## 7. Worker model and cancellation

| Work | Concurrency | Notes |
| --- | --- | --- |
| Scan | One call per add-paths action | Recursive walk, 10-minute context |
| Thumbnails | 4 workers | One bounded queue; 320 px max edge, JPEG q80, memory only |
| Export | 3 workers | One shared queue; each photo reserved atomically; pause at item boundary |
| Journal | Serialized by `App.mu` | Single writer; atomic file replacement |
| ExifTool | Child process per request | Bounded stdout/stderr and per-request timeout |
| jpegtran | Child process per rotation | `-perfect`; no crop or re-encode fallback |

Thumbnails use a bounded worker pool so a 162-photo drop does not start 162
ExifTool processes. Each thumbnail attempt has a 20-second context at the app
layer, and the tool runner enforces its own timeout and output caps. Thumbnail
failures are non-fatal: the grid shows a `RAW` placeholder instead.

At startup the app also checks for both helpers in a background goroutine. A
missing ExifTool or `jpegtran` produces an install banner instead of letting
every photo fail silently. ExifTool is required for inspection/export;
`jpegtran` is required only for physical rotation, which degrades to a warning
and metadata-only orientation when unavailable.

Scanning is bounded by a 10-minute context but currently runs synchronously
inside the add-paths action rather than in its own goroutine. It is outside
`View` and outside the export worker, so it cannot corrupt UI state, but a very
large folder can delay the next frame. Moving the scan behind a goroutine is a
contained change because `scan.Collect` already takes a context and returns a
plain plan.

The export pool runs at most `exportWorkers` (3) photos at once. Three is an
arbitrary safety bound that overlaps the I/O and process time of independent
photos while keeping ExifTool child processes, memory, and disk contention
bounded — not a measured optimum, and not a throughput claim. A run-through of
a small file set is not evidence that the bound generalizes to a large batch;
larger batches still serialize through the same disk and helper processes.

Each worker claims the next `queued` item by marking it `inspecting` under
`App.mu`, so two workers can never reserve the same photo. A failed or
canceled item is requeued once by `StartExport` before the pool starts,
rather than being reselected inside the loop, so a repeatedly failing item
cannot spin forever within one run. The journal is saved under `App.mu` at
each reservation, stage change, and terminal outcome.

Pause sets a flag and does not disturb work already reserved: a worker about
to reserve a new item waits on `pauseWait` instead, so in-flight photos finish
while no new ones start. Resume closes `pauseWait` (waking every waiting
worker) and installs a fresh channel for the next pause. Cancel cancels the
shared context and any running helper process, and also wakes workers waiting
on `pauseWait`; every committed output is kept and only unfinished items
become canceled. The run finalizes — leaving the running phase and saving the
final state — only after every worker has stopped, so a late worker cannot
race the final journal write.

`runID` invalidates stale work. Starting a new batch increments it. A worker
that finishes after a reset checks `runID` before publishing state, so an old
scan, thumbnail, or export cannot mutate the new job. `Window.Update` results
are likewise only applied after the guard.

## 8. Threading rules (MyGo)

MyGo drives GTK on the process main thread. The view runs there whenever the
window needs a frame: after input, after `Window.Update`, and while something
animates.

Rules used by RawDrop:

1. **`View` runs only on the UI thread.** It calls `App.snapshot()` once,
   copies all job fields it needs, and then builds elements. Callbacks may call
   `App` methods; they never retain elements or a `ui.Context`.
2. **UI-only state stays on the UI thread.** `grid`, `bitmaps`, `thumbSize`,
   `selected`, and `icon` are not touched by workers.
3. **Workers never touch MyGo UI objects.** Thumbnail and export goroutines
   use `App.mu` for shared state and call `Window.Update` to request a frame.
   `Window.Update` is asynchronous: it queues the function on the main thread
   and returns without waiting.
4. **No slow work in `View`.** Image decode/encode, ExifTool, jpegtran, file
   hashing, scanning, and journal I/O are not part of element construction.
5. **Snapshots are immutable copies.** The grid receives `[]model.Item`
   copies, not pointers that a worker can mutate mid-frame. Thumbnail bytes are
   copied into in-memory items and decoded into `ui.Bitmap` only in the view.
6. **One mutex, one writer discipline.** `App.mu` guards the job, phase,
   status, settings, and cancellation state. The journal store is called only
   while holding that mutex, because it is not internally concurrent-safe.
7. **Stale results are dropped.** `runID` and context cancellation prevent a
   late worker from publishing into a new batch.
8. **MyGo UI values never escape the main thread.** `mygo vet` checks for
   build values stored in structs or captured by goroutines.

The intended data flow is therefore:

```text
 worker goroutine                       main thread
 ────────────────                       ───────────
 read source / run tool
 build result
 lock App.mu
 mutate job fields
 save journal (serialized)
 unlock
 win.Update(func(){}) ────────────────▶ queue fn, then rebuild View
                                        snapshot under App.mu
                                        decode thumbnails, draw frame
```

Avoid calling `Invalidate` as a substitute for synchronization: it only asks
for another frame and does not make model writes safe. Use `Window.Update`
when shared state changes.

## 9. Verification and quality boundaries

RawDrop can guarantee **no additional JPEG encoding generation**. It cannot
make JPEG lossless, and it cannot recover detail the camera did not store.

What the code verifies:

- the embedded image is a decodable JPEG;
- its dimensions are the camera's full image dimensions, not a thumbnail;
- stored width/height and orientation are consistent after rotation;
- the temporary file decodes before it is committed;
- the committed output's SHA-256 is recorded for resume validation.

What it deliberately does not do:

- render RAW sensor data;
- resize or re-encode the JPEG;
- replace a missing full-size JPEG with a preview;
- crop to make a rotation fit;
- overwrite a pre-existing output;
- modify, rename, move, or delete a source.

Color profiles and other tags already inside the embedded JPEG are preserved.
RawDrop does not copy a separate metadata set from the RAW container. GPS and
geotag tags are removed unless **Keep location data** is enabled. Metadata
changes do not re-encode the compressed image; a physical orientation change
uses the lossless DCT transform.

## 10. Known implementation gaps

These are useful to know when reviewing or extending the app:

- The full-size test is a 90% dimensional threshold, not a byte-exact or
  pixel-exact proof. It rejects ordinary previews, but a near-full-size crop
  embedded by a camera would also pass.
- Scanning currently runs synchronously in the add-paths action, so a huge
  folder can briefly delay the next frame.
- The CLI has no journal or resume behavior.
- The journal is best-effort; save errors are not surfaced to the user.
- A crash after commit but before the journal write can leave a valid
  unreferenced JPEG. Recovery may export a suffixed duplicate.
- Stale `.rawdrop-*.jpg` files from a hard kill are not cleaned automatically.
- The MVP validates Canon `.CR2`; the scanner is broader than the verified
  exporter coverage. The recognized, exportable and verified levels are
  tracked per camera family in `docs/FORMAT_SUPPORT.md`.
- Physical rotation depends on `jpegtran` and on `-perfect` being applicable;
  metadata-only orientation remains the safe fallback.
- Hard-link commits restrict destinations to filesystems that support links.
- Only one job is retained; **New batch** clears the journal rather than
  keeping a history.

## 11. Format support, destination, and progress

### Recognized, exportable, verified

The scanner's recognized set is defined once, in
`model.rawExtensionList` (`internal/model/model.go`); `RawExtensions` and the
native picker's filter are derived from it, so the two cannot drift. An
extension being recognized only routes a file to the exporter. For a given
file, **exportable** means ExifTool could read the metadata and an embedded
JPEG reached the 90% full-resolution threshold. **Verified** is a stronger,
per-camera claim: a representative real sample passed inspect, export,
orientation, metadata policy, output validation, and unchanged-source checks.
`docs/FORMAT_SUPPORT.md` records which is which; no non-Canon format is
verified without such a sample. Recognition, export eligibility, and the
no-preview-fallback rule are otherwise independent of format.

### Destination persistence

`SetOutputDir` normalizes the chosen path and, when a job exists, writes
`Settings.OutputDir` to the journal immediately — it does not wait for
**Export** to become durable. The destination is validated: a path that exists
and is a regular file is refused, and the refusal is surfaced rather than
silently ignored; a missing directory is allowed, because the export creates
it. Changes are refused while an export is running and once the batch has
committed any output. After the first successful export, the destination is
fixed for that job and the UI says a new batch is required to change it. This
keeps a resumed job writing to the folder its completed outputs already used.

### Progress semantics

The batch size is known, so the progress bar is determinate:
`processed / total`, with `processed` counting done, skipped, failed and
canceled items because all four are terminal. The sidebar shows a whole-number
percentage and a count (`41% · 68 of 162 processed`), the active photo and its
stage (`IMG_3128.CR2 — Extracting the camera JPEG…`), and the success count
separately. Between files, and when paused or canceling, it shows
`Preparing…`, `Paused` or `Canceling…` instead. RawDrop never fabricates a
within-file percentage, because ExifTool and `jpegtran` cannot report per-file
progress. The active-file line replaces the old startup text as soon as work
begins.
