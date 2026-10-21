package raw

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"rawdrop/internal/exiftool"
)

// Export inspects src and writes a complete, verified JPEG for it to tmpPath.
// tmpPath must not exist; Export fails rather than replacing anything. On
// success the caller owns tmpPath and commits it to its final name; on any
// failure Export removes it. src is only ever read.
//
// Export is a convenience wrapper around InspectFile and Inspection.Export.
// Callers that have already inspected a file should reuse the Inspection
// handle and avoid a second ExifTool read.
//
// It extracts the best embedded JPEG tag (PreviewImage, then JpgFromRaw, then
// OtherImage), rejects candidates that are not at least 90% of the camera's
// usable displayed size, strips location metadata unless opts.PreserveLocation
// is set, and rotates the JPEG losslessly when opts.CorrectOrientation asks for
// it and the pixels allow it. When there is no full-resolution embedded JPEG it
// returns an error wrapping ErrNoFullJPEG.
func Export(ctx context.Context, src, tmpPath string, opts Options) (Result, error) {
	insp, err := InspectFile(ctx, src)
	if err != nil {
		return Result{}, err
	}
	return insp.Export(ctx, tmpPath, opts)
}

// Info returns the summary of the inspected file. The result is a value copy,
// so callers cannot mutate the handle through it.
func (in *Inspection) Info() Info { return in.info }

// Export writes a complete, verified JPEG for the inspected source to tmpPath,
// using the candidates already gathered by InspectFile. It has the same
// contract as the package-level Export.
func (in *Inspection) Export(ctx context.Context, tmpPath string, opts Options) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	runner := in.runner
	if runner == nil {
		return Result{}, errors.New("raw: inspection has no helper runner")
	}
	src := in.source
	if src == "" {
		return Result{}, errors.New("raw: inspection has no source path")
	}
	// Refuse stale candidate offsets if the source changed after inspection.
	if err := in.checkSource(); err != nil {
		return Result{}, err
	}
	tmpPath, err := absolutePath(tmpPath)
	if err != nil {
		return Result{}, err
	}
	if src == tmpPath {
		return Result{}, errors.New("raw: temp path must differ from the source path")
	}
	// Fail fast when the temp name is taken; the O_EXCL create below is still
	// the authority in case of a race.
	if _, err := os.Lstat(tmpPath); err == nil {
		return Result{}, fmt.Errorf("raw: temp path %s already exists", tmpPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Result{}, err
	}

	info := in.info

	// Reserve the temp name with O_EXCL so Export can never touch an existing
	// file. The deferred cleanup removes it unless the export completes.
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return Result{}, fmt.Errorf("raw: create temp file: %w", err)
	}
	keep := false
	defer func() {
		if f != nil {
			f.Close()
		}
		if !keep {
			os.Remove(tmpPath)
		}
	}()

	// Extract candidates in preference order, keeping the first one that is a
	// complete JPEG of the camera's usable size. Extraction is the first
	// stage, so report it before any candidate is written.
	report(opts, StageExtracting)
	var (
		width, height int
		reasons       []string
	)
	for _, c := range in.candidates {
		if err := extractCandidate(ctx, runner, src, f, c); err != nil {
			if ctx.Err() != nil {
				// A canceled extraction must surface as context.Canceled
				// rather than as an exhausted candidate list.
				return Result{}, err
			}
			reasons = append(reasons, fmt.Sprintf("%s: %v", c.tag, err))
			continue
		}
		if err := f.Sync(); err != nil {
			return Result{}, fmt.Errorf("raw: flush temp file: %w", err)
		}
		w, h, err := validateJPEGFile(tmpPath)
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("%s: %v", c.tag, err))
			continue
		}
		if info.Width <= 0 || info.Height <= 0 {
			reasons = append(reasons, fmt.Sprintf("%s: %dx%d cannot be verified because the camera's dimensions are unknown", c.tag, w, h))
			continue
		}
		if !isFullSize(w, h, info) {
			reasons = append(reasons, fmt.Sprintf("%s: %dx%d is smaller than the camera's usable size %dx%d", c.tag, w, h, info.Width, info.Height))
			continue
		}
		width, height = w, h
		break
	}
	if width == 0 {
		if len(reasons) == 0 {
			reasons = append(reasons, "no PreviewImage, JpgFromRaw or OtherImage tag is present")
		}
		return Result{}, fmt.Errorf("%w: %s: %s", ErrNoFullJPEG, filepath.Base(src), strings.Join(reasons, "; "))
	}
	// The source must not have changed while it was being read: otherwise the
	// bytes just extracted may not correspond to the inspected candidates.
	if err := in.checkSource(); err != nil {
		return Result{}, err
	}
	if err := f.Close(); err != nil {
		f = nil
		return Result{}, fmt.Errorf("raw: close temp file: %w", err)
	}
	f = nil

	rotated, cleaned, warning, err := applyMetadata(ctx, runner, tmpPath, info.Orientation, opts)
	if err != nil {
		return Result{}, err
	}

	final := finalOrientation(info.Orientation, rotated)
	expW, expH := width, height
	if rotated && swapsDims(info.Orientation) {
		expW, expH = height, width
	}
	report(opts, StageVerifying)
	if err := verifyOutput(ctx, runner, tmpPath, expW, expH, final, !opts.PreserveLocation); err != nil {
		return Result{}, err
	}

	keep = true
	return Result{
		Width:           expW,
		Height:          expH,
		Orientation:     final,
		Rotated:         rotated,
		Warning:         warning,
		MetadataCleaned: cleaned,
	}, nil
}

