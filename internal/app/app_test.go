package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rawdrop/internal/journal"
	"rawdrop/internal/model"
)

func TestUniquePathsCleansAndSorts(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "b.CR2")
	b := filepath.Join(dir, "a.CR2")
	got := uniquePaths([]string{a, " " + a + " ", b, a, ""})
	if len(got) != 2 {
		t.Fatalf("got %d paths, want 2: %v", len(got), got)
	}
	if got[0] != b || got[1] != a {
		t.Fatalf("paths not cleaned and sorted: %v", got)
	}
	for _, p := range got {
		if !filepath.IsAbs(p) {
			t.Fatalf("path is not absolute: %q", p)
		}
	}
}

func TestDefaultOutputSingleFolder(t *testing.T) {
	dir := t.TempDir()
	got := defaultOutput([]string{dir})
	want := filepath.Join(dir, "JPEG - Full Resolution")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestDefaultOutputSingleFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "IMG_0001.CR2")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := defaultOutput([]string{file})
	want := filepath.Join(dir, "JPEG - Full Resolution")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestDefaultOutputUnrelatedInputsFallBack(t *testing.T) {
	// Paths whose deepest common parent is the filesystem root have no
	// sensible shared export folder, so the Pictures fallback wins.
	got := defaultOutput([]string{"/usr", "/etc"})
	if !strings.HasSuffix(got, filepath.Join("Pictures", "RawDrop Exports")) {
		t.Fatalf("got %q, want the Pictures/RawDrop Exports fallback", got)
	}
}

func TestDefaultOutputSharedParent(t *testing.T) {
	root := t.TempDir()
	one := filepath.Join(root, "a")
	two := filepath.Join(root, "b")
	for _, dir := range []string{one, two} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got := defaultOutput([]string{one, two})
	if !strings.HasPrefix(got, root) || !strings.HasSuffix(got, "JPEG - Full Resolution") {
		t.Fatalf("got %q, want an export folder under %q", got, root)
	}
}

// AddPaths is the drop path: it scans synchronously and fills the batch.
func TestAddPathsBuildsBatch(t *testing.T) {
	in := t.TempDir()
	for _, name := range []string{"IMG_0001.CR2", "IMG_0002.cr2"} {
		if err := os.WriteFile(filepath.Join(in, name), []byte("not really raw"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(in, "notes.txt"), []byte("ignored"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := New()
	a.AddPaths([]string{in})

	a.mu.Lock()
	job := a.job
	scanning := a.scanning
	a.mu.Unlock()
	if scanning {
		t.Fatal("scan did not finish")
	}
	if job == nil {
		t.Fatal("no job was created")
	}
	if len(job.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(job.Items))
	}
	if !strings.HasSuffix(job.Settings.OutputDir, "JPEG - Full Resolution") {
		t.Fatalf("unexpected output dir %q", job.Settings.OutputDir)
	}
	for _, item := range job.Items {
		if item.Status != model.StatusQueued {
			t.Fatalf("item %s has status %s, want queued", item.Name, item.Status)
		}
		if !strings.HasSuffix(item.OutputName, ".jpg") {
			t.Fatalf("item %s has output name %q", item.Name, item.OutputName)
		}
	}

	// Adding the same folder again must not duplicate items.
	a.AddPaths([]string{in})
	a.mu.Lock()
	total := len(a.job.Items)
	a.mu.Unlock()
	if total != 2 {
		t.Fatalf("re-adding the folder produced %d items, want 2", total)
	}
}

func TestNewBatchClearsEverything(t *testing.T) {
	in := t.TempDir()
	if err := os.WriteFile(filepath.Join(in, "IMG_0001.CR2"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := New()
	a.AddPaths([]string{in})
	a.NewBatch()

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.job != nil || a.inputs != nil || a.outputDir != "" {
		t.Fatalf("batch state survived: job=%v inputs=%v outputDir=%q", a.job, a.inputs, a.outputDir)
	}
	if a.phase != phaseEmpty {
		t.Fatalf("phase = %v, want phaseEmpty", a.phase)
	}
}

func TestStartExportWithoutPhotosOnlyFlashes(t *testing.T) {
	a := New()
	a.StartExport()
	deadline := time.Now().Add(time.Second)
	for {
		a.mu.Lock()
		job := a.job
		running := a.running
		a.mu.Unlock()
		if job == nil && !running {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("StartExport with no photos started a job")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Resume rules: verified outputs are kept, missing or modified outputs are
// queued again, and items interrupted mid-flight are requeued.
func TestLoadPreviousValidatesOutputs(t *testing.T) {
	stateDir := t.TempDir()
	outDir := t.TempDir()

	validPath := filepath.Join(outDir, "IMG_0001.jpg")
	if err := os.WriteFile(validPath, []byte("a complete export"), 0o644); err != nil {
		t.Fatal(err)
	}
	validHash, err := hashFile(validPath)
	if err != nil {
		t.Fatal(err)
	}

	job := model.NewJob([]string{filepath.Join(outDir, "IMG_0001.CR2")}, model.DefaultSettings(outDir))
	job.Items = []*model.Item{
		{ID: "1", Source: "/src/IMG_0001.CR2", Name: "IMG_0001.CR2", OutputName: "IMG_0001.jpg",
			Status: model.StatusDone, OutputPath: validPath, OutputSHA256: validHash},
		{ID: "2", Source: "/src/IMG_0002.CR2", Name: "IMG_0002.CR2", OutputName: "IMG_0002.jpg",
			Status: model.StatusDone, OutputPath: filepath.Join(outDir, "missing.jpg"), OutputSHA256: validHash},
		{ID: "3", Source: "/src/IMG_0003.CR2", Name: "IMG_0003.CR2", OutputName: "IMG_0003.jpg",
			Status: model.StatusInspecting, Message: "Reading camera metadata…"},
	}

	seed, err := journal.NewStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Lock(); err != nil {
		t.Fatal(err)
	}
	if err := seed.Save(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	a := New()
	a.loadPreviousFrom(stateDir)

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.job == nil {
		t.Fatal("journal was not loaded")
	}
	if len(a.job.Items) != 3 {
		t.Fatalf("got %d items, want 3", len(a.job.Items))
	}
	it := a.job.Items[0]
	if it.Status != model.StatusDone || it.OutputPath != validPath {
		t.Errorf("item 1 = %s %q, want done with its verified output", it.Status, it.OutputPath)
	}
	it = a.job.Items[1]
	if it.Status != model.StatusQueued || it.OutputPath != "" || it.OutputSHA256 != "" {
		t.Errorf("item 2 = %s %q (hash %q), want requeued with cleared output", it.Status, it.OutputPath, it.OutputSHA256)
	}
	it = a.job.Items[2]
	if it.Status != model.StatusQueued {
		t.Errorf("item 3 = %s, want queued after an interruption", it.Status)
	}
	if a.phase != phaseReady || a.banner == "" {
		t.Errorf("phase = %v banner = %q, want a resume banner", a.phase, a.banner)
	}
}
