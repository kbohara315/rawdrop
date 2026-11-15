// Package raw is RawDrop's photo-processing core. It inspects RAW and JPEG
// files, extracts the camera's embedded full-resolution JPEG from RAW files,
// applies lossless optimization and orientation correction, and optionally
// recompresses outputs to a requested size limit.
//
// The package shells out to ExifTool, jpegtran, cjpeg, and djpeg as needed.
// Helpers are found with exec.LookPath and can be overridden with environment
// variables. External processes run under the caller's context.
//
// Source files are opened read-only and are never modified, renamed or
// deleted. Export writes to the caller-provided temporary path.
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

// Info describes a RAW or JPEG photo.
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
	// MaxOutputBytes optionally enables lossy JPEG recompression. A zero value
	// keeps the full-quality JPEG after lossless optimization.
	MaxOutputBytes int64
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
	// Compressed reports whether lossy recompression was needed to meet the
	// requested MaxOutputBytes limit.
	Compressed bool
	// Quality is the libjpeg quality setting used when Compressed is true.
	Quality int
	// SizeBytes is the final encoded JPEG size.
	SizeBytes int64
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

// Inspect reads a RAW or JPEG photo's metadata without extracting embedded
// image data. For RAW files it also inspects full-resolution JPEG candidates.
func Inspect(ctx context.Context, src string) (Info, error) {
	if isJPEGPath(src) {
		return inspectJPEG(ctx, src)
	}
	insp, err := inspectFile(ctx, src)
	if err != nil {
		return Info{}, err
	}
	return insp.info, nil
}
