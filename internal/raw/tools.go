package raw

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

// Environment variables that override helper discovery, for packaged apps or
// tests that ship their own binaries.
const (
	envExifTool = "RAWDROP_EXIFTOOL"
	envJpegTran = "RAWDROP_JPEGTRAN"
	envCJPEG    = "RAWDROP_CJPEG"
	envDJPEG    = "RAWDROP_DJPEG"
)

// maxStderrBytes bounds how much helper stderr is kept for error messages.
const maxStderrBytes = 4 << 10

// lookTool resolves a helper binary: the environment override first, then the
// usual executable search path.
func lookTool(envName, name string) (string, error) {
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

func exiftoolPath() (string, error) { return lookTool(envExifTool, "exiftool") }

func jpegtranPath() (string, error) { return lookTool(envJpegTran, "jpegtran") }

func cjpegPath() (string, error) { return lookTool(envCJPEG, "cjpeg") }

func djpegPath() (string, error) { return lookTool(envDJPEG, "djpeg") }

// CheckCompressionTools verifies the optional tools used for lossy size caps.
func CheckCompressionTools() error {
	var errs []error
	if _, err := cjpegPath(); err != nil {
		errs = append(errs, err)
	}
	if _, err := djpegPath(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
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

// runTool executes a helper with stdout streamed to w (so a multi-megabyte
// extraction never lands in memory) and stderr captured up to
// maxStderrBytes. Arguments are passed directly; no shell is involved.
func runTool(ctx context.Context, bin string, args []string, stdout io.Writer) error {
	name := filepath.Base(bin)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = stdout
	stderr := cappedBuffer{max: maxStderrBytes}
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil && ctx.Err() != nil {
		// The helper was killed by context cancellation; report that instead
		// of an opaque signal error so errors.Is(err, context.Canceled) works.
		return fmt.Errorf("%s canceled: %w", name, ctx.Err())
	}
	if err != nil {
		return toolError(name, err, stderr.String())
	}
	return nil
}

// captureTool runs a helper and returns its standard output, which is used
// for small metadata responses.
func captureTool(ctx context.Context, bin string, args []string) ([]byte, error) {
	var out bytes.Buffer
	if err := runTool(ctx, bin, args, &out); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func toolError(name string, err error, stderr string) error {
	if stderr == "" {
		return fmt.Errorf("%s: %w", name, err)
	}
	return fmt.Errorf("%s: %w: %s", name, err, stderr)
}

// toolPrefix runs a helper and returns at most limit bytes of its standard
// output, then stops it. Bounded like this, Inspect can read the header of an
// embedded JPEG without extracting the whole thing to disk or memory.
func toolPrefix(ctx context.Context, bin string, args []string, limit int64) ([]byte, error) {
	name := filepath.Base(bin)
	cmd := exec.CommandContext(ctx, bin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := cappedBuffer{max: maxStderrBytes}
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
		return nil, fmt.Errorf("%s canceled: %w", name, ctx.Err())
	}
	if readErr != nil {
		return nil, fmt.Errorf("%s: %w", name, readErr)
	}
	if len(data) == 0 {
		if waitErr != nil {
			return nil, toolError(name, waitErr, stderr.String())
		}
		return nil, fmt.Errorf("%s: no data for the requested tag", name)
	}
	return data, nil
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
