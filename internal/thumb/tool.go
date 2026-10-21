package thumb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"rawdrop/internal/exiftool"
)

const (
	// maxToolOutput bounds how much one exiftool extraction may buffer.
	maxToolOutput = 64 << 20
	// defaultToolTimeout bounds one exiftool run when ctx has no deadline.
	defaultToolTimeout = 30 * time.Second
)

// toolExtract returns the raw bytes of one embedded image tag. It is a
// variable so tests can replace the exiftool invocation.
var toolExtract = extractExiftool

// toolOrientation returns the source orientation (1-8) as reported by
// exiftool. It is a variable so tests can replace the exiftool invocation.
var toolOrientation = orientationExiftool

// extractExiftool returns the bytes of tag from source using
// `exiftool -b`.
func extractExiftool(ctx context.Context, source, tag string) ([]byte, error) {
	return runExiftool(ctx, maxToolOutput, "-b", "-"+tag, "--", source)
}

// orientationExiftool asks exiftool for the numeric Orientation value. It
// runs from bounded output just like the image extraction.
func orientationExiftool(ctx context.Context, source string) (int, error) {
	out, err := runExiftool(ctx, 256, "-n", "-s3", "-Orientation", "--", source)
	if err != nil {
		return 0, err
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("thumb: parse exiftool orientation %q: %w", strings.TrimSpace(string(out)), err)
	}
	if value < 1 || value > 8 {
		return 0, fmt.Errorf("thumb: exiftool returned orientation %d", value)
	}
	return value, nil
}

// runExiftool runs exiftool with bounded stdout and stderr and a timeout
// derived from ctx, through the shared internal/exiftool runner.
// RAWDROP_EXIFTOOL overrides the binary so tests and packaged installs can
// point at a helper.
func runExiftool(ctx context.Context, limit int, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bin := os.Getenv(exiftool.EnvExifTool)
	if bin == "" {
		bin = "exiftool"
	}
	toolCtx, cancel := ctx, context.CancelFunc(func() {})
	if _, ok := ctx.Deadline(); !ok {
		toolCtx, cancel = context.WithTimeout(ctx, defaultToolTimeout)
	}
	defer cancel()

	out, err := exiftool.New(bin).Capture(toolCtx, args, int64(limit))
	if err == nil {
		return out, nil
	}
	if toolCtx.Err() != nil {
		return nil, fmt.Errorf("thumb: exiftool: %w", toolCtx.Err())
	}
	if errors.Is(err, exiftool.ErrOutputLimit) {
		return nil, fmt.Errorf("thumb: %s", err)
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return nil, fmt.Errorf("thumb: %s", err)
	}
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("thumb: exiftool not found (%s); install exiftool or set %s", bin, exiftool.EnvExifTool)
	}
	return nil, fmt.Errorf("thumb: run exiftool: %w", err)
}
