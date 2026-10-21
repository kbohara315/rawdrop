# RawDrop — Desktop App Implementation Plan

Status: Planning only; application code has not been created.

## 1. Goal

Build a local desktop app using **MyGo native UI and Go**. Users drop RAW photos or a folder into the app, choose an output location, and export full-resolution, mobile-friendly JPEGs that display upright.

Start with Ubuntu/Linux and Canon `.CR2`. Keep the processing core portable for future Windows and macOS releases.

### Core user journey

1. Drop files or a folder, or use **Add Photos / Add Folder**.
2. Review a thumbnail grid and supported/skipped file counts.
3. Confirm **JPEG — Full resolution, no additional compression loss**.
4. Keep **Correct orientation** enabled; optionally preserve capture information.
5. Choose an output folder, defaulting to a new `JPEG - Full Resolution` folder beneath a single source folder. For mixed source locations, require an explicit destination.
6. Click **Export** and see per-file progress, failures, and pause/cancel controls.
7. Open the output folder or retry failed items.
8. After a crash or restart, resume a saved job without repeating verified work.

Dropping files prepares a job; it does not immediately write files. Folder watching is not part of the MVP.

## 2. Quality and safety contract

- Never modify, rename, move, or delete source photos.
- JPEG is inherently lossy. Promise **no additional compression loss**, not lossless RAW-to-JPEG conversion.
- Extract an existing full-resolution camera JPEG without resizing or re-encoding it.
- Compare the embedded JPEG's dimensions against the camera's usable full-image dimensions. Do not assume every camera's raw sensor dimensions equal its rendered dimensions.
- If no full-resolution embedded JPEG is available, skip with a clear explanation. Never silently export a small preview or switch to RAW rendering.
- Preserve camera orientation using EXIF metadata by default. This fixes display in orientation-aware viewers without changing JPEG image data.
- Apply manual rotation as an explicit user override, not through image-content guessing.
- Preserve supported capture dates and selected camera/exposure metadata. Do not copy all MakerNotes or offset tags indiscriminately.
- Do not copy GPS metadata by default; provide a separate opt-in if GPS preservation is added.
- Preserve embedded color profiles when present. If color interpretation is unknown, warn rather than falsely label the file sRGB.
- Never overwrite an unrelated existing output. Create a unique name or ask the user to resolve the collision.
- Keep processing offline. No accounts, uploads, telemetry, or cloud services in the MVP.

## 3. Architecture

```text
+------------------------------------------------------------+
| MyGo Native UI                                             |
| Drop zone / Folder picker / Thumbnail grid / Export settings |
| Progress / Pause / Resume / Cancel / Results                 |
+----------------------------+-------------------------------+
                             | commands       ^ snapshots
                             v                |
+------------------------------------------------------------+
| Go Job Controller                                          |
| Planning / Bounded workers / Cancellation / Recovery        |
+--------------+----------------------------+----------------+
               |                            |
               v                            v
+----------------------------+  +----------------------------+
| Processing Pipeline        |  | Local Job Store (JSON)     |
|                            |  | Settings / Input identity  |
| Scan and deduplicate       |  | Item state / Output hashes |
|          |                 |  | Errors / Crash recovery    |
| Read metadata: ExifTool    |  +----------------------------+
|          |                 |
| Check full-size JPEG       |------> Unsupported: skip + reason
|          |                 |
| Extract JPEG: ExifTool     |
|          |                 |
| Write EXIF orientation     |
| and selected metadata      |
|          |                 |
| Verify temporary output    |
|          |                 |
| Commit without overwriting |
+--------------+-------------+
               |
               v
+------------------------------------------------------------+
| New Output Folder                                          |
| IMG_3128.jpg / IMG_3129.jpg / ...                            |
| Originals remain untouched                                 |
+------------------------------------------------------------+
```

### Technology choices

