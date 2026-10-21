# Format support

RawDrop recognizes many camera RAW extensions, but recognition is not the
same as a tested, working export path. This document keeps three claims
distinct so a file extension is never mistaken for camera compatibility.

## The three claims

1. **Recognized** — the extension is accepted by the scanner and the native
   file picker. The recognized set has one definition,
   `model.rawExtensionList` in `internal/model/model.go`; both the scanner and
   the picker are derived from it. Being recognized only means "route this
   file to the exporter". It is a routing hint, not a quality promise.
2. **Exportable** — for a particular file, ExifTool can read the metadata and
   RawDrop finds an embedded JPEG that reaches the existing full-resolution
   threshold (at least 90% of the camera's usable displayed dimensions). A
   file that is recognized but not exportable is skipped with a reason; a
   preview or thumbnail is never substituted.
3. **Verified** — a representative real sample from a stated camera
   family/model has passed the full contract: inspect, full-resolution export,
   orientation, metadata policy (location removed by default), output
   validation, and byte-for-byte unchanged source. Verification is per camera
   family/model; one body does not validate a whole brand.

The 90% full-resolution threshold, the no-overwrite commit, and the
no-additional-JPEG-encode contract are unchanged by format support. RAW
demosaicing, JPEG re-encoding, and preview fallback are out of scope.

## Current support matrix

Only the Canon EOS 80D `.CR2` sample in this repository's test suite has been
verified end to end. Every other row is recognized, and exportable only when a
given file actually contains a full-resolution embedded JPEG — none of them is
verified yet.

| Extension | Family | Status | Sample make/model | Embedded candidate / dimensions | Orientation | Notes |
| --- | --- | --- | --- | --- | --- | --- |
| `.cr2` | Canon CR2 | verified | Canon EOS 80D | `IFD0:PreviewImage`, 6000×4000 | 1 and 8 | Real Canon EOS 80D samples, including the repository test suite's orientation-8 portrait and an orientation-1 landscape; landscape export is byte-identical to the embedded JPEG |
| `.cr3` | Canon CR3 | recognized | — | — | — | ISO-BMFF container; thumbnail orientation falls back to ExifTool. No real sample tested |
| `.nef` | Nikon NEF | recognized | — | — | — | Scanner test only. No real sample tested |
| `.nrw` | Nikon NRW | recognized | — | — | — | No real sample tested |
| `.arw` | Sony ARW | recognized | — | — | — | **Not verified**: no representative real Sony sample is available in this repository |
| `.srf` | Sony SRF | recognized | — | — | — | **Not verified**: no real sample |
| `.sr2` | Sony SR2 | recognized | — | — | — | **Not verified**: no real sample |
| `.dng` | Adobe/Leica/Pentax/Apple DNG | recognized | — | — | — | TIFF-derived; no real sample tested |
| `.raf` | Fujifilm RAF | recognized | — | — | — | No real sample tested |
| `.rw2` | Panasonic RW2 | recognized | — | — | — | TIFF-derived; no real sample tested |
| `.orf` | Olympus/OM ORF | recognized | — | — | — | TIFF-derived; no real sample tested |
| `.pef` | Pentax PEF | recognized | — | — | — | TIFF-derived; no real sample tested |
| `.srw` | Samsung SRW | recognized | — | — | — | No real sample tested |
| `.raw` | generic RAW | recognized | — | — | — | Vendor-ambiguous; no real sample tested |
| `.rwl` | Leica RWL | recognized | — | — | — | No real sample tested |
| `.3fr` | Hasselblad 3FR | recognized | — | — | — | No real sample tested |
| `.erf` | Epson ERF | recognized | — | — | — | No real sample tested |
| `.mef` | Mamiya MEF | recognized | — | — | — | No real sample tested |
| `.mos` | Leaf MOS | recognized | — | — | — | No real sample tested |
| `.iiq` | Phase One IIQ | recognized | — | — | — | No real sample tested |

The recognized list is checked by `TestRawExtensionRegistry` and
`TestCollectRecognizesEveryListedExtension` in `internal/scan/scan_test.go`,
including uppercase and mixed-case forms such as `.ARW`, `.SRF` and `.SR2`.

## Promoting a format to verified

Verification needs a real sample from the camera family being claimed. Samples
are never committed to the repository. Build a **curated** fixture directory
first — one or two representative samples per camera body/format, not a whole
photo library — and point the opt-in test at it:

```sh
mkdir -p ~/rawdrop-fixtures
cp "/path/to/library/IMG_3128.CR2" ~/rawdrop-fixtures/   # Canon EOS 80D
# add one or two more per camera body/format, then:
RAWDROP_TEST_FORMATS_DIR=~/rawdrop-fixtures GOTOOLCHAIN=go1.27.1 \
  go test -run TestInspectAndExportRealFormats ./internal/raw/ -v
```

The fixture directory is copied to, never exported into: each sample is opened
read-only and every output goes to the test's temp directory.

Do **not** point `RAWDROP_TEST_FORMATS_DIR` at a full library. Every sample is
exported at full resolution, so a directory of hundreds of photos runs past the
default `go test` timeout before it finishes. The harness walks recursively but
stops with a clear error once the directory exceeds `maxFormatSamples`
(currently 20), telling you to curate the fixture set — it never starts
processing a whole library by default.

`TestInspectAndExportRealFormats` walks the folder for recognized RAW files
and, for each one, checks:

- ExifTool reports a non-empty make and model;
- a full-resolution embedded JPEG is found;
- `raw.Export` produces a valid JPEG whose decoded dimensions match the
  reported result;
- the output's EXIF orientation matches the result;
- location metadata is removed by default;
- the source is byte-for-byte unchanged.

Record the observed make/model, candidate tag, dimensions and orientation in
the matrix above, mark the row **verified**, and note which camera body was
used. Do not mark a whole brand verified from a single body or a single
extension.

If a sample turns out to have no full-resolution embedded JPEG, stop and
report it: the threshold must not be lowered, and a preview must not be
exported. That format stays recognized/unverified and, if a real RAW
conversion path is ever wanted, it is a separate product decision.
