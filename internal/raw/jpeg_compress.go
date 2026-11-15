package raw

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

// compressJPEGToLimit applies lossy recompression only when lossless output is
// larger than limit. It decodes once to a temporary PPM, then tries descending
// quality settings until the complete JPEG, including copied metadata, fits.
func compressJPEGToLimit(ctx context.Context, path string, limit int64) (int, bool, error) {
	if limit <= 0 {
		return 0, false, nil
	}
	st, err := os.Stat(path)
	if err != nil {
		return 0, false, err
	}
	if st.Size() < limit {
		return 0, false, nil
	}
	djpeg, err := djpegPath()
	if err != nil {
		return 0, false, err
	}
	cjpeg, err := cjpegPath()
	if err != nil {
		return 0, false, err
	}
	exiftool, err := exiftoolPath()
	if err != nil {
		return 0, false, err
	}

	ppm, err := os.CreateTemp(filepath.Dir(path), ".rawdrop-decode-*.ppm")
	if err != nil {
		return 0, false, fmt.Errorf("create JPEG decode temp file: %w", err)
	}
	ppmPath := ppm.Name()
	ppm.Close()
	defer os.Remove(ppmPath)
	if err := runTool(ctx, djpeg, []string{"-pnm", "-outfile", ppmPath, path}, io.Discard); err != nil {
		return 0, false, fmt.Errorf("decode JPEG for size limit: %w", err)
	}
	ppmFile, err := os.Open(ppmPath)
	if err != nil {
		return 0, false, err
	}
	var ppmMagic [2]byte
	_, readErr := io.ReadFull(ppmFile, ppmMagic[:])
	ppmFile.Close()
	if readErr != nil {
		return 0, false, fmt.Errorf("read decoded PNM header: %w", readErr)
	}

	encoded, err := os.CreateTemp(filepath.Dir(path), ".rawdrop-compress-*.jpg")
	if err != nil {
		return 0, false, fmt.Errorf("create JPEG compression temp file: %w", err)
	}
	encodedPath := encoded.Name()
	encoded.Close()
	defer os.Remove(encodedPath)

	qualities := make([]int, 0, 42)
	for q := 90; q >= 50; q -= 2 {
		qualities = append(qualities, q)
	}
	qualities = append(qualities, 45, 40, 35, 30, 25, 20)
	for _, quality := range qualities {
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
		if err := os.Remove(encodedPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, false, err
		}
		args := []string{"-quality", strconv.Itoa(quality), "-optimize", "-progressive"}
		if ppmMagic == [2]byte{'P', '6'} {
			args = append(args, "-sample", "1x1,1x1,1x1")
		}
		args = append(args, "-outfile", encodedPath, ppmPath)
		if err := runTool(ctx, cjpeg, args, io.Discard); err != nil {
			return 0, false, fmt.Errorf("re-encode JPEG at quality %d: %w", quality, err)
		}
		// The source has already had orientation and the location policy
		// applied. Copy image metadata but leave MPF offsets to the old
		// secondary JPEG behind; the compressed file is a normal JPEG frame.
		metaArgs := []string{
			"-TagsFromFile", path,
			"-EXIF:All", "-IPTC:All", "-XMP:All", "-ICC_Profile", "-Comment",
			"-overwrite_original", "--", encodedPath,
		}
		if err := runTool(ctx, exiftool, metaArgs, io.Discard); err != nil {
			return 0, false, fmt.Errorf("copy JPEG metadata: %w", err)
		}
		st, err := os.Stat(encodedPath)
		if err != nil {
			return 0, false, err
		}
		if st.Size() >= limit {
			continue
		}
		if err := os.Rename(encodedPath, path); err != nil {
			return 0, false, fmt.Errorf("replace JPEG with size-limited output: %w", err)
		}
		return quality, true, nil
	}
	return 0, false, fmt.Errorf("could not fit JPEG below %d bytes without resizing", limit)
}
