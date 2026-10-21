package app

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"rawdrop/internal/exiftool"
	"rawdrop/internal/model"
)

// testJPEG returns a complete baseline JPEG of the given size.
func testJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 3), G: uint8(y * 5), B: 120, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatalf("encode JPEG: %v", err)
	}
	return buf.Bytes()
}

// appInspectionJSON describes a source that is itself a JPEG whose
// PreviewImage covers the whole file, so inspection reads the dimensions from
// the source and the fake helper only serves one binary extraction.
func appInspectionJSON(w, h, length int) string {
	return fmt.Sprintf(`[{"IFD0:ImageWidth":%d,"IFD0:ImageHeight":%d,"IFD0:Orientation":1,`+
		`"IFD0:PreviewImage":"(Binary data %d bytes, use -b option to extract)",`+
		`"IFD0:PreviewImageStart":0,"IFD0:PreviewImageLength":%d}]`, w, h, length, length)
}

// writeAppFakeExifTool writes an executable helper that answers -j with meta
// and every other call with the JPEG bytes. When gate is set, binary
// extractions wait until that file exists. When marker is set, a source whose
// path contains the marker blocks forever, so a test can cancel a single
// in-flight photo deterministically. logPath, when set, receives a line per
// inspection (the only calls carrying -ImageWidth).
func writeAppFakeExifTool(t *testing.T, meta, jpegPath, logPath, gate, marker string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("helper scripts require a POSIX shell")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not available: %v", err)
	}
	logLine := ""
	if logPath != "" {
		logLine = "  if [ \"$arg\" = \"-ImageWidth\" ]; then echo inspect >> '" + logPath + "'; fi\n"
	}
	markLine := ""
	if marker != "" {
		markLine = "  case \"$arg\" in *" + marker + "*) marked=1 ;; esac\n"
	}
	extract := ""
	if gate != "" {
		extract += "  while [ ! -e '" + gate + "' ]; do sleep 0.02; done\n"
	}
	if marker != "" {
		extract += "  if [ \"$marked\" = \"1\" ]; then exec sleep 30; fi\n"
	}
	extract += "  cat '" + jpegPath + "'\n"
	body := "#!/" + sh + "\n" +
		"json=0\n" +
		"marked=0\n" +
		"for arg in \"$@\"; do\n" +
		"  if [ \"$arg\" = \"-j\" ]; then json=1; fi\n" +
		logLine +
		markLine +
		"done\n" +
		"if [ \"$json\" = \"1\" ]; then\n" +
		"  printf '%s' '" + meta + "'\n" +
		"else\n" +
		"  case \"$1\" in -b) ;; *) cat '" + jpegPath + "'; exit 0 ;; esac\n" +
		extract +
		"fi\n"
	path := filepath.Join(t.TempDir(), "fake-exiftool")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake helper: %v", err)
	}
	return path
}

// newPoolApp builds an app with a directly seeded job so the export pool can be
// exercised without scanning or a journal.
func newPoolApp(t *testing.T, sources []string, outDir string) *App {
	t.Helper()
	job := model.NewJob([]string{filepath.Dir(sources[0])}, model.DefaultSettings(outDir))
	for i, src := range sources {
		name := filepath.Base(src)
		job.Items = append(job.Items, &model.Item{
			ID:         fmt.Sprintf("id%02d", i),
			Source:     src,
			Name:       name,
			OutputName: strings.TrimSuffix(name, filepath.Ext(name)) + ".jpg",
			Status:     model.StatusQueued,
		})
	}
	a := New()
	a.mu.Lock()
	a.job = job
	a.outputDir = outDir
	a.mu.Unlock()
	return a
}

func writeSources(t *testing.T, dir string, count int, data []byte) []string {
	t.Helper()
	sources := make([]string, count)
	for i := range sources {
		sources[i] = filepath.Join(dir, fmt.Sprintf("img%02d.CR2", i))
		if err := os.WriteFile(sources[i], data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return sources
}

func inspectionsLogged(t *testing.T, logPath string) int {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read inspection log: %v", err)
	}
	return strings.Count(string(data), "inspect")
}

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".rawdrop-") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}

