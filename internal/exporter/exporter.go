// Package exporter performs one complete photo export: inspect a RAW source
// once, extract its embedded full-resolution JPEG, apply the metadata policy,
// verify it, and commit it under a collision-safe name without ever replacing
// an existing file.
//
// It is the shared per-photo core behind the desktop app and the headless CLI.
// It deliberately does not import MyGo, does not own a job or a journal, and
// carries no status or progress wording: callers translate Options.Progress
// into whatever they display. Source files are only ever read.
package exporter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"rawdrop/internal/fsutil"
	"rawdrop/internal/raw"
)

// Stage identifies the coarse step of an export that is currently running. It
// is intentionally not a percentage: the helpers cannot report per-file
// progress, so callers must not fabricate one.
type Stage string

const (
	// StageInspect means metadata is being read and eligibility decided.
	StageInspect Stage = "inspecting"
	// StageExtract means the embedded JPEG is being streamed to the temp file.
	StageExtract Stage = "extracting"
	// StageRotate means a lossless orientation rotation is being applied.
	StageRotate Stage = "rotating"
)

// Progress is one progress notification. Info is the source's metadata once
// inspection has completed and the zero value before.
type Progress struct {
	Stage Stage
	Info  raw.Info
}

// ProgressFunc receives progress notifications during an export. It runs on
// the exporting goroutine and must not block for long.
type ProgressFunc func(Progress)

// Options controls one export.
type Options struct {
	// OutputDir is the destination directory. It is created when missing.
	OutputDir string
	// OutputName is the preferred base name, including extension. When empty,
	// the source's base name with a .jpg extension is used.
	OutputName string
	// CorrectOrientation physically rotates photos upright when possible.
	CorrectOrientation bool
	// PreserveLocation keeps GPS and geotag metadata. Off means strip it.
	PreserveLocation bool
	// Progress, when non-nil, is called at each stage. It may call back into
	// the caller, but must not assume any lock is held.
	Progress ProgressFunc
}

// Result describes a committed export.
type Result struct {
	// Source is the source path that was passed in.
	Source string
	// OutputPath is the absolute path of the committed JPEG.
	OutputPath string
	// OutputName is the base name of the committed JPEG, including any
	// collision suffix.
	OutputName string
	// Width and Height are the stored pixel dimensions of the output.
	Width, Height int
	// Orientation is the EXIF orientation carried by the output.
	Orientation int
	// Rotated is true when the JPEG pixels were losslessly rotated.
	Rotated bool
	// Warning is non-empty when the export succeeded with a caveat.
	Warning string
	// MetadataCleaned is true when location metadata was removed.
	MetadataCleaned bool
	// SHA256 is the hexadecimal digest of the committed output.
	SHA256 string
}

// maxNameAttempts bounds the collision retry loop. A name is chosen, then the
// no-replace commit is the authority; the loop only re-tries when the name
// appeared between the two steps. A bound keeps a pathological directory from
// spinning forever.
const maxNameAttempts = 64

// Export inspects source, extracts and verifies its embedded JPEG, and commits
// it into opts.OutputDir. It never replaces an existing file: on a name
// collision it retries with a " (2)", " (3)", … suffix. The source is only
// read. On failure no visible output and no temp file is left behind.
//
// It inspects the source exactly once and reuses that inspection for the
// export, so a caller that also needs Info should read it from Progress rather
// than calling raw.Inspect again.
func Export(ctx context.Context, source string, opts Options) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(source) == "" {
		return Result{}, errors.New("exporter: no source path")
	}
	dir, err := filepath.Abs(opts.OutputDir)
	if err != nil {
		return Result{}, fmt.Errorf("exporter: resolve output directory %q: %w", opts.OutputDir, err)
	}
	dir = filepath.Clean(dir)
	name := strings.TrimSpace(opts.OutputName)
	if name == "" {
		base := filepath.Base(source)
		name = strings.TrimSuffix(base, filepath.Ext(base)) + ".jpg"
	}

	insp, err := raw.InspectFile(ctx, source)
	if err != nil {
		return Result{}, err
	}
	info := insp.Info()
	if opts.Progress != nil {
		opts.Progress(Progress{Stage: StageInspect, Info: info})
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{}, fmt.Errorf("exporter: create output folder: %w", err)
	}
	// Reserve a temp name in the destination directory so the final hard link
	// cannot cross filesystems. raw.Export requires a path that does not exist
	// yet, so the created file is removed before the export writes it.
	temp, err := os.CreateTemp(dir, ".rawdrop-*.jpg")
	if err != nil {
		return Result{}, fmt.Errorf("exporter: create temp file: %w", err)
	}
	tempPath := temp.Name()
	temp.Close()
	os.Remove(tempPath)
	defer os.Remove(tempPath)

	rawOpts := raw.Options{
		CorrectOrientation: opts.CorrectOrientation,
		PreserveLocation:   opts.PreserveLocation,
	}
	if opts.Progress != nil {
		opts.Progress(Progress{Stage: StageExtract, Info: info})
		if opts.CorrectOrientation && info.Orientation >= 2 && info.Orientation <= 8 {
			opts.Progress(Progress{Stage: StageRotate, Info: info})
		}
	}

	res, err := insp.Export(ctx, tempPath, rawOpts)
	if err != nil {
		return Result{}, err
	}

	dest, err := commit(ctx, tempPath, dir, name)
	if err != nil {
		return Result{}, err
	}
	// Make the directory entry durable. Failure is advisory on platforms that
	// do not permit directories to be opened for syncing.
	_ = fsutil.SyncDir(dir)

	// Temporary files are created 0600; a finished export should be a normal,
	// readable photo.
	if err := os.Chmod(dest, 0o644); err != nil {
		return Result{}, fmt.Errorf("exporter: set output permissions: %w", err)
	}
	hash, err := hashFile(dest)
	if err != nil {
		return Result{}, fmt.Errorf("exporter: verify output: %w", err)
	}

	return Result{
		Source:          source,
		OutputPath:      dest,
		OutputName:      filepath.Base(dest),
		Width:           res.Width,
		Height:          res.Height,
		Orientation:     res.Orientation,
		Rotated:         res.Rotated,
		Warning:         res.Warning,
		MetadataCleaned: res.MetadataCleaned,
		SHA256:          hash,
	}, nil
}

// commit links the finished temp file to a fresh name in dir without replacing
// an existing file. UniquePath only picks a candidate; CommitNew's no-replace
// link is the authority, so an EEXIST from a concurrent writer is retried with
// the next suffix. The retry is bounded.
func commit(ctx context.Context, tempPath, dir, name string) (string, error) {
	for attempt := 0; attempt < maxNameAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		dest := fsutil.UniquePath(dir, name)
		err := fsutil.CommitNew(tempPath, dest)
		if err == nil {
			return dest, nil
		}
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return "", fmt.Errorf("exporter: save output: %w", err)
	}
	return "", fmt.Errorf("exporter: could not find a free output name for %q after %d attempts", name, maxNameAttempts)
}

// hashFile returns the hexadecimal SHA-256 of path.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
