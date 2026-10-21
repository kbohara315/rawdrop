package raw

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"rawdrop/internal/exiftool"
)

// Environment variables that override helper discovery, for packaged apps or
// tests that ship their own binaries.
const (
	envExifTool = exiftool.EnvExifTool
	envJpegTran = "RAWDROP_JPEGTRAN"
)

// lookTool resolves a helper binary: the environment override first, then the
// usual executable search path.
func lookTool(envName, name string) (string, error) { return exiftool.Discover(envName, name) }

// exiftoolPath resolves the ExifTool binary. All ExifTool execution goes
// through the shared internal/exiftool runner.
func exiftoolPath() (string, error) { return exiftool.Path() }

// jpegtranPath resolves the jpegtran binary. jpegtran is specific to raw's
// lossless rotation, so only its discovery and rotation arguments live here.
func jpegtranPath() (string, error) { return lookTool(envJpegTran, "jpegtran") }

// runTool executes a helper with stdout streamed to w, so a multi-megabyte
// extraction never lands in memory.
func runTool(ctx context.Context, bin string, args []string, stdout io.Writer) error {
	return exiftool.New(bin).Stream(ctx, args, stdout)
}

// captureTool runs a helper and returns its standard output, which is used by
// integration tests that drive a real helper binary. The inspection and export
// paths use the Runner carried by Inspection instead, so tests can substitute
// a fake.
func captureTool(ctx context.Context, bin string, args []string) ([]byte, error) {
	return exiftool.New(bin).Capture(ctx, args, 0)
}

// absolutePath resolves a user-supplied path. Helper binaries treat
// arguments starting with "-" as switches, so callers pass resolved paths on
// to the helpers.
func absolutePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("raw: resolve path %q: %w", path, err)
	}
	return abs, nil
}
