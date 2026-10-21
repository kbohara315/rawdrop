package raw

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"rawdrop/internal/exiftool"
)

// fakeRunner is an in-process exiftool.Runner. It answers the inspection query
// and every metadata read, and streams jpeg for an extraction, so the actual
// raw inspection/export path can run without starting a subprocess.
type fakeRunner struct {
	inspectJSON []byte
	jpeg        []byte

	// orient is the EXIF orientation the fake reports for a finished JPEG. A
	// -Orientation#=n write updates it.
	orient int

	// onInspect, when set, runs while the inspection query is being answered.
	onInspect func()
	// onStream, when set, runs while an extraction is being streamed.
	onStream func()

	streamCalls  int
	captureCalls int
}

var _ exiftool.Runner = (*fakeRunner)(nil)

func (f *fakeRunner) Stream(ctx context.Context, args []string, w io.Writer) error {
	f.streamCalls++
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.onStream != nil {
		f.onStream()
	}
	_, err := w.Write(f.jpeg)
	return err
}

func (f *fakeRunner) Capture(ctx context.Context, args []string, limit int64) ([]byte, error) {
	f.captureCalls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if slices.Contains(args, "-ImageWidth") {
		if f.onInspect != nil {
			f.onInspect()
		}
		return f.inspectJSON, nil
	}
	for _, arg := range args {
		if n, ok := strings.CutPrefix(arg, "-Orientation#="); ok {
			value, err := strconv.Atoi(n)
			if err != nil {
				return nil, fmt.Errorf("fake runner: bad orientation %q", arg)
			}
			f.orient = value
			return nil, nil
		}
	}
	return []byte(fmt.Sprintf(`[{"IFD0:Orientation":%d}]`, f.orient)), nil
}

func (f *fakeRunner) Prefix(ctx context.Context, args []string, limit int64) ([]byte, error) {
	return nil, errors.New("fake runner: prefix is not expected when the candidate has offsets")
}

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

// testInspectionJSON makes the source itself the embedded JPEG: a candidate at
// offset 0 covering the whole file, so probing reads dimensions from the source.
func testInspectionJSON(w, h, orientation, length int) []byte {
	return []byte(fmt.Sprintf(`[{"IFD0:ImageWidth":%d,"IFD0:ImageHeight":%d,"IFD0:Orientation":%d,`+
		`"IFD0:PreviewImage":"(Binary data %d bytes, use -b option to extract)",`+
		`"IFD0:PreviewImageStart":0,"IFD0:PreviewImageLength":%d}]`,
		w, h, orientation, length, length))
}

// writeRawSource writes data to a .CR2 file in a fresh temp directory.
func writeRawSource(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "photo.CR2")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	return path
}

// newFakeRunner builds a fake for a JPEG whose stored size is w x h.
func newFakeRunner(jpeg []byte, w, h, orientation int) *fakeRunner {
	return &fakeRunner{
		inspectJSON: testInspectionJSON(w, h, orientation, len(jpeg)),
		jpeg:        jpeg,
		orient:      1,
	}
}

// appendSource appends bytes to path, changing its size and mtime.
func appendSource(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open source for mutation: %v", err)
	}
	if _, err := f.Write([]byte("mutated")); err != nil {
		f.Close()
		t.Fatalf("mutate source: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close mutated source: %v", err)
	}
}

// TestInspectionRunsWithFakeRunner proves the actual raw caller executes
// through an injected Runner with no subprocess: PATH has no helpers, yet
// inspection and export both succeed.
func TestInspectionRunsWithFakeRunner(t *testing.T) {
	jpeg := testJPEG(t, 640, 480)
	src := writeRawSource(t, jpeg)
	fake := newFakeRunner(jpeg, 640, 480, 1)
	t.Setenv("PATH", t.TempDir())

	ctx := context.Background()
	insp, err := inspectFile(ctx, src, fake)
	if err != nil {
		t.Fatalf("inspectFile() error: %v", err)
	}
	if got := insp.Info(); got.Width != 640 || got.Height != 480 || got.Orientation != 1 {
		t.Fatalf("Info() = %+v, want 640x480 orientation 1", got)
	}

	var stages []Stage
	res, err := insp.Export(ctx, filepath.Join(t.TempDir(), "out.jpg"), Options{
		Progress: func(s Stage) { stages = append(stages, s) },
	})
	if err != nil {
		t.Fatalf("Export() error: %v", err)
	}
	if res.Width != 640 || res.Height != 480 || res.Orientation != 1 {
		t.Errorf("Export() = %+v, want 640x480 orientation 1", res)
	}
	if fake.streamCalls != 1 {
		t.Errorf("stream extraction calls = %d, want 1", fake.streamCalls)
	}
	// Orientation 1 needs no rotation, so the rotate stage must not appear and
	// extraction must precede verification.
	if want := []Stage{StageExtracting, StageVerifying}; !slices.Equal(stages, want) {
		t.Errorf("stages = %v, want %v", stages, want)
	}
}