// TestExportPoolProcessesEachItemOnce runs the whole batch through the pool
// and checks that every photo is inspected exactly once and committed to its
// own output, with the sources untouched.
func TestExportPoolProcessesEachItemOnce(t *testing.T) {
	jpeg := testJPEG(t, 640, 480)
	srcDir := t.TempDir()
	outDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "inspect.log")
	jpegPath := filepath.Join(srcDir, "jpeg.bin")
	if err := os.WriteFile(jpegPath, jpeg, 0o644); err != nil {
		t.Fatal(err)
	}
	bin := writeAppFakeExifTool(t, appInspectionJSON(640, 480, len(jpeg)), jpegPath, logPath, "", "")
	t.Setenv(exiftool.EnvExifTool, bin)

	const n = 5
	sources := writeSources(t, srcDir, n, jpeg)
	before := make(map[string]string, n)
	for _, src := range sources {
		h, err := hashFile(src)
		if err != nil {
			t.Fatal(err)
		}
		before[src] = h
	}

	a := newPoolApp(t, sources, outDir)
	a.StartExport()
	waitFor(t, 60*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return !a.running
	})

	a.mu.Lock()
	counts := a.job.Count()
	outputs := make(map[string]bool)
	for _, item := range a.job.Items {
		if item.OutputPath != "" {
			outputs[item.OutputPath] = true
		}
	}
	a.mu.Unlock()
	if counts.Done != n || counts.Failed != 0 {
		t.Fatalf("counts = %+v, want %d done", counts, n)
	}
	if len(outputs) != n {
		t.Errorf("got %d distinct outputs, want %d", len(outputs), n)
	}
	if got := inspectionsLogged(t, logPath); got != n {
		t.Errorf("inspection count = %d, want %d (one per photo)", got, n)
	}
	for _, src := range sources {
		after, err := hashFile(src)
		if err != nil || after != before[src] {
			t.Errorf("source %s changed", src)
		}
	}
	assertNoTempFiles(t, outDir)
}

// TestExportPoolPausesNewReservations checks that Pause lets in-flight photos
// finish but stops the pool from claiming new ones, and that Resume continues.
func TestExportPoolPausesNewReservations(t *testing.T) {
	jpeg := testJPEG(t, 640, 480)
	srcDir := t.TempDir()
	outDir := t.TempDir()
	gate := filepath.Join(t.TempDir(), "gate")
	jpegPath := filepath.Join(srcDir, "jpeg.bin")
	if err := os.WriteFile(jpegPath, jpeg, 0o644); err != nil {
		t.Fatal(err)
	}
	bin := writeAppFakeExifTool(t, appInspectionJSON(640, 480, len(jpeg)), jpegPath, "", gate, "")
	t.Setenv(exiftool.EnvExifTool, bin)

	const n = 6
	sources := writeSources(t, srcDir, n, jpeg)
	a := newPoolApp(t, sources, outDir)
	a.StartExport()

	// Every worker is now blocked inside its extraction.
	waitFor(t, 30*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		active := 0
		for _, item := range a.job.Items {
			if item.Status == model.StatusExtracting {
				active++
			}
		}
		return active == exportWorkers
	})
	a.Pause()
	a.mu.Lock()
	paused := a.paused
	a.mu.Unlock()
	if !paused {
		t.Fatal("Pause did not set the paused flag")
	}

	// Release the blocked extractions; the workers finish their photos and
	// then must wait instead of reserving the remaining ones.
	if err := os.WriteFile(gate, []byte("go"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.job.Count().Done == exportWorkers
	})
	time.Sleep(200 * time.Millisecond) // let the workers settle into the pause wait

	a.mu.Lock()
	counts := a.job.Count()
	queued := 0
	for _, item := range a.job.Items {
		if item.Status == model.StatusQueued {
			queued++
		}
	}
	running := a.running
	a.mu.Unlock()
	if !running {
		t.Fatal("the export stopped instead of pausing")
	}
	if counts.Done != exportWorkers || queued != n-exportWorkers {
		t.Fatalf("paused state = %d done, %d queued; want %d done, %d queued",
			counts.Done, queued, exportWorkers, n-exportWorkers)
	}

	a.Resume()
	waitFor(t, 60*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return !a.running
	})
	a.mu.Lock()
	counts = a.job.Count()
	a.mu.Unlock()
	if counts.Done != n {
		t.Fatalf("after resume: %+v, want all %d done", counts, n)
	}
	assertNoTempFiles(t, outDir)
}

