package thumb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	// maxToolOutput bounds how much one exiftool extraction may buffer.
	maxToolOutput = 64 << 20
	// maxToolError bounds the stderr kept for error messages.
	maxToolError = 8 << 10
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
// derived from ctx. RAWDROP_EXIFTOOL overrides the binary so tests and
// packaged installs can point at a helper.
func runExiftool(ctx context.Context, limit int, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bin := os.Getenv("RAWDROP_EXIFTOOL")
	if bin == "" {
		bin = "exiftool"
	}
	toolCtx, cancel := ctx, context.CancelFunc(func() {})
	if _, ok := ctx.Deadline(); !ok {
		toolCtx, cancel = context.WithTimeout(ctx, defaultToolTimeout)
	}
	defer cancel()

	cmd := exec.CommandContext(toolCtx, bin, args...)
	var stdout, stderr limitedBuffer
	stdout.limit = limit
	stderr.limit = maxToolError
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if toolCtx.Err() != nil {
			return nil, fmt.Errorf("thumb: exiftool: %w", toolCtx.Err())
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			message := strings.TrimSpace(stderr.String())
			if message == "" {
				message = err.Error()
			}
			return nil, fmt.Errorf("thumb: exiftool: %s", message)
		}
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("thumb: exiftool not found (%s); install exiftool or set RAWDROP_EXIFTOOL", bin)
		}
		return nil, fmt.Errorf("thumb: run exiftool: %w", err)
	}
	if stdout.truncated {
		return nil, fmt.Errorf("thumb: exiftool output exceeded %d bytes", limit)
	}
	return stdout.Bytes(), nil
}

// limitedBuffer collects at most limit bytes and discards the rest, so a
// misbehaving tool cannot fill memory. Write always reports success so the
// child keeps draining instead of blocking on a full pipe.
type limitedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) <= room {
			b.buf.Write(p)
		} else {
			b.buf.Write(p[:room])
			b.truncated = true
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return len(p), nil
}

func (b *limitedBuffer) Bytes() []byte { return b.buf.Bytes() }

func (b *limitedBuffer) String() string { return b.buf.String() }