// TestExportReportsStagesInOrder proves extraction is reported before rotation
// and rotation before verification. The rotation cannot be applied because no
// jpegtran is configured, which is fine: the stage still describes the real
// attempt.
func TestExportReportsStagesInOrder(t *testing.T) {
	jpeg := testJPEG(t, 640, 480)
	src := writeRawSource(t, jpeg)
	fake := newFakeRunner(jpeg, 640, 480, 3)
	t.Setenv(envJpegTran, filepath.Join(t.TempDir(), "missing-jpegtran"))

	insp, err := inspectFile(context.Background(), src, fake)
	if err != nil {
		t.Fatalf("inspectFile() error: %v", err)
	}
	var stages []Stage
	res, err := insp.Export(context.Background(), filepath.Join(t.TempDir(), "out.jpg"), Options{
		CorrectOrientation: true,
		Progress:           func(s Stage) { stages = append(stages, s) },
	})
	if err != nil {
		t.Fatalf("Export() error: %v", err)
	}
	if res.Rotated {
		t.Error("Export reported a physical rotation without jpegtran")
	}
	if want := []Stage{StageExtracting, StageRotating, StageVerifying}; !slices.Equal(stages, want) {
		t.Fatalf("stages = %v, want %v (extraction before rotation)", stages, want)
	}
}

// TestInspectFileRejectsSourceChangedDuringInspection mutates the source while
// the inspection query is running. The post-inspection stat must reject it.
func TestInspectFileRejectsSourceChangedDuringInspection(t *testing.T) {
	jpeg := testJPEG(t, 640, 480)
	src := writeRawSource(t, jpeg)
	fake := newFakeRunner(jpeg, 640, 480, 1)
	fake.onInspect = func() { appendSource(t, src) }

	_, err := inspectFile(context.Background(), src, fake)
	if !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("inspectFile() error = %v, want ErrSourceChanged", err)
	}
}

// TestInspectionRejectsChangeBeforeExport changes the source after inspection
// and confirms Export refuses to use the stale candidate offsets.
func TestInspectionRejectsChangeBeforeExport(t *testing.T) {
	jpeg := testJPEG(t, 640, 480)
	src := writeRawSource(t, jpeg)
	fake := newFakeRunner(jpeg, 640, 480, 1)

	insp, err := inspectFile(context.Background(), src, fake)
	if err != nil {
		t.Fatalf("inspectFile() error: %v", err)
	}
	appendSource(t, src)

	tmp := filepath.Join(t.TempDir(), "out.jpg")
	if _, err := insp.Export(context.Background(), tmp, Options{}); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("Export() error = %v, want ErrSourceChanged", err)
	}
	if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Export left a temp file behind after rejecting the source")
	}
}

// TestInspectionRejectsChangeDuringExtraction changes the source while its
// embedded JPEG is being streamed. Export must re-stat after extraction and
// refuse the extracted bytes instead of continuing to commit them.
func TestInspectionRejectsChangeDuringExtraction(t *testing.T) {
	jpeg := testJPEG(t, 640, 480)
	src := writeRawSource(t, jpeg)
	fake := newFakeRunner(jpeg, 640, 480, 1)

	insp, err := inspectFile(context.Background(), src, fake)
	if err != nil {
		t.Fatalf("inspectFile() error: %v", err)
	}
	fake.onStream = func() { appendSource(t, src) }

	tmp := filepath.Join(t.TempDir(), "out.jpg")
	if _, err := insp.Export(context.Background(), tmp, Options{}); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("Export() error = %v, want ErrSourceChanged", err)
	}
	if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Export left a temp file behind after rejecting the source")
	}
}
