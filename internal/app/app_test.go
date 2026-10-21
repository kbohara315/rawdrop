package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
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

// TestProgressHelpers pins the determinate progress value and the truthful
// label: skipped, failed and canceled items are terminal, so they count as
// processed.
func TestProgressHelpers(t *testing.T) {
	tests := []struct {
		name      string
		counts    model.Counts
		wantValue float64
		wantLabel string
	}{
		{"zero", model.Counts{}, 0, "0% · 0 of 0 processed"},
		{"initial", model.Counts{Total: 162}, 0, "0% · 0 of 162 processed"},
		{"intermediate", model.Counts{Total: 162, Done: 60, Skipped: 6, Failed: 1, Canceled: 1}, 68.0 / 162.0, "41% · 68 of 162 processed"},
		{"mostly done", model.Counts{Total: 4, Done: 3}, 0.75, "75% · 3 of 4 processed"},
		{"skips and failures are terminal", model.Counts{Total: 5, Done: 2, Skipped: 1, Failed: 2}, 1, "100% · 5 of 5 processed"},
		{"all done", model.Counts{Total: 4, Done: 4}, 1, "100% · 4 of 4 processed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := progressValue(tt.counts); math.Abs(got-tt.wantValue) > 1e-9 {
				t.Errorf("progressValue() = %v, want %v", got, tt.wantValue)
			}
			if got := progressLabel(tt.counts); got != tt.wantLabel {
				t.Errorf("progressLabel() = %q, want %q", got, tt.wantLabel)
			}
		})
	}
}

// TestRunningStage checks the active-file/stage line and the paused,
// canceling and preparing fallbacks. No inside-file percentage is invented.
func TestRunningStage(t *testing.T) {
	active := viewState{items: []model.Item{{
		Name: "IMG_0001.CR2", Status: model.StatusExtracting, Message: "Extracting the camera JPEG…",
	}}}
	if got, want := runningStage(active), "IMG_0001.CR2 — Extracting the camera JPEG…"; got != want {
		t.Errorf("runningStage(active) = %q, want %q", got, want)
	}
	if got, want := runningStage(viewState{paused: true}), "Paused"; got != want {
		t.Errorf("runningStage(paused) = %q, want %q", got, want)
	}
	if got, want := runningStage(viewState{canceling: true}), "Canceling…"; got != want {
		t.Errorf("runningStage(canceling) = %q, want %q", got, want)
	}
	if got, want := runningStage(viewState{}), "Preparing…"; got != want {
		t.Errorf("runningStage(empty) = %q, want %q", got, want)
	}
	queued := viewState{items: []model.Item{{Name: "IMG_0002.CR2", Status: model.StatusQueued}}, paused: true}
	if got, want := runningStage(queued), "Paused"; got != want {
		t.Errorf("runningStage(queued, paused) = %q, want %q", got, want)
	}
	fresh := viewState{items: []model.Item{{Name: "IMG_0003.CR2", Status: model.StatusRotating}}}
	if got, want := runningStage(fresh), "IMG_0003.CR2 — Rotating losslessly…"; got != want {
		t.Errorf("runningStage(fresh) = %q, want %q", got, want)
	}
}