// TestExportPoolCancelKeepsCompletedOutputs lets three photos finish, then
// cancels while a fourth blocks. The finished outputs must survive and only
// the blocked photo becomes canceled.
func TestExportPoolCancelKeepsCompletedOutputs(t *testing.T) {
	jpeg := testJPEG(t, 640, 480)
	srcDir := t.TempDir()
	outDir := t.TempDir()
	jpegPath := filepath.Join(srcDir, "jpeg.bin")
	if err := os.WriteFile(jpegPath, jpeg, 0o644); err != nil {
		t.Fatal(err)
	}
	bin := writeAppFakeExifTool(t, appInspectionJSON(640, 480, len(jpeg)), jpegPath, "", "", "SLOW")
	t.Setenv(exiftool.EnvExifTool, bin)

	sources := []string{
		filepath.Join(srcDir, "a.CR2"),
		filepath.Join(srcDir, "b.CR2"),
		filepath.Join(srcDir, "c.CR2"),
		filepath.Join(srcDir, "SLOW.CR2"),
	}
	for _, src := range sources {
		if err := os.WriteFile(src, jpeg, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	a := newPoolApp(t, sources, outDir)
	a.StartExport()

	waitFor(t, 60*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.job.Count().Done == 3
	})
	a.Cancel()
	waitFor(t, 60*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return !a.running
	})

	a.mu.Lock()
	counts := a.job.Count()
	canceledHasOutput := false
	for _, item := range a.job.Items {
		if item.Status == model.StatusCanceled && item.OutputPath != "" {
			canceledHasOutput = true
		}
	}
	a.mu.Unlock()
	if counts.Done != 3 || counts.Canceled != 1 {
		t.Fatalf("counts = %+v, want 3 done and 1 canceled", counts)
	}
	if canceledHasOutput {
		t.Error("a canceled item reported a committed output")
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Errorf("outDir has %d entries, want the 3 completed outputs", len(entries))
	}
	assertNoTempFiles(t, outDir)
}

// TestExportPoolAvoidsDuplicateOutputNames gives every photo the same planned
// name while a sentinel already holds it. The pool must commit distinct
// suffixed names and never touch the sentinel.
func TestExportPoolAvoidsDuplicateOutputNames(t *testing.T) {
	jpeg := testJPEG(t, 320, 240)
	srcDir := t.TempDir()
	outDir := t.TempDir()
	jpegPath := filepath.Join(srcDir, "jpeg.bin")
	if err := os.WriteFile(jpegPath, jpeg, 0o644); err != nil {
		t.Fatal(err)
	}
	bin := writeAppFakeExifTool(t, appInspectionJSON(320, 240, len(jpeg)), jpegPath, "", "", "")
	t.Setenv(exiftool.EnvExifTool, bin)

	sentinel := filepath.Join(outDir, "shot.jpg")
	if err := os.WriteFile(sentinel, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	const n = 3
	sources := writeSources(t, srcDir, n, jpeg)
	a := newPoolApp(t, sources, outDir)
	a.mu.Lock()
	for _, item := range a.job.Items {
		item.OutputName = "shot.jpg"
	}
	a.mu.Unlock()

	a.StartExport()
	waitFor(t, 60*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return !a.running
	})

	a.mu.Lock()
	counts := a.job.Count()
	names := make(map[string]bool)
	for _, item := range a.job.Items {
		if item.OutputName != "" {
			names[item.OutputName] = true
		}
	}
	a.mu.Unlock()
	if counts.Done != n {
		t.Fatalf("counts = %+v, want %d done", counts, n)
	}
	if len(names) != n {
		t.Errorf("got %d distinct output names, want %d: %v", len(names), n, names)
	}
	got, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "precious" {
		t.Errorf("sentinel was modified: %q", got)
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != n+1 {
		t.Errorf("outDir has %d entries, want %d (sentinel + outputs)", len(entries), n+1)
	}
	assertNoTempFiles(t, outDir)
}