| Concern | Choice |
| --- | --- |
| Desktop framework | MyGo native template; no HTML frontend or webview |
| Language | Go; pin a version supported by the selected MyGo release |
| RAW metadata and embedded JPEG extraction | ExifTool behind a Go adapter |
| JPEG inspection and thumbnails | Go image/jpeg and a vetted EXIF reader; apply orientation to preview rendering |
| Background work | Goroutines, bounded channels, context cancellation |
| Persistence | Versioned JSON job records in the OS application-data directory |
| Initial distribution | Ubuntu `.deb` using MyGo packaging |
| Optional later physical rotation | libjpeg-turbo `jpegtran` with `-perfect` |

The inspected MyGo documentation currently specifies Go 1.27+ and MyGo 0.6.2. Recheck and pin compatible versions during setup; do not rely on an unpinned `latest` in repeatable builds.

## 4. Proposed project structure

This is the intended layout, not files already implemented.

```text
rawdrop/
  IMPLEMENTATION_PLAN.md
  main.go                     # App startup and dependency wiring
  go.mod
  go.sum
  mygo.json                   # Native MyGo app configuration
  internal/
    domain/                   # Job, item, settings, status, error types
    app/                      # Scan/export/resume orchestration
    scanner/                  # Filesystem traversal and deduplication
    metadata/                 # ExifTool adapter and tag normalization
    exporter/                 # Extraction and metadata-writing pipeline
    verifier/                 # Dimensions, compressed-data integrity, decoding
    storage/                  # Versioned job journal and settings
    filesystem/               # Temp files, safe commits, naming, disk checks
    thumbnail/                # Bounded orientation-aware thumbnail cache
    desktop/                  # MyGo views, state, dialogs, progress updates
    tools/                    # Helper discovery, version checks, process runner
  resources/
    icon.png
    licenses/                 # Notices for redistributed dependencies
  testdata/                   # Small synthetic/licensed test fixtures
```

Keep the processing core independent of MyGo. Define narrow interfaces for metadata extraction, safe output writing, and job persistence so tests can use fakes.

## 5. Processing and recovery design

### Scan and plan

- Accept multiple local files and directories; reject remote URLs.
- Recursively scan folders without following directory symlinks by default.
- Deduplicate overlapping drops and repeated file paths.
- Exclude the chosen output folder and app-owned temporary files.
- Recognize extensions case-insensitively, then validate actual content through metadata inspection.
- Record unsupported items, such as videos, as skipped rather than failed exports.
- Resolve naming collisions before processing. For multi-folder input, use deterministic suffixes for duplicate basenames.
- Preflight tool availability, source readability, destination permissions, and estimated disk capacity.

### Export one item

1. Record source identity and hash; open source read-only.
2. Inspect metadata and identify the correct embedded full-resolution JPEG tag. Different formats can use different tags; do not hardcode `PreviewImage` for every RAW type.
3. Extract to a uniquely named temporary file on the destination filesystem.
4. Confirm JPEG dimensions and record its compressed-image-data fingerprint.
5. Write normalized orientation, supported selected metadata, and any existing embedded color profile to the temporary JPEG.
6. Verify decoding, dimensions, orientation, and unchanged JPEG coding tables/image scans. Hashing scans alone is insufficient if coding tables change.
7. Check that the source was not changed during processing; initial integration tests also compare source hashes before and after export.
8. Flush and commit with a platform-safe **no-replace** operation; a normal rename must not overwrite an existing destination.
9. Save output hash and completed status to the job journal.

Metadata-copy warnings are distinct from extraction failures. Missing optional capture information can produce a warning; invalid orientation or unknown image integrity must prevent successful completion.

### Job state

```text
Job:  scanning -> ready -> running -> completed / completed_with_errors
                            |  ^
                            v  |
                           paused
                            |
                         canceled

Item: queued -> inspecting -> extracting -> metadata -> verifying -> completed
                   |              |            |           |
                   +--------------+------------+-----------+--> failed
                   |
                   +--> skipped (unsupported or no full-size JPEG)
```