// report calls a caller's progress hook, if any.
func report(opts Options, stage Stage) {
	if opts.Progress != nil {
		opts.Progress(stage)
	}
}

// checkSource fails when the source changed after inspection, so Export never
// uses stale candidate offsets. A change in the file's identity, size or
// modification time invalidates the offsets.
func (in *Inspection) checkSource() error {
	if in.identity == nil {
		return nil
	}
	st, err := os.Stat(in.source)
	if err != nil {
		return fmt.Errorf("raw: re-stat source: %w", err)
	}
	if !sameSource(in.identity, st) {
		return fmt.Errorf("%w: %s", ErrSourceChanged, filepath.Base(in.source))
	}
	return nil
}

// extractCandidate streams one embedded JPEG candidate into dst, replacing
// whatever dst held. Nothing is buffered in memory beyond the file copy.
func extractCandidate(ctx context.Context, runner exiftool.Runner, src string, dst *os.File, c candidate) error {
	if err := dst.Truncate(0); err != nil {
		return fmt.Errorf("reset temp file: %w", err)
	}
	if _, err := dst.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind temp file: %w", err)
	}
	return runner.Stream(ctx, []string{"-b", "-" + c.selector(), "--", src}, dst)
}

// applyMetadata performs the metadata stages of an export on the extracted,
// verified JPEG at path: location removal, optional lossless rotation, and
// orientation normalisation. It reports whether the pixels were rotated and
// whether location metadata was cleaned; warning explains a rotation that was
// requested but could not be applied losslessly.
func applyMetadata(ctx context.Context, runner exiftool.Runner, path string, orientation int, opts Options) (rotated, cleaned bool, warning string, err error) {
	if !opts.PreserveLocation {
		if err := stripLocation(ctx, runner, path); err != nil {
			return false, false, "", fmt.Errorf("raw: remove location metadata: %w", err)
		}
		cleaned = true
	}
	if needsRotation(orientation, opts.CorrectOrientation) {
		// Report the rotation just before attempting it, so the UI never shows
		// a rotation stage while extraction is still running.
		report(opts, StageRotating)
		if rotateErr := rotateLossless(ctx, path, orientation); rotateErr != nil {
			if ctx.Err() != nil {
				return false, cleaned, "", rotateErr
			}
			warning = fmt.Sprintf("lossless rotation for orientation %d was not applied (%v); the photo is upright only through its EXIF orientation", orientation, rotateErr)
		} else {
			rotated = true
		}
	}
	final := finalOrientation(orientation, rotated)
	current, err := readOrientation(ctx, runner, path)
	if err != nil {
		return false, cleaned, "", err
	}
	if rotated || current != final {
		if err := setOrientation(ctx, runner, path, final); err != nil {
			return false, cleaned, "", fmt.Errorf("raw: set EXIF orientation: %w", err)
		}
	}
	return rotated, cleaned, warning, nil
}

// stripLocation removes GPS and geotag metadata, keeping everything else.
func stripLocation(ctx context.Context, runner exiftool.Runner, path string) error {
	args := []string{"-overwrite_original", "-gps:all=", "-xmp:geotag=", "--", path}
	_, err := runner.Capture(ctx, args, 0)
	return err
}

