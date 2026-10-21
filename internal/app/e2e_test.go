package app

import (
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"rawdrop/internal/model"
	"rawdrop/internal/raw"
)

// TestEndToEndDropExportResume drives the path a person takes, without a
// display: the MyGo headless tester renders the real view, so the folder is
// dropped on the actual drop zone and Export is the actual button. The
// export, the interruption and the relaunch all use the app's own code.
func TestEndToEndDropExportResume(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: exports real RAW photos")
	}
	if err := raw.CheckTools(); err != nil {
		t.Skipf("exiftool or jpegtran is not available: %v", err)
	}
	sample := "/home/kshitij/Pictures/dashain photos"
	// Two sideways photos (orientation 8) and two already-upright ones, so
	// the check covers both a lossless rotation and a byte-equal copy.
	want := map[string][2]int{
		"IMG_3128.CR2": {4000, 6000}, // sideways: must come out upright portrait
		"IMG_3129.CR2": {4000, 6000},
		"IMG_3144.CR2": {6000, 4000}, // upright landscape
		"IMG_3145.CR2": {6000, 4000},
	}
	for name := range want {
		if _, err := os.Stat(filepath.Join(sample, name)); err != nil {
			t.Skipf("sample photo missing: %v", err)
		}
	}

	src := t.TempDir()
	before := make(map[string]string, len(want))
	for name := range want {
		dst := filepath.Join(src, name)
		if err := copyFile(dst, filepath.Join(sample, name)); err != nil {
			t.Fatal(err)
		}
		h, err := hashFile(dst)
		if err != nil {
			t.Fatal(err)
		}
		before[name] = h
	}

	stateDir := t.TempDir()
	outDir := t.TempDir()

	// First run: drop the folder, set the destination, press Export, then
	// cancel once the first photo is done, as a person would.
	a := New()
	a.loadPreviousFrom(stateDir)
	t.Cleanup(func() { closeStore(a) })

	tester := ui.NewTester(a.View, 1400, 900)
	tester.Frame()
	zone, ok := tester.Find("Drop RAW photos here")
	if !ok {
		t.Fatal("the welcome screen did not render its drop zone")
	}
	if !tester.DropFiles(zone.X+zone.W/2, zone.Y+zone.H/2, src) {
		t.Fatal("the drop zone did not take the dropped folder")
	}
	tester.Frame()

	a.SetOutputDir(outDir)
	tester.Frame()
	if err := tester.Click("Export 4 photo(s)"); err != nil {
		t.Fatalf("could not press Export: %v", err)
	}
	tester.Frame()

	// The running view must show a truthful processed count, not the old
	// startup text. The bar fraction is determinate over the known batch.
	if !tester.HasText("processed") {
		t.Error("the running view did not show a processed progress label")
	}
	if tester.HasText("Starting…") {
		t.Error("the running view still shows the stale Starting… text")
	}

	// Pause and resume once, through the real buttons.
	if err := tester.Click("Pause"); err != nil {
		t.Fatalf("could not press Pause: %v", err)
	}
	tester.Frame()
	waitFor(t, 30*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.paused
	})
	if !tester.HasText("Resume") {
		t.Fatal("the paused view does not offer Resume")
	}
	if err := tester.Click("Resume"); err != nil {
		t.Fatalf("could not press Resume: %v", err)
	}
	tester.Frame()

	// The sidebar shows the active photo and its stage while it works.
	sawActive := false
	for i := 0; i < 200 && !sawActive; i++ {
		a.mu.Lock()
		running := a.running
		a.mu.Unlock()
		if !running {
			break
		}
		tester.Frame()
		if tester.HasText("Reading camera metadata…") || tester.HasText("Extracting the camera JPEG…") ||
			tester.HasText("Correcting orientation losslessly…") {
			sawActive = true
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !sawActive {
		t.Error("the running view never showed the active photo and stage")
	}

	waitFor(t, 60*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.job != nil && a.job.Count().Done >= 1
	})
	a.Cancel()
	waitFor(t, 2*time.Minute, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return !a.running
	})

	a.mu.Lock()
	firstRun := a.job.Count()
	a.mu.Unlock()
	if firstRun.Done == 0 || firstRun.Done >= len(want) {
		t.Fatalf("first run finished %d of %d photos; the test needs a partial run to resume", firstRun.Done, len(want))
	}
	if _, err := os.Stat(filepath.Join(stateDir, "jobs.json")); err != nil {
		t.Fatalf("the journal was not saved: %v", err)
	}
	closeStore(a)

	// Second run: relaunch, load the journal, and resume.
	b := New()
	b.loadPreviousFrom(stateDir)
	t.Cleanup(func() { closeStore(b) })
	b.mu.Lock()
	loaded := b.job
	b.mu.Unlock()
	if loaded == nil {
		t.Fatal("the journal did not reload")
	}
	b.StartExport()
	waitFor(t, 3*time.Minute, func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return !b.running && b.job != nil && b.job.Finished()
	})

	b.mu.Lock()
	counts := b.job.Count()
	outputs := make(map[string]string)
	for _, item := range b.job.Items {
		if item.Status == model.StatusDone {
			outputs[item.Name] = item.OutputPath
		}
	}
	b.mu.Unlock()

	if counts.Total != len(want) || counts.Done != len(want) || counts.Failed != 0 || counts.Skipped != 0 || counts.Canceled != 0 {
		t.Fatalf("final counts = %+v, want all %d exported", counts, len(want))
	}

	// Every photo must have exactly one full-resolution upright output.
	seen := make(map[string]bool)
	for name, size := range want {
		path, ok := outputs[name]
		if !ok {
			t.Errorf("%s did not finish", name)
			continue
		}
		if seen[path] {
			t.Errorf("%s reused output %q", name, path)
		}
		seen[path] = true
		checkJPEG(t, path, size[0], size[1])
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(want) {
		t.Errorf("output folder has %d entries, want %d", len(entries), len(want))
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".rawdrop-") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
	// Originals are read-only inputs and must be byte-identical.
	for name, h := range before {
		now, err := hashFile(filepath.Join(src, name))
		if err != nil || now != h {
			t.Errorf("original %s changed (err %v)", name, err)
		}
	}
}

func checkJPEG(t *testing.T, path string, wantW, wantH int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Errorf("%s: %v", path, err)
		return
	}
	defer f.Close()
	cfg, err := jpeg.DecodeConfig(f)
	if err != nil {
		t.Errorf("%s: not a JPEG: %v", path, err)
		return
	}
	if cfg.Width != wantW || cfg.Height != wantH {
		t.Errorf("%s is %dx%d, want %dx%d (full-resolution, upright)", path, cfg.Width, cfg.Height, wantW, wantH)
	}
	st, err := f.Stat()
	if err != nil {
		t.Errorf("%s: %v", path, err)
		return
	}
	if st.Size() < 500_000 {
		t.Errorf("%s looks preview-sized (%d bytes)", path, st.Size())
	}
	if st.Mode().Perm() != 0o644 {
		t.Errorf("%s has mode %v, want 644", path, st.Mode().Perm())
	}
}

// waitFor polls cond until it is true or the timeout passes.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the export")
}

func copyFile(dst, src string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

// closeStore releases the journal lock so a relaunch can take it.
func closeStore(a *App) {
	a.mu.Lock()
	store := a.store
	a.store = nil
	a.mu.Unlock()
	if store != nil {
		_ = store.Close()
	}
}