- Pause stops scheduling new items; an active item may finish safely.
- Cancel stops scheduling, cancels active helper processes, and removes only job-owned incomplete temporary files. Preserve completed exports.
- Use per-operation timeouts and cancellation, not one short timeout for the whole batch.
- Begin with one worker and batched metadata reads. Increase to at most two export workers after measuring memory and disk behavior.
- Persist journal updates atomically through a single writer; include schema version, settings, helper versions, input fingerprints, item states, destination paths, and verified output hashes.
- After a restart, return unfinished items to queued. Trust completed items only after checking their source/settings identity and output hash.
- Reconcile the crash window between output commit and journal commit: verify the existing candidate instead of overwriting it or blindly duplicating it.
- Allow only one app instance to own a resumable job at a time.

## 6. UI plan

### Empty state

- Large **Drop RAW photos or a folder here** target.
- **Add Photos** and **Add Folder** buttons as keyboard-accessible alternatives.
- Brief statement: **Originals stay untouched. Everything runs on your computer.**

### Ready state

- Virtualized thumbnail grid with filename, selected state, and eligibility badge.
- Output-location picker and estimated output size.
- Format fixed to JPEG in MVP; do not present unsupported formats as working options.
- Correct orientation enabled by default; selected capture metadata optional.
- Clearly distinguish **full-resolution extraction** from an unsupported conversion.
- **Export N Photos** action.

### Running and results

- Completed/total count, current stage, per-file errors, and progress indicator.
- Pause/resume/cancel; no image decoding or helper execution on the UI thread.
- Publish immutable/coalesced state snapshots through MyGo's documented UI-thread update mechanism.
- Results summary: exported, skipped, failed, warnings; **Open Output Folder** and **Retry Failed** actions.
- On next launch, offer to resume unfinished jobs.

## 7. Implementation milestones

### Phase 0 — Feasibility and dependency spike

- Confirm the pinned MyGo native template builds and opens on Ubuntu under Wayland and X11.
- Verify external file **and folder** drops from Ubuntu Files. Internal UI drag/drop is not sufficient; test MyGo's native dropped-file/system transfer APIs.
- Use a small, explicitly selected sample of Canon EOS 80D `.CR2` files to verify full-resolution extraction and camera orientation.
- Establish ExifTool tag normalization, color-profile behavior, helper invocation, and warning handling.
- Confirm the Linux dependency/package approach before committing to distribution promises.

**Acceptance:** Drop a folder, identify valid photos, extract one full-size upright JPEG to a new destination, and prove the original and JPEG image data remain unchanged.

### Phase 1 — Headless export vertical slice

- Scaffold the native project into the existing `rawdrop` folder without overwriting this plan; use temporary scaffolding if the generator requires an empty destination.
- Implement the domain model, tool adapter, metadata reader, verifier, and no-replace writer.
- Build a testable one-file export path before adding batch UI.
- Add tests alongside each processing component.

**Acceptance:** A portrait and a landscape CR2 export correctly; missing previews, existing output files, and corrupt inputs fail safely with useful messages.

### Phase 2 — Drag-and-drop batch application

- Add native drop/picker inputs, scanning, deduplication, settings, preview grid, and export action.
- Wire background processing to responsive UI progress.
- Add cancel and pause-at-item-boundary behavior.
- Ensure thumbnail generation is bounded and never overwrites full-resolution exports.

**Acceptance:** Dropping mixed files and overlapping folders creates one deduplicated job; exporting a folder produces full-resolution upright JPEGs while the UI remains responsive.

### Phase 3 — Persistence and interruption recovery

- Add the versioned job journal, lock ownership, output hashes, retry, and restart reconciliation.
- Persist per-file progress rather than waiting until the entire batch finishes.
- Handle missing sources, changed settings, changed files, and externally modified outputs explicitly.

