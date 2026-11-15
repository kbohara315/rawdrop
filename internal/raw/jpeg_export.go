package raw

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
)

// exportJPEG processes an already-rendered JPEG without changing its source.
// Orientation and lossless size optimization preserve JPEG coefficients;
// size-limited compression is an explicit lossy option.
func exportJPEG(ctx context.Context, src, tmpPath string, opts Options) (Result, error) {
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
	if _, err := os.Lstat(tmpPath); err == nil {
		return Result{}, fmt.Errorf("raw: temp path %s already exists", tmpPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Result{}, err
	}

	info, err := inspectJPEG(ctx, src)
	if err != nil {
		return Result{}, err
	}
	in, err := os.Open(src)
	if err != nil {
		return Result{}, err
	}
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		in.Close()
		return Result{}, fmt.Errorf("raw: create temp file: %w", err)
	}
	keep := false
	defer func() {
		in.Close()
		f.Close()
		if !keep {
			os.Remove(tmpPath)
		}
	}()
	if _, err := io.Copy(f, in); err != nil {
		return Result{}, fmt.Errorf("raw: copy JPEG source: %w", err)
	}
	if err := f.Sync(); err != nil {
		return Result{}, fmt.Errorf("raw: flush copied JPEG: %w", err)
	}
	if err := in.Close(); err != nil {
		return Result{}, fmt.Errorf("raw: close JPEG source: %w", err)
	}
	if err := f.Close(); err != nil {
		return Result{}, fmt.Errorf("raw: close temp JPEG: %w", err)
	}

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
	expW, expH := info.Width, info.Height
	if rotated && swapsDims(info.Orientation) {
		expW, expH = info.Height, info.Width
	}
	bin, err := exiftoolPath()
	if err != nil {
		return Result{}, err
	}
	final := finalOrientation(info.Orientation, rotated)
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
