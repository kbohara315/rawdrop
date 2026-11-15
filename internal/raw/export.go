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
)

// Export writes a complete, verified JPEG for src to tmpPath. tmpPath must
// not exist; Export fails rather than replacing anything. On success the
// caller owns tmpPath and commits it to its final name; on any failure
// Export removes it. src is only ever read.
//
// Export extracts the best full-resolution embedded JPEG from RAW sources or
// copies JPEG sources directly. It strips location metadata unless
// opts.PreserveLocation is set, corrects orientation losslessly when possible,
// and uses lossless Huffman optimization. A positive MaxOutputBytes enables
// lossy recompression only when needed to meet that cap.
func Export(ctx context.Context, src, tmpPath string, opts Options) (Result, error) {
	if isJPEGPath(src) {
		return exportJPEG(ctx, src, tmpPath, opts)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	src, err := absolutePath(src)
	if err != nil {
		return Result{}, err
	}
	tmpPath, err = absolutePath(tmpPath)
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

	insp, err := inspectFile(ctx, src)
	if err != nil {
		return Result{}, err
	}
	info := insp.info

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
	// complete JPEG of the camera's usable size.
	var (
		width, height int
		reasons       []string
	)
	for _, c := range insp.candidates {
		if err := extractCandidate(ctx, src, f, c); err != nil {
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
	if err := f.Close(); err != nil {
		f = nil
		return Result{}, fmt.Errorf("raw: close temp file: %w", err)
	}
	f = nil

	rotated, cleaned, warning, err := applyMetadata(ctx, tmpPath, info.Orientation, opts)
	if err != nil {
		return Result{}, err
	}
	compressed, quality := false, 0
	if opts.MaxOutputBytes > 0 {
		quality, compressed, err = compressJPEGToLimit(ctx, tmpPath, opts.MaxOutputBytes)
		if err != nil {
			return Result{}, err
		}
	}

	final := finalOrientation(info.Orientation, rotated)
	expW, expH := width, height
	if rotated && swapsDims(info.Orientation) {
		expW, expH = height, width
	}
	bin, err := exiftoolPath()
	if err != nil {
		return Result{}, err
	}
	if err := verifyOutput(ctx, bin, tmpPath, expW, expH, final, !opts.PreserveLocation); err != nil {
		return Result{}, err
	}
	st, err := os.Stat(tmpPath)
	if err != nil {
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
		Compressed:      compressed,
		Quality:         quality,
		SizeBytes:       st.Size(),
	}, nil
}

// extractCandidate streams one embedded JPEG candidate into dst, replacing
// whatever dst held. Nothing is buffered in memory beyond the file copy.
func extractCandidate(ctx context.Context, src string, dst *os.File, c candidate) error {
	if err := dst.Truncate(0); err != nil {
		return fmt.Errorf("reset temp file: %w", err)
	}
	if _, err := dst.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind temp file: %w", err)
	}
	bin, err := exiftoolPath()
	if err != nil {
		return err
	}
	return runTool(ctx, bin, []string{"-b", "-" + c.selector(), "--", src}, dst)
}

// applyMetadata performs location removal, optional lossless rotation,
// orientation normalization, and lossless Huffman optimization on path. It
// reports whether the pixels were rotated and whether location metadata was
// cleaned; warning explains a requested operation that could not be applied.
func applyMetadata(ctx context.Context, path string, orientation int, opts Options) (rotated, cleaned bool, warning string, err error) {
	bin, err := exiftoolPath()
	if err != nil {
		return false, false, "", err
	}
	if !opts.PreserveLocation {
		if err := stripLocation(ctx, bin, path); err != nil {
			return false, false, "", fmt.Errorf("raw: remove location metadata: %w", err)
		}
		cleaned = true
	}
	if needsRotation(orientation, opts.CorrectOrientation) {
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
	current, err := readOrientation(ctx, bin, path)
	if err != nil {
		return false, cleaned, "", err
	}
	if rotated || current != final {
		if err := setOrientation(ctx, bin, path, final); err != nil {
			return false, cleaned, "", fmt.Errorf("raw: set EXIF orientation: %w", err)
		}
	}
	if !rotated {
		if err := optimizeLossless(ctx, path); err != nil {
			if ctx.Err() != nil {
				return false, cleaned, warning, err
			}
			warning = appendWarning(warning, "lossless JPEG size optimization was skipped: "+err.Error())
		}
	}
	return rotated, cleaned, warning, nil
}

func appendWarning(current, next string) string {
	if current == "" {
		return next
	}
	return current + "; " + next
}

// stripLocation removes GPS and geotag metadata, keeping everything else.
func stripLocation(ctx context.Context, bin, path string) error {
	args := []string{"-overwrite_original", "-gps:all=", "-xmp:geotag=", "--", path}
	_, err := captureTool(ctx, bin, args)
	return err
}

// setOrientation writes the EXIF orientation tag.
func setOrientation(ctx context.Context, bin, path string, orientation int) error {
	args := []string{"-overwrite_original", fmt.Sprintf("-Orientation#=%d", orientation), "--", path}
	_, err := captureTool(ctx, bin, args)
	return err
}

// readOrientation reads a JPEG's EXIF orientation. A missing or invalid tag
// is the EXIF default, 1.
func readOrientation(ctx context.Context, bin, path string) (int, error) {
	orientation, _, err := readOrientationAndGPS(ctx, bin, path)
	return orientation, err
}

// rotateLossless rewrites path with jpegtran's lossless DCT transform for the
// given orientation. The transform is applied only when every edge block can
// be transformed (-perfect); otherwise the JPEG pixels stay exactly as they
// were and the caller reports a warning. No crop, trim, or lossy image
// re-encoding happens here.
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
	args := append([]string{"-copy", "all", "-optimize", "-perfect"}, transform...)
	args = append(args, "-outfile", outName, path)
	if err := runTool(ctx, bin, args, io.Discard); err != nil {
		return err
	}
	if err := os.Rename(outName, path); err != nil {
		return fmt.Errorf("replace rotated file: %w", err)
	}
	return nil
}

// optimizeLossless rewrites JPEG entropy coding without changing any image
// coefficients. Metadata markers are copied unchanged.
func optimizeLossless(ctx context.Context, path string) error {
	bin, err := jpegtranPath()
	if err != nil {
		return err
	}
	out, err := os.CreateTemp(filepath.Dir(path), ".rawdrop-optimize-*.jpg")
	if err != nil {
		return fmt.Errorf("create optimization temp file: %w", err)
	}
	outName := out.Name()
	out.Close()
	defer os.Remove(outName)
	if err := runTool(ctx, bin, []string{"-copy", "all", "-optimize", "-outfile", outName, path}, io.Discard); err != nil {
		return err
	}
	if err := os.Rename(outName, path); err != nil {
		return fmt.Errorf("replace optimized JPEG: %w", err)
	}
	return nil
}

// verifyOutput re-checks the finished temp file: dimensions, JPEG structure,
// EXIF orientation and (when required) that no location metadata remains.
func verifyOutput(ctx context.Context, bin, path string, wantW, wantH, wantOrientation int, requireNoGPS bool) error {
	w, h, err := validateJPEGFile(path)
	if err != nil {
		return fmt.Errorf("raw: verify output: %w", err)
	}
	if w != wantW || h != wantH {
		return fmt.Errorf("raw: verify output: dimensions are %dx%d, expected %dx%d", w, h, wantW, wantH)
	}
	orientation, gps, err := readOrientationAndGPS(ctx, bin, path)
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
func readOrientationAndGPS(ctx context.Context, bin, path string) (orientation int, gpsTags []string, err error) {
	selectors := append([]string{"-Orientation"}, gpsSelectors...)
	doc, err := readExifDoc(ctx, bin, path, selectors)
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
