package raw

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestInspectionInfoIsACopy proves Info hands back a value, so a caller cannot
// mutate the shared handle through the summary.
func TestInspectionInfoIsACopy(t *testing.T) {
	insp := &Inspection{info: Info{Width: 6000, Height: 4000, Make: "Canon"}}
	got := insp.Info()
	got.Width = 1
	got.Make = "changed"
	if insp.info.Width != 6000 || insp.info.Make != "Canon" {
		t.Fatalf("Info() exposed a mutable reference: %+v", insp.info)
	}
}

// TestInspectionRejectsChangedSource proves the handle refuses to export from
// candidate offsets when the source changed after inspection.
func TestInspectionRejectsChangedSource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "photo.CR2")
	if err := os.WriteFile(src, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	insp := &Inspection{source: src, identity: st}

	if err := insp.checkSource(); err != nil {
		t.Fatalf("checkSource() on the unchanged source = %v, want nil", err)
	}

	// A size change must be detected even when the mtime resolution is coarse.
	if err := os.WriteFile(src, []byte("original plus extra bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := insp.checkSource(); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("checkSource() after a size change = %v, want ErrSourceChanged", err)
	}
}

// TestInspectionCheckSourceWithoutIdentity proves a hand-built inspection with
// no recorded identity does not stat the file.
func TestInspectionCheckSourceWithoutIdentity(t *testing.T) {
	insp := &Inspection{source: filepath.Join(t.TempDir(), "missing.CR2")}
	if err := insp.checkSource(); err != nil {
		t.Fatalf("checkSource() without identity = %v, want nil", err)
	}
}
