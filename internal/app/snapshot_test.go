package app

import (
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
)

// TestSnapshots renders the main screens to PNG files so a person can review
// the actual pixels, for example after changing the view. It does nothing
// unless RAWDROP_SNAPSHOTS names a directory to write into:
//
//	RAWDROP_SNAPSHOTS=/tmp/shots GOTOOLCHAIN=go1.27.1 \
//	  go test -run TestSnapshots ./internal/app/
func TestSnapshots(t *testing.T) {
	dir := os.Getenv("RAWDROP_SNAPSHOTS")
	if dir == "" {
		t.Skip("set RAWDROP_SNAPSHOTS to a directory to write UI snapshots")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	a := New()
	tester := ui.NewTester(a.View, 1440, 900)
	tester.Frame()
	writePNG(t, filepath.Join(dir, "welcome.png"), tester.Image())

	// A real batch, so the grid shows real thumbnails. The sample folder is
	// this machine's; the test skips elsewhere.
	sample := "/home/kshitij/Pictures/dashain photos"
	files := []string{"IMG_3128.CR2", "IMG_3129.CR2", "IMG_3144.CR2", "IMG_3145.CR2"}
	paths := make([]string, 0, len(files))
	for _, name := range files {
		p := filepath.Join(sample, name)
		if _, err := os.Stat(p); err != nil {
			t.Skipf("sample photo missing: %v", err)
		}
		paths = append(paths, p)
	}

	zone, ok := tester.Find("Drop RAW photos here")
	if !ok {
		t.Fatal("the welcome screen did not render its drop zone")
	}
	if !tester.DropFiles(zone.X+zone.W/2, zone.Y+zone.H/2, paths...) {
		t.Fatal("the drop zone did not take the files")
	}
	a.SetOutputDir(filepath.Join(t.TempDir(), "JPEG - Full Resolution"))
	tester.Frame()

	// Thumbnails arrive in the background; wait so the grid shows photos.
	deadline := time.Now().Add(90 * time.Second)
	for {
		a.mu.Lock()
		ready, total := 0, len(a.job.Items)
		for _, item := range a.job.Items {
			if len(item.Thumbnail) > 0 {
				ready++
			}
		}
		a.mu.Unlock()
		if ready == total {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("thumbnails did not arrive: %d of %d", ready, total)
		}
		time.Sleep(100 * time.Millisecond)
	}
	tester.Frame()
	writePNG(t, filepath.Join(dir, "grid.png"), tester.Image())

	tester.SetDark(true)
	tester.Frame()
	writePNG(t, filepath.Join(dir, "grid-dark.png"), tester.Image())
	tester.SetDark(false)
	tester.Frame()

	if err := tester.Click("Export 4 photo(s)"); err != nil {
		t.Fatalf("could not press Export: %v", err)
	}
	// Capture a running frame that has a real progress line and an active
	// photo rather than an empty start.
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		tester.Frame()
		if tester.HasText("Extracting the camera JPEG…") || tester.HasText("Reading camera metadata…") ||
			tester.HasText("Correcting orientation losslessly…") {
			break
		}
		a.mu.Lock()
		terminal := false
		for _, item := range a.job.Items {
			if item.Status.Terminal() {
				terminal = true
				break
			}
		}
		a.mu.Unlock()
		if terminal {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	tester.Frame()
	writePNG(t, filepath.Join(dir, "exporting.png"), tester.Image())

	a.Cancel()
	deadline = time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		running := a.running
		a.mu.Unlock()
		if !running {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func writePNG(t *testing.T, path string, img *image.RGBA) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s (%dx%d)", path, img.Bounds().Dx(), img.Bounds().Dy())
}
