package exporter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"rawdrop/internal/exiftool"
	"rawdrop/internal/raw"
)

// encodeJPEG returns a complete baseline JPEG of the given size.
func encodeJPEG(t *testing.T, w, h int) []byte {
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

// inspectionJSON describes a source that is itself a JPEG with an embedded
// PreviewImage at offset 0 covering the whole file. Reporting the offset lets
// inspection read the dimensions straight from the source, so the fake helper
// only has to serve one binary extraction.
func inspectionJSON(w, h, length int) string {
	return fmt.Sprintf(`[{"IFD0:ImageWidth":%d,"IFD0:ImageHeight":%d,"IFD0:Orientation":1,`+
		`"IFD0:PreviewImage":"(Binary data %d bytes, use -b option to extract)",`+
		`"IFD0:PreviewImageStart":0,"IFD0:PreviewImageLength":%d}]`, w, h, length, length)
}

// writeFakeExifTool writes an executable helper that answers -j with meta and
// every other invocation with extractBody. It appends a line to logPath for
// every invocation that carries -ImageWidth, which only the one inspection
// call does, so a test can count inspections. An empty logPath disables
// logging.
func writeFakeExifTool(t *testing.T, meta, extractBody, logPath string) string {
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
		logLine = "    if [ \"$arg\" = \"-ImageWidth\" ]; then echo inspect >> '" + logPath + "'; fi\n"
	}
	body := "#!/" + sh + "\n" +
		"json=0\n" +
		"for arg in \"$@\"; do\n" +
		"    if [ \"$arg\" = \"-j\" ]; then json=1; fi\n" +
		logLine +
		"done\n" +
		"if [ \"$json\" = \"1\" ]; then\n" +
		"    printf '%s' '" + meta + "'\n" +
		"else\n" +
		extractBody +
		"fi\n"
	path := filepath.Join(t.TempDir(), "fake-exiftool")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake helper: %v", err)
	}
	return path
}

// writeSource writes one source file holding the JPEG bytes.
func writeSource(t *testing.T, dir, name string, jpeg []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, jpeg, 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	return path
}

func countInspections(t *testing.T, logPath string) int {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0
		}
		t.Fatalf("read inspection log: %v", err)
	}
	return strings.Count(string(data), "inspect")
}

// TestExportCommitsVerifiedOutput checks the happy path end to end: one
// inspection per source, a byte-identical copy of the embedded JPEG (no GPS,
// no rotation), a matching result hash, 0644 permissions, an unchanged source,
// and no leftover temp file.
func TestExportCommitsVerifiedOutput(t *testing.T) {
	jpeg := encodeJPEG(t, 640, 480)
	srcDir := t.TempDir()
	outDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "inspect.log")
	bin := writeFakeExifTool(t, inspectionJSON(640, 480, len(jpeg)), "    cat '"+filepath.Join(srcDir, "jpeg.bin")+"'\n", logPath)
	if err := os.WriteFile(filepath.Join(srcDir, "jpeg.bin"), jpeg, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(exiftool.EnvExifTool, bin)

	sources := []string{
		writeSource(t, srcDir, "a.CR2", jpeg),
		writeSource(t, srcDir, "b.CR2", jpeg),
		writeSource(t, srcDir, "c.CR2", jpeg),
	}
	before := make(map[string]string, len(sources))
	for _, src := range sources {
		before[src], _ = fileSHA(t, src)
	}

	for _, src := range sources {
		res, err := Export(context.Background(), src, Options{OutputDir: outDir})
		if err != nil {
			t.Fatalf("Export(%s) error: %v", src, err)
		}
		if res.Width != 640 || res.Height != 480 || res.Orientation != 1 {
			t.Errorf("Export(%s) result = %+v, want 640x480 orientation 1", src, res)
		}
		got, err := os.ReadFile(res.OutputPath)
		if err != nil {
			t.Fatalf("read output: %v", err)
		}
		if !bytes.Equal(got, jpeg) {
			t.Errorf("output %s differs from the embedded JPEG", res.OutputPath)
		}
		sum, _ := fileSHA(t, res.OutputPath)
		if res.SHA256 != sum {
			t.Errorf("result SHA256 = %s, want %s", res.SHA256, sum)
		}
		st, err := os.Stat(res.OutputPath)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o644 {
			t.Errorf("%s has mode %v, want 644", res.OutputPath, st.Mode().Perm())
		}
	}

	if got := countInspections(t, logPath); got != len(sources) {
		t.Errorf("inspection count = %d, want exactly %d (one per source)", got, len(sources))
	}
	for _, src := range sources {
		after, _ := fileSHA(t, src)
		if after != before[src] {
			t.Errorf("source %s changed", src)
		}
	}
	assertNoTemps(t, outDir)
}