// TestSetOutputDirPersistsToJournal checks that a destination chosen before
// Export is written to the journal immediately, not only when the export
// starts.
func TestSetOutputDirPersistsToJournal(t *testing.T) {
	in := t.TempDir()
	if err := os.WriteFile(filepath.Join(in, "IMG_0001.CR2"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	a := New()
	a.loadPreviousFrom(stateDir)
	t.Cleanup(func() { closeStore(a) })
	a.AddPaths([]string{in})

	out := filepath.Join(t.TempDir(), "exports")
	a.SetOutputDir(out)

	a.mu.Lock()
	gotDir := a.outputDir
	gotJobDir := ""
	if a.job != nil {
		gotJobDir = a.job.Settings.OutputDir
	}
	a.mu.Unlock()
	if gotDir != out {
		t.Fatalf("outputDir = %q, want %q", gotDir, out)
	}
	if gotJobDir != out {
		t.Fatalf("job.Settings.OutputDir = %q, want %q", gotJobDir, out)
	}

	data, err := os.ReadFile(filepath.Join(stateDir, "jobs.json"))
	if err != nil {
		t.Fatalf("journal was not written: %v", err)
	}
	var saved model.Job
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("journal is not valid JSON: %v", err)
	}
	if saved.Settings.OutputDir != out {
		t.Fatalf("journal OutputDir = %q, want %q", saved.Settings.OutputDir, out)
	}
}

// TestSetOutputDirRefusesInvalidTargets checks the safety rules: a regular
// file is not a folder, the destination cannot change while running, and it is
// fixed once the batch has committed an output.
func TestSetOutputDirRefusesInvalidTargets(t *testing.T) {
	in := t.TempDir()
	if err := os.WriteFile(filepath.Join(in, "IMG_0001.CR2"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := New()
	a.AddPaths([]string{in})
	a.mu.Lock()
	original := a.outputDir
	a.mu.Unlock()
	if original == "" {
		t.Fatal("AddPaths did not choose a default destination")
	}

	// A regular file is not a folder.
	file := filepath.Join(t.TempDir(), "not-a-folder")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	a.SetOutputDir(file)
	a.mu.Lock()
	if a.outputDir != original {
		t.Errorf("outputDir = %q, want the unchanged %q after a file target", a.outputDir, original)
	}
	if a.banner == "" {
		t.Error("refusing a file destination did not surface an explanation")
	}
	a.banner = ""
	a.mu.Unlock()

	// A change while running is refused.
	a.mu.Lock()
	a.running = true
	a.mu.Unlock()
	a.SetOutputDir(filepath.Join(t.TempDir(), "running"))
	a.mu.Lock()
	a.running = false
	if a.outputDir != original {
		t.Errorf("outputDir = %q, want the unchanged %q while running", a.outputDir, original)
	}
	if a.banner == "" {
		t.Error("refusing a change while running did not surface an explanation")
	}
	a.banner = ""
	a.mu.Unlock()

	// Once an output is committed, the destination is fixed for the batch.
	a.mu.Lock()
	a.job.Items[0].OutputPath = filepath.Join(t.TempDir(), "IMG_0001.jpg")
	a.mu.Unlock()
	a.SetOutputDir(filepath.Join(t.TempDir(), "committed"))
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.outputDir != original {
		t.Errorf("outputDir = %q, want the unchanged %q after a committed output", a.outputDir, original)
	}
	if a.banner == "" {
		t.Error("refusing a destination change after a committed output did not surface an explanation")
	}
}

// TestSetOutputDirSurfacesStatErrors checks that a target whose parent is a
// regular file is rejected with a visible explanation. os.Stat on such a path
// fails with a not-a-directory error (not os.ErrNotExist), so the destination
// must not be mistaken for a folder that will be created: the previously chosen
// destination and the journal stay unchanged. Using a regular-file parent keeps
// the case portable even when the test runs as root, unlike a permission error.
func TestSetOutputDirSurfacesStatErrors(t *testing.T) {
	in := t.TempDir()
	if err := os.WriteFile(filepath.Join(in, "IMG_0001.CR2"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	a := New()
	a.loadPreviousFrom(stateDir)
	t.Cleanup(func() { closeStore(a) })
	a.AddPaths([]string{in})

	good := filepath.Join(t.TempDir(), "exports")
	a.SetOutputDir(good)
	a.mu.Lock()
	if a.outputDir != good {
		got := a.outputDir
		a.mu.Unlock()
		t.Fatalf("setup: outputDir = %q, want %q", got, good)
	}
	a.mu.Unlock()

	journalPath := filepath.Join(stateDir, "jobs.json")
	before, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatalf("journal was not written for the accepted destination: %v", err)
	}

	parent := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(parent, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(parent, "child")
	if _, statErr := os.Stat(bad); statErr == nil || errors.Is(statErr, os.ErrNotExist) {
		t.Skipf("os.Stat(%s) = %v; this platform does not report a not-a-directory error", bad, statErr)
	}
	a.SetOutputDir(bad)

	a.mu.Lock()
	gotDir := a.outputDir
	gotBanner := a.banner
	a.mu.Unlock()
	if gotDir != good {
		t.Errorf("outputDir = %q, want the unchanged %q after a bad-parent target", gotDir, good)
	}
	if gotBanner == "" {
		t.Error("rejecting a stat-error destination did not surface an explanation")
	}

	after, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatalf("read journal after refusal: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("journal changed after a refused destination:\nbefore: %s\nafter:  %s", before, after)
	}
}
