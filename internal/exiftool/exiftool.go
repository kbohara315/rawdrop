// Package exiftool is RawDrop's low-level adapter for helper processes. It
// owns binary discovery, bounded subprocess execution, and the wording of the
// errors those helpers produce. It deliberately knows nothing about RAW
// formats or ExifTool's tags: callers pass arguments and interpret the bytes.
//
// The Runner interface is the seam. Production uses a Binary, which executes a
// resolved helper path; tests can supply any Runner.
package exiftool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	// EnvExifTool overrides exiftool discovery, for packaged installs or tests
	// that ship their own binary.
	EnvExifTool = "RAWDROP_EXIFTOOL"
	// MaxStderr bounds how much helper stderr is kept for error messages.
	MaxStderr = 4 << 10
)

// ErrOutputLimit reports that a helper produced more output than the caller
// allowed. Callers can test for it with errors.Is.
var ErrOutputLimit = errors.New("helper output limit exceeded")

// Discover resolves a helper binary: the environment override named by
// envName first, then the usual executable search path.
func Discover(envName, name string) (string, error) {
	if override := strings.TrimSpace(os.Getenv(envName)); override != "" {
		if _, err := exec.LookPath(override); err != nil {
			return "", fmt.Errorf("required tool %q from %s is not executable: %w", override, envName, err)
		}
		return override, nil
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("required tool %q not found in PATH (set %s to override): %w", name, envName, err)
	}
	return path, nil
}

// Path resolves the exiftool binary, honoring RAWDROP_EXIFTOOL.
func Path() (string, error) { return Discover(EnvExifTool, "exiftool") }

// Runner runs one helper binary. Each call is a separate process; arguments
// are passed directly and no shell is involved.
type Runner interface {
	// Stream runs the helper and streams its standard output to w, so a
	// multi-megabyte extraction never lands in memory. A canceled context
	// stops the helper and surfaces as an error wrapping context.Canceled.
	Stream(ctx context.Context, args []string, stdout io.Writer) error
	// Capture runs the helper and returns its standard output. A positive
	// limit bounds the buffered output; exceeding it fails with ErrOutputLimit.
	// A limit of zero or less means unbounded.
	Capture(ctx context.Context, args []string, limit int64) ([]byte, error)
	// Prefix runs the helper and returns at most limit bytes of its standard
	// output, then stops it. It is for peeking at the start of a large tag.
	Prefix(ctx context.Context, args []string, limit int64) ([]byte, error)
}

// Binary runs one resolved helper binary. It is the production Runner.
type Binary struct {
	path string
	name string
}

// New returns a Runner for the helper at path. The error text names the
// helper by its base name.
func New(path string) *Binary {
	return &Binary{path: path, name: filepath.Base(path)}
}

// Stream runs the helper with stdout streamed to w and stderr captured up to
// MaxStderr bytes.
func (b *Binary) Stream(ctx context.Context, args []string, stdout io.Writer) error {
	cmd := exec.CommandContext(ctx, b.path, args...)
	cmd.Stdout = stdout
	stderr := cappedBuffer{max: MaxStderr}
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil && ctx.Err() != nil {
		// The helper was killed by context cancellation; report that instead
		// of an opaque signal error so errors.Is(err, context.Canceled) works.
		return fmt.Errorf("%s canceled: %w", b.name, ctx.Err())
	}
	if err != nil {
		return toolError(b.name, err, stderr.String())
	}
	return nil
}

// Capture runs the helper and returns its standard output, which is used for
// small metadata responses and bounded image extractions.
func (b *Binary) Capture(ctx context.Context, args []string, limit int64) ([]byte, error) {
	var out limitedBuffer
	out.limit = limit
	if err := b.Stream(ctx, args, &out); err != nil {
		return nil, err
	}
	if out.truncated {
		return nil, fmt.Errorf("%s: output exceeded %d bytes: %w", b.name, limit, ErrOutputLimit)
	}
	return out.Bytes(), nil
}

// Prefix runs the helper and returns at most limit bytes of its standard
// output, then stops it. Bounded like this, a caller can read the header of
// an embedded JPEG without extracting the whole thing to disk or memory.
func (b *Binary) Prefix(ctx context.Context, args []string, limit int64) ([]byte, error) {
	cmd := exec.CommandContext(ctx, b.path, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := cappedBuffer{max: MaxStderr}
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(stdout, limit))
	// Close the pipe first: a helper still writing gets EPIPE and exits, and
	// Kill covers the rest so Wait cannot block on a full pipe.
	stdout.Close()
	if cmd.Process != nil {
		cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%s canceled: %w", b.name, ctx.Err())
	}
	if readErr != nil {
		return nil, fmt.Errorf("%s: %w", b.name, readErr)
	}
	if len(data) == 0 {
		if waitErr != nil {
			return nil, toolError(b.name, waitErr, stderr.String())
		}
		return nil, fmt.Errorf("%s: no data for the requested tag", b.name)
	}
	return data, nil
}

// toolError shapes a command failure, appending the bounded stderr when there
// is any.
func toolError(name string, err error, stderr string) error {
	if stderr == "" {
		return fmt.Errorf("%s: %w", name, err)
	}
	return fmt.Errorf("%s: %w: %s", name, err, stderr)
}

// cappedBuffer keeps at most max bytes; further writes are dropped but
// reported as accepted so helper processes never block on stderr.
type cappedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
			b.truncated = true
		} else {
			b.buf.Write(p)
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string {
	s := strings.TrimSpace(b.buf.String())
	if b.truncated {
		s += " …(truncated)"
	}
	return s
}

// limitedBuffer collects at most limit bytes and discards the rest, so a
// misbehaving tool cannot fill memory. Write always reports success so the
// child keeps draining instead of blocking on a full pipe.
type limitedBuffer struct {
	buf       bytes.Buffer
	limit     int64
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		b.buf.Write(p)
		return len(p), nil
	}
	if room := int(b.limit) - b.buf.Len(); room > 0 {
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
