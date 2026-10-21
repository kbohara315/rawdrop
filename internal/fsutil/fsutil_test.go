package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestCommitNewLinksAndRemovesTemp checks the ordinary commit: the content is
// visible under the new name and the temp file is cleaned up.
func TestCommitNewLinksAndRemovesTemp(t *testing.T) {
	dir := t.TempDir()
	temp := filepath.Join(dir, "temp.jpg")
	dst := filepath.Join(dir, "final.jpg")
	if err := os.WriteFile(temp, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CommitNew(temp, dst); err != nil {
		t.Fatalf("CommitNew() error: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read committed file: %v", err)
	}
	if string(got) != "data" {
		t.Errorf("committed content = %q, want %q", got, "data")
	}
	if _, err := os.Stat(temp); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temp file still exists after a successful commit")
	}
}

// TestCommitNewRefusesExistingDestination checks the no-replace contract: an
// existing destination is untouched and the temp file is left for the caller.
func TestCommitNewRefusesExistingDestination(t *testing.T) {
	dir := t.TempDir()
	temp := filepath.Join(dir, "temp.jpg")
	dst := filepath.Join(dir, "final.jpg")
	if err := os.WriteFile(temp, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CommitNew(temp, dst); !errors.Is(err, os.ErrExist) {
		t.Fatalf("CommitNew() error = %v, want os.ErrExist", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old" {
		t.Errorf("existing destination was modified: %q", got)
	}
	if _, err := os.Stat(temp); err != nil {
		t.Errorf("temp file should remain after a refused commit: %v", err)
	}
}

// TestCommitNewSucceedsWhenTempCannotBeRemoved makes temp unlinkable by making
// its directory read-only while the destination lives elsewhere. The link is
// the commit point, so CommitNew must still report success; the caller's own
// cleanup removes temp. Running as root bypasses the permission, in which case
// the remove simply succeeds and the success assertion still holds.
func TestCommitNewSucceedsWhenTempCannotBeRemoved(t *testing.T) {
	tempDir := t.TempDir()
	dstDir := t.TempDir()
	temp := filepath.Join(tempDir, "temp.jpg")
	dst := filepath.Join(dstDir, "final.jpg")
	if err := os.WriteFile(temp, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tempDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tempDir, 0o755) })

	if err := CommitNew(temp, dst); err != nil {
		t.Fatalf("CommitNew() = %v, want success once the link exists", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("committed file missing after a cleanup failure: %v", err)
	}
	if string(got) != "data" {
		t.Errorf("committed content = %q, want %q", got, "data")
	}
}

// TestUniquePath checks the deterministic suffix sequence.
func TestUniquePath(t *testing.T) {
	dir := t.TempDir()
	if got, want := UniquePath(dir, "a.jpg"), filepath.Join(dir, "a.jpg"); got != want {
		t.Fatalf("UniquePath() = %q, want %q", got, want)
	}
	for _, name := range []string{"a.jpg", "a (2).jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := UniquePath(dir, "a.jpg"), filepath.Join(dir, "a (3).jpg"); got != want {
		t.Fatalf("UniquePath() = %q, want %q", got, want)
	}
}
