package exiftool

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// script writes an executable shell helper and returns its path. The tests
// need a POSIX shell, so they skip on Windows.
func script(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("helper scripts require a POSIX shell")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not available: %v", err)
	}
	path := filepath.Join(t.TempDir(), "fake-exiftool")
	if err := os.WriteFile(path, []byte("#!"+sh+"\n"+body), 0o755); err != nil {
		t.Fatalf("write helper script: %v", err)
	}
	return path
}

func TestDiscoverUsesEnvironmentOverride(t *testing.T) {
	bin := script(t, "exit 0\n")
	t.Setenv("RAWDROP_TEST_TOOL", bin)
	got, err := Discover("RAWDROP_TEST_TOOL", "definitely-not-on-path")
	if err != nil {
		t.Fatalf("Discover() error: %v", err)
	}
	if got != bin {
		t.Fatalf("Discover() = %q, want the override %q", got, bin)
	}
}

func TestDiscoverRejectsMissingOverride(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-tool")
	t.Setenv("RAWDROP_TEST_TOOL", missing)
	_, err := Discover("RAWDROP_TEST_TOOL", "definitely-not-on-path")
	if err == nil {
		t.Fatal("Discover() = nil error, want a missing-override error")
	}
	if !strings.Contains(err.Error(), "RAWDROP_TEST_TOOL") {
		t.Errorf("Discover() = %v, want it to name the override variable", err)
	}
}

func TestDiscoverFindsOnPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "exiftool"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	got, err := Discover("RAWDROP_TEST_UNSET", "exiftool")
	if err != nil {
		t.Fatalf("Discover() error: %v", err)
	}
	if got != filepath.Join(dir, "exiftool") {
		t.Fatalf("Discover() = %q, want the path entry", got)
	}
}

func TestDiscoverMissingOnPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := Discover("RAWDROP_TEST_UNSET", "exiftool")
	if err == nil {
		t.Fatal("Discover() = nil error, want a not-found error")
	}
	if !strings.Contains(err.Error(), "RAWDROP_TEST_UNSET") {
		t.Errorf("Discover() = %v, want it to suggest the override", err)
	}
}

func TestPathHonorsEnvExifTool(t *testing.T) {
	bin := script(t, "exit 0\n")
	t.Setenv(EnvExifTool, bin)
	got, err := Path()
	if err != nil {
		t.Fatalf("Path() error: %v", err)
	}
	if got != bin {
		t.Fatalf("Path() = %q, want %q", got, bin)
	}
}

func TestStreamWritesStdout(t *testing.T) {
	bin := New(script(t, "printf 'hello world'\n"))
	var out bytes.Buffer
	if err := bin.Stream(context.Background(), nil, &out); err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	if out.String() != "hello world" {
		t.Fatalf("Stream() wrote %q, want %q", out.String(), "hello world")
	}
}

func TestCaptureReturnsAllOutput(t *testing.T) {
	bin := New(script(t, "printf '0123456789abcdefghijklmnopqrstuvwxyz'\n"))
	out, err := bin.Capture(context.Background(), nil, 0)
	if err != nil {
		t.Fatalf("Capture() error: %v", err)
	}
	if string(out) != "0123456789abcdefghijklmnopqrstuvwxyz" {
		t.Fatalf("Capture() = %q, want the whole output", out)
	}
}

func TestCaptureEnforcesLimit(t *testing.T) {
	bin := New(script(t, "printf '0123456789abcdefghijklmnopqrstuvwxyz'\n"))
	out, err := bin.Capture(context.Background(), nil, 16)
	if !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("Capture() error = %v, want ErrOutputLimit", err)
	}
	if out != nil {
		t.Fatalf("Capture() returned %q with an error, want nil", out)
	}
	if !strings.Contains(err.Error(), "exceeded 16 bytes") {
		t.Errorf("Capture() error = %v, want it to name the limit", err)
	}
}

func TestCaptureAcceptsOutputAtLimit(t *testing.T) {
	bin := New(script(t, "printf 'exactly sixteen!'\n"))
	out, err := bin.Capture(context.Background(), nil, int64(len("exactly sixteen!")))
	if err != nil {
		t.Fatalf("Capture() error: %v", err)
	}
	if string(out) != "exactly sixteen!" {
		t.Fatalf("Capture() = %q, want the exact output", out)
	}
}

func TestCommandFailureIncludesStderr(t *testing.T) {
	bin := New(script(t, "echo 'boom' >&2\nexit 3\n"))
	err := bin.Stream(context.Background(), nil, &bytes.Buffer{})
	if err == nil {
		t.Fatal("Stream() = nil error, want a command failure")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("Stream() error = %v, want the helper stderr", err)
	}
	if !strings.Contains(err.Error(), "exif") && !strings.Contains(err.Error(), "fake-exiftool") {
		t.Errorf("Stream() error = %v, want it to name the helper", err)
	}
}

func TestStreamContextCancellation(t *testing.T) {
	bin := New(script(t, "exec sleep 30\n"))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	defer cancel()
	err := bin.Stream(ctx, nil, &bytes.Buffer{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Stream() error = %v, want context.Canceled", err)
	}
}

func TestPrefixReturnsBoundedPrefix(t *testing.T) {
	bin := New(script(t, "printf 'abcdefghijklmnopqrstuvwxyz'\n"))
	out, err := bin.Prefix(context.Background(), nil, 10)
	if err != nil {
		t.Fatalf("Prefix() error: %v", err)
	}
	if string(out) != "abcdefghij" {
		t.Fatalf("Prefix() = %q, want the first 10 bytes", out)
	}
}

func TestPrefixNoData(t *testing.T) {
	bin := New(script(t, "exit 0\n"))
	_, err := bin.Prefix(context.Background(), nil, 10)
	if err == nil {
		t.Fatal("Prefix() = nil error, want a no-data error")
	}
	if !strings.Contains(err.Error(), "no data") {
		t.Errorf("Prefix() error = %v, want a no-data explanation", err)
	}
}

func TestPrefixReportsFailure(t *testing.T) {
	bin := New(script(t, "echo 'nope' >&2\nexit 4\n"))
	_, err := bin.Prefix(context.Background(), nil, 10)
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("Prefix() error = %v, want the helper stderr", err)
	}
}

func TestCappedBufferKeepsPrefix(t *testing.T) {
	b := cappedBuffer{max: 4}
	if n, err := b.Write([]byte("abcdefgh")); n != 8 || err != nil {
		t.Fatalf("Write = %d, %v; want 8, nil", n, err)
	}
	if got := b.String(); !strings.Contains(got, "abcd") || !strings.Contains(got, "truncated") {
		t.Fatalf("String() = %q, want the kept prefix and a truncation note", got)
	}
	b2 := cappedBuffer{max: 16}
	_, _ = b2.Write([]byte(" short "))
	if got := b2.String(); got != "short" {
		t.Fatalf("String() = %q, want %q", got, "short")
	}
}