// setOrientation writes the EXIF orientation tag.
func setOrientation(ctx context.Context, runner exiftool.Runner, path string, orientation int) error {
	args := []string{"-overwrite_original", fmt.Sprintf("-Orientation#=%d", orientation), "--", path}
	_, err := runner.Capture(ctx, args, 0)
	return err
}

// readOrientation reads a JPEG's EXIF orientation. A missing or invalid tag
// is the EXIF default, 1.
func readOrientation(ctx context.Context, runner exiftool.Runner, path string) (int, error) {
	orientation, _, err := readOrientationAndGPS(ctx, runner, path)
	return orientation, err
}

// rotateLossless rewrites path with jpegtran's lossless DCT transform for the
// given orientation. The transform is applied only when every edge block can
// be transformed (-perfect); otherwise the extracted JPEG stays exactly as it
// was and the caller reports a warning. No crop, trim or re-encode ever
// happens here.
func rotateLossless(ctx context.Context, path string, orientation int) error {
	transform := jpegTransform(orientation)
	if transform == nil {
		return fmt.Errorf("no lossless rotation for orientation %d", orientation)
	}
	bin, err := jpegtranPath()
	if err != nil {
		return err
	}
	out, err := os.CreateTemp(filepath.Dir(path), ".rawdrop-rotate-*.jpg")
	if err != nil {
		return fmt.Errorf("create rotation temp file: %w", err)
	}
	outName := out.Name()
	out.Close()
	defer os.Remove(outName)
	args := append([]string{"-copy", "all", "-perfect"}, transform...)
	args = append(args, "-outfile", outName, path)
	if err := runTool(ctx, bin, args, io.Discard); err != nil {
		return err
	}
	if err := os.Rename(outName, path); err != nil {
		return fmt.Errorf("replace rotated file: %w", err)
	}
	return nil
}

// verifyOutput re-checks the finished temp file: dimensions, JPEG structure,
// EXIF orientation and (when required) that no location metadata remains.
func verifyOutput(ctx context.Context, runner exiftool.Runner, path string, wantW, wantH, wantOrientation int, requireNoGPS bool) error {
	w, h, err := validateJPEGFile(path)
	if err != nil {
		return fmt.Errorf("raw: verify output: %w", err)
	}
	if w != wantW || h != wantH {
		return fmt.Errorf("raw: verify output: dimensions are %dx%d, expected %dx%d", w, h, wantW, wantH)
	}
	orientation, gps, err := readOrientationAndGPS(ctx, runner, path)
	if err != nil {
		return fmt.Errorf("raw: verify output: %w", err)
	}
	if orientation != wantOrientation {
		return fmt.Errorf("raw: verify output: EXIF orientation is %d, expected %d", orientation, wantOrientation)
	}
	if requireNoGPS && len(gps) > 0 {
		return fmt.Errorf("raw: verify output: location metadata remains: %s", strings.Join(gps, ", "))
	}
	return nil
}

// readOrientationAndGPS reads the EXIF orientation and reports every location
// tag still present, using one ExifTool call.
func readOrientationAndGPS(ctx context.Context, runner exiftool.Runner, path string) (orientation int, gpsTags []string, err error) {
	selectors := append([]string{"-Orientation"}, gpsSelectors...)
	doc, err := readExifDoc(ctx, runner, path, selectors)
	if err != nil {
		return 0, nil, err
	}
	orientation = 1
	if v, _, ok := intTag(doc, "Orientation", "IFD0"); ok && v >= 1 && v <= 8 {
		orientation = v
	}
	for key := range doc {
		group, name := splitTagKey(key)
		if group == "" || name == "" {
			continue
		}
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "gps") || lower == "geotag" {
			gpsTags = append(gpsTags, key)
		}
	}
	sort.Strings(gpsTags)
	return orientation, gpsTags, nil
}

// gpsSelectors covers EXIF GPS, the XMP GPS tags and ExifTool's composite
// location tags.
var gpsSelectors = []string{
	"-GPS:all",
	"-XMP:GPSLatitude", "-XMP:GPSLongitude", "-XMP:GPSAltitude", "-XMP:GPSDateTime",
	"-Composite:GPSPosition", "-Composite:Geotag",
}
