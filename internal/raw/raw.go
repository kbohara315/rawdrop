// Package raw is RawDrop's processing core. It inspects RAW files with
// ExifTool, extracts the camera's embedded full-resolution JPEG without
// re-encoding it, removes location metadata unless asked to keep it, and
// optionally rotates the JPEG physically with a lossless jpegtran DCT
// transform.
//
// The package shells out to the exiftool and jpegtran helpers. Both are found
// with exec.LookPath but may be overridden with the RAWDROP_EXIFTOOL and
// RAWDROP_JPEGTRAN environment variables. External processes always run under
// the caller's context, so canceling the context stops a running helper and
// surfaces as context.Canceled.
//
// Source files are opened read-only and are never modified, renamed or
// deleted. Export streams the embedded JPEG into the caller-provided temporary
// path and never holds image data in memory.
package raw

import (
	"context"
	"errors"
)

// ErrNoFullJPEG reports that a RAW file has no embedded JPEG large enough to
// count as the camera's full-resolution image. Callers can test for it with
// errors.Is; the error message names the candidates that were rejected and
// why.
var ErrNoFullJPEG = errors.New("no full-resolution embedded JPEG found")

// ErrSourceChanged reports that a RAW file changed between inspection and
// export, so the inspection's candidate offsets can no longer be trusted.
// Callers can test for it with errors.Is; re-inspecting is the safe response.
var ErrSourceChanged = errors.New("source changed since inspection")

// Info describes a RAW file, read without extracting its embedded JPEG.
type Info struct {
	// Width and Height are the camera's usable, rendered image dimensions as
	// reported by EXIF (for example 6000x4000 for a Canon EOS 80D). They are
	// in sensor orientation, not display orientation: for orientation 5-8 the
	// displayed image size is Height x Width. Zero means the file did not
	// report dimensions.
	Width, Height int
	// Orientation is the EXIF orientation value, 1..8. A missing or invalid
	// tag is reported as 1.
	Orientation int
	// PreviewTag names the embedded JPEG used for the preview fields below
	// ("PreviewImage", "JpgFromRaw" or "OtherImage"). When HasPreview is
	// false it names the largest candidate found, for diagnostics, or is
	// empty when none were found.
	PreviewTag string
	Make       string
	Model      string
	// HasPreview is true when at least one embedded JPEG reaches 90% of the
	// camera's usable displayed dimensions, so that Export can extract a
	// full-resolution image.
	HasPreview bool
	// PreviewWidth and PreviewHeight are the embedded JPEG's stored pixel
	// dimensions. They are zero when unknown.
	PreviewWidth, PreviewHeight int
}

// Stage identifies a coarse step of an export for progress reporting. It is
// deliberately not a percentage: the helpers cannot report per-file progress,
// so callers must not fabricate one.
type Stage string

const (
	// StageExtracting is emitted immediately before the embedded JPEG is
	// streamed into the temporary file.
	StageExtracting Stage = "extracting"
	// StageRotating is emitted immediately before a lossless rotation is
	// attempted, and only when one is actually needed.
	StageRotating Stage = "rotating"
	// StageVerifying is emitted immediately before the finished temporary file
	// is validated.
	StageVerifying Stage = "verifying"
)

// Options controls an export.
type Options struct {
	// CorrectOrientation rotates photos upright with a lossless jpegtran
	// transform when EXIF orientation is 2..8. When the transform is not
	// possible, Export keeps the image pixels and relies on EXIF orientation
	// alone, reporting a Warning.
	CorrectOrientation bool
	// PreserveLocation keeps GPS and other location tags. When false (the
	// default for a zero Options) location metadata is removed.
	PreserveLocation bool
	// Progress, when non-nil, is called at the start of each export stage. It
	// runs on the exporting goroutine and must not block. Stages are emitted
	// in real order, so a caller never sees StageRotating before
	// StageExtracting.
	Progress func(Stage)
}

// Result describes a successful export.
type Result struct {
	// Width and Height are the stored pixel dimensions of the exported JPEG.
	// After a physical rotation of an orientation 5-8 photo they are swapped
	// relative to the extracted JPEG.
	Width, Height int
	// Orientation is the EXIF orientation carried by the exported JPEG. It is
	// 1 when the pixels were physically rotated.
	Orientation int
	// Rotated is true when the JPEG pixels were losslessly rotated.
	Rotated bool
	// Warning is non-empty when the export succeeded with a caveat, most
	// commonly that a requested lossless rotation was not possible and the
	// photo is only upright because of its EXIF orientation tag.
	Warning string
	// MetadataCleaned is true when location metadata was removed.
	MetadataCleaned bool
}

// CheckTools returns an error naming every required helper that cannot be
// found. It is a preflight check for the application; Export itself only
// requires exiftool, treating a missing jpegtran as a rotation warning.
func CheckTools() error {
	var errs []error
	if _, err := exiftoolPath(); err != nil {
		errs = append(errs, err)
	}
	if _, err := jpegtranPath(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Inspect reads a RAW file's metadata without extracting the embedded JPEG.
// The returned Info reports the camera's usable dimensions, EXIF orientation,
// make and model, and the best embedded full-resolution JPEG candidate.
//
// Inspect keeps only the summary. Callers that will also export the file
// should use InspectFile and pass its handle to Export, so the file is
// inspected only once.
func Inspect(ctx context.Context, src string) (Info, error) {
	insp, err := InspectFile(ctx, src)
	if err != nil {
		return Info{}, err
	}
	return insp.Info(), nil
}