**Acceptance:** Terminate the app mid-export and relaunch; verified completed files are not re-exported, unfinished files resume safely, and no original or unrelated file is changed.

### Phase 4 — Packaging and release validation

- Configure MyGo's native-only Linux package without a WebKit dependency.
- For the Ubuntu MVP, declare ExifTool as a package dependency (`libimage-exiftool-perl`) and verify the generated package's dependency metadata.
- Do not run sudo or silently install packages from the application. Show actionable diagnostics if launched without required helpers.
- Include desktop entry, icon, version, dependency checks, and third-party notices.
- Test install, launch, export, resume, and uninstall on a clean Ubuntu environment.

**Acceptance:** The `.deb` installs with dependencies through the package manager and performs the full workflow without a developer toolchain or Python installation.

### Later — Explicitly outside MVP

- Physical lossless JPEG rotation via `jpegtran -perfect`, resetting EXIF orientation only after success. On incompatible JPEG block dimensions, retain metadata-only rotation or ask the user; never silently crop or recompress.
- Additional RAW formats after adding camera-specific fixtures and dimension/tag rules.
- High-quality RAW rendering, JPEG re-encoding, PNG/TIFF/WebP output, resizing, and color-space conversion as separately labeled modes with explicit quality tradeoffs.
- Windows/macOS packaging, bundled platform-specific helpers/runtimes, and signing. ExifTool is not a universal standalone binary; resolve its runtime and redistribution needs per platform.
- Folder watching, signed auto-updates, and optional drag-to-auto-export presets.

## 8. Test and release checklist

- [ ] Portrait and landscape photos display upright in the app, Ubuntu Files, and a mobile image viewer.
- [ ] All EXIF orientation values 1–8 have unit fixtures, including mirrored cases.
- [ ] Existing full-resolution JPEG bytes are extracted without re-encoding; verification covers coding tables and compressed image scans.
- [ ] Metadata-only changes do not alter image data; orientation metadata matches the source or explicit override.
- [ ] Source hashes are unchanged after integration exports.
- [ ] Missing/low-resolution previews are skipped with an explanation.
- [ ] Corrupt/truncated files, unknown color metadata, helper warnings, and tool failures are surfaced appropriately.
- [ ] Spaces, Unicode filenames, uppercase extensions, duplicate names, overlapping drops, and nested output folders work safely.
- [ ] Permission failures, destination races, symlinks, disk-full errors, cancellation, and crashes do not overwrite unrelated files.
- [ ] Repeated restarts correctly resume jobs, including interruption immediately after output commit.
- [ ] Long batches are not constrained by a fixed short total timeout; UI and memory usage remain bounded.
- [ ] Exported metadata does not include GPS unless deliberately requested.
- [ ] Automated tests use synthetic/licensed fixtures; personal photos are never committed to the project.
- [ ] A real-folder acceptance run, only with permission, handles the 162-photo Dashain sample without modifying its RAW originals or existing JPEG exports.
- [ ] Clean Ubuntu installation verifies runtime dependencies and desktop integration.

## 9. Definition of done

A user can install RawDrop on Ubuntu, drop a folder, export eligible photos as full-resolution upright JPEGs into a new folder, inspect clear skip/error reasons, and recover after interruption without losing work or changing originals.

## 10. Reference documentation

Consult current APIs during implementation; this plan intentionally avoids guessing framework method signatures.

- MyGo setup and native template: https://mygo.egoist.dev/docs/getting-started
- MyGo native UI: https://mygo.egoist.dev/docs/ui
- MyGo native drag/drop: https://mygo.egoist.dev/docs/ui/drag-and-drop
- MyGo dropped files: https://mygo.egoist.dev/docs/ui/input#dropped-files
- MyGo UI state/threading: https://mygo.egoist.dev/docs/ui/views
- MyGo packaging and helper resources: https://mygo.egoist.dev/docs/distribution
- ExifTool: https://exiftool.org/
- libjpeg-turbo: https://libjpeg-turbo.org/