// TestExportReportsNoFullJPEG checks the typed skip classification and that no
// visible or temporary file is left behind.
func TestExportReportsNoFullJPEG(t *testing.T) {
	srcDir := t.TempDir()
	outDir := t.TempDir()
	bin := writeFakeExifTool(t, `[{"IFD0:ImageWidth":640,"IFD0:ImageHeight":480,"IFD0:Orientation":1}]`, "    cat /dev/null\n", "")
	t.Setenv(exiftool.EnvExifTool, bin)

	src := writeSource(t, srcDir, "plain.CR2", []byte("not really raw"))
	_, err := Export(context.Background(), src, Options{OutputDir: outDir})
	if !errors.Is(err, raw.ErrNoFullJPEG) {
		t.Fatalf("Export() error = %v, want ErrNoFullJPEG", err)
	}
	assertNoTemps(t, outDir)
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("outDir has %d entries after a skip, want 0", len(entries))
	}
}

// TestExportCancellation checks that canceling the context stops a running
// extraction, surfaces as context.Canceled, and leaves nothing behind.
func TestExportCancellation(t *testing.T) {
	jpeg := encodeJPEG(t, 640, 480)
	srcDir := t.TempDir()
	outDir := t.TempDir()
	bin := writeFakeExifTool(t, inspectionJSON(640, 480, len(jpeg)), "    exec sleep 30\n", "")
	t.Setenv(exiftool.EnvExifTool, bin)

	src := writeSource(t, srcDir, "slow.CR2", jpeg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	_, err := Export(ctx, src, Options{OutputDir: outDir})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Export() error = %v, want context.Canceled", err)
	}
	assertNoTemps(t, outDir)
}

// TestExportAvoidsOverwritingExistingOutput plants a sentinel under the
// preferred name and checks that the export goes to a suffixed name instead.
func TestExportAvoidsOverwritingExistingOutput(t *testing.T) {
	jpeg := encodeJPEG(t, 640, 480)
	srcDir := t.TempDir()
	outDir := t.TempDir()
	bin := writeFakeExifTool(t, inspectionJSON(640, 480, len(jpeg)), "    cat '"+filepath.Join(srcDir, "jpeg.bin")+"'\n", "")
	if err := os.WriteFile(filepath.Join(srcDir, "jpeg.bin"), jpeg, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(exiftool.EnvExifTool, bin)

	sentinel := filepath.Join(outDir, "shot.jpg")
	if err := os.WriteFile(sentinel, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := writeSource(t, srcDir, "shot.CR2", jpeg)
	res, err := Export(context.Background(), src, Options{OutputDir: outDir, OutputName: "shot.jpg"})
	if err != nil {
		t.Fatalf("Export() error: %v", err)
	}
	if res.OutputName == "shot.jpg" {
		t.Errorf("Export reused the taken name %q", res.OutputName)
	}
	got, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "precious" {
		t.Errorf("sentinel was modified: %q", got)
	}
}

// TestExportConcurrentCollisions runs many exports of the same preferred name
// at once. Every one must commit a distinct output and the sentinel must
// survive.
func TestExportConcurrentCollisions(t *testing.T) {
	const n = 8
	jpeg := encodeJPEG(t, 320, 240)
	srcDir := t.TempDir()
	outDir := t.TempDir()
	// One shared helper that always streams the same JPEG.
	jpegPath := filepath.Join(srcDir, "jpeg.bin")
	if err := os.WriteFile(jpegPath, jpeg, 0o644); err != nil {
		t.Fatal(err)
	}
	bin := writeFakeExifTool(t, inspectionJSON(320, 240, len(jpeg)), "    cat '"+jpegPath+"'\n", "")
	t.Setenv(exiftool.EnvExifTool, bin)

	sentinel := filepath.Join(outDir, "shot.jpg")
	if err := os.WriteFile(sentinel, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	sources := make([]string, n)
	for i := range sources {
		sources[i] = writeSource(t, srcDir, fmt.Sprintf("shot%02d.CR2", i), jpeg)
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		outputs = map[string]bool{}
		failed  []error
	)
	for _, src := range sources {
		src := src
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := Export(context.Background(), src, Options{OutputDir: outDir, OutputName: "shot.jpg"})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed = append(failed, err)
				return
			}
			if outputs[res.OutputName] {
				failed = append(failed, fmt.Errorf("duplicate output name %q", res.OutputName))
			}
			outputs[res.OutputName] = true
		}()
	}
	wg.Wait()
	if len(failed) != 0 {
		t.Fatalf("concurrent exports failed: %v", failed)
	}
	if len(outputs) != n {
		t.Fatalf("got %d distinct outputs, want %d", len(outputs), n)
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
	assertNoTemps(t, outDir)
}

// TestExportPrecommitFailureLeavesNoOutput injects a failure in the pre-commit
// step (flush/chmod/hash) after the temp file was written but before the
// no-replace link. A failed export must leave no visible JPEG and no temp.
func TestExportPrecommitFailureLeavesNoOutput(t *testing.T) {
	jpeg := encodeJPEG(t, 640, 480)
	srcDir := t.TempDir()
	outDir := t.TempDir()
	jpegPath := filepath.Join(srcDir, "jpeg.bin")
	if err := os.WriteFile(jpegPath, jpeg, 0o644); err != nil {
		t.Fatal(err)
	}
	bin := writeFakeExifTool(t, inspectionJSON(640, 480, len(jpeg)), "    cat '"+jpegPath+"'\n", "")
	t.Setenv(exiftool.EnvExifTool, bin)
	src := writeSource(t, srcDir, "a.CR2", jpeg)

	injected := errors.New("injected pre-commit failure")
	_, err := export(context.Background(), src, Options{OutputDir: outDir}, func(string) (string, error) {
		return "", injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("export() error = %v, want the injected pre-commit failure", err)
	}
	entries, readErr := os.ReadDir(outDir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Errorf("outDir has %d entries after a pre-commit failure, want 0", len(entries))
	}
	assertNoTemps(t, outDir)
}

// TestExportReportsNoRotationWhenNotNeeded checks the progress stages for an
// upright source: inspecting then extracting, with no rotation stage.
func TestExportReportsNoRotationWhenNotNeeded(t *testing.T) {
	jpeg := encodeJPEG(t, 640, 480)
	srcDir := t.TempDir()
	outDir := t.TempDir()
	jpegPath := filepath.Join(srcDir, "jpeg.bin")
	if err := os.WriteFile(jpegPath, jpeg, 0o644); err != nil {
		t.Fatal(err)
	}
	bin := writeFakeExifTool(t, inspectionJSON(640, 480, len(jpeg)), "    cat '"+jpegPath+"'\n", "")
	t.Setenv(exiftool.EnvExifTool, bin)
	src := writeSource(t, srcDir, "a.CR2", jpeg)

	var stages []Stage
	if _, err := Export(context.Background(), src, Options{
		OutputDir:          outDir,
		CorrectOrientation: true,
		Progress:           func(p Progress) { stages = append(stages, p.Stage) },
	}); err != nil {
		t.Fatalf("Export() error: %v", err)
	}
	if want := []Stage{StageInspect, StageExtract}; !slices.Equal(stages, want) {
		t.Errorf("stages = %v, want %v", stages, want)
	}
}

func assertNoTemps(t *testing.T, dir string) {
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

func fileSHA(t *testing.T, path string) (string, error) {
	t.Helper()
	return hashFile(path)
}
