package raw

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"rawdrop/internal/model"
)

// Integration tests use the real Canon EOS 80D sample photos when they are
// available. The folder is read-only for these tests: every output goes to
// t.TempDir(). Set RAWDROP_TEST_PHOTOS to point at another sample folder.
const defaultSampleDir = "/home/kshitij/Pictures/dashain photos"

func sampleDir() string {
	if dir := os.Getenv("RAWDROP_TEST_PHOTOS"); dir != "" {
		return dir
	}
	return defaultSampleDir
}

// requireExifTool skips the test when exiftool is unavailable.
func requireExifTool(t *testing.T) string {
	t.Helper()
	bin, err := exiftoolPath()
	if err != nil {
		t.Skipf("exiftool not available: %v", err)
	}
	return bin
}

// requireJpegTran skips the test when jpegtran is unavailable.
func requireJpegTran(t *testing.T) string {
	t.Helper()
	bin, err := jpegtranPath()
	if err != nil {
		t.Skipf("jpegtran not available: %v", err)
	}
	return bin
}

// samplePhoto returns the path of one sample photo, skipping the test when
// the sample folder or file is missing.
func samplePhoto(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(sampleDir(), name)
	if _, err := os.Stat(path); err != nil {
		t.Skipf("sample photo %s not available: %v", path, err)
	}
	return path
}

// statFile records a file's size, modification time and SHA-256 so a test can
// prove a helper never touched it.
type fileState struct {
	size    int64
	modTime time.Time
	sha256  string
}

func statFile(t *testing.T, path string) fileState {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hash %s: %v", path, err)
	}
	return fileState{size: info.Size(), modTime: info.ModTime(), sha256: hex.EncodeToString(h.Sum(nil))}
}

func assertFileUnchanged(t *testing.T, path string, before fileState) {
	t.Helper()
	after := statFile(t, path)
	if after.size != before.size {
		t.Errorf("%s size changed: %d -> %d", path, before.size, after.size)
	}
	if !after.modTime.Equal(before.modTime) {
		t.Errorf("%s modification time changed: %v -> %v", path, before.modTime, after.modTime)
	}
	if after.sha256 != before.sha256 {
		t.Errorf("%s content changed: %s -> %s", path, before.sha256, after.sha256)
	}
}

func writeJPEGFile(t *testing.T, path string, w, h int) {
	t.Helper()
	if err := os.WriteFile(path, encodeJPEG(t, w, h), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatalf("open %s: %v", src, err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatalf("create %s: %v", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		t.Fatalf("copy %s -> %s: %v", src, dst, err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("close %s: %v", dst, err)
	}
}

func TestCheckTools(t *testing.T) {
	requireExifTool(t)
	requireJpegTran(t)
	if err := CheckTools(); err != nil {
		t.Fatalf("CheckTools() = %v, want nil", err)
	}
}

// TestInspectAndExportRealPortrait exercises the full export of a real
// portrait CR2 whose embedded preview needs orientation 8, using the real
// jpegtran lossless rotation.
func TestInspectAndExportRealPortrait(t *testing.T) {
	requireExifTool(t)
	requireJpegTran(t)
	src := samplePhoto(t, "IMG_3128.CR2")
	before := statFile(t, src)

	info, err := Inspect(context.Background(), src)
	if err != nil {
		t.Fatalf("Inspect() error: %v", err)
	}
	if info.Width != 6000 || info.Height != 4000 {
		t.Errorf("Inspect dimensions = %dx%d, want 6000x4000", info.Width, info.Height)
	}
	if info.Orientation != 8 {
		t.Errorf("Inspect orientation = %d, want 8", info.Orientation)
	}
	if info.Make != "Canon" || info.Model != "Canon EOS 80D" {
		t.Errorf("Inspect make/model = %q %q, want Canon / Canon EOS 80D", info.Make, info.Model)
	}
	if !info.HasPreview || info.PreviewTag != "PreviewImage" {
		t.Errorf("Inspect preview = has %v, tag %q; want true, PreviewImage", info.HasPreview, info.PreviewTag)
	}
	if info.PreviewWidth != 6000 || info.PreviewHeight != 4000 {
		t.Errorf("Inspect preview dims = %dx%d, want 6000x4000", info.PreviewWidth, info.PreviewHeight)
	}

	out := filepath.Join(t.TempDir(), "IMG_3128.jpg")
	res, err := Export(context.Background(), src, out, Options{CorrectOrientation: true})
	if err != nil {
		t.Fatalf("Export() error: %v", err)
	}
	if !res.Rotated {
		t.Error("Export result Rotated = false, want true")
	}
	if res.Orientation != 1 {
		t.Errorf("Export result Orientation = %d, want 1", res.Orientation)
	}
	if res.Width != 4000 || res.Height != 6000 {
		t.Errorf("Export result dims = %dx%d, want 4000x6000", res.Width, res.Height)
	}
	if res.Warning != "" {
		t.Errorf("Export warning = %q, want none", res.Warning)
	}
	if !res.MetadataCleaned {
		t.Error("Export MetadataCleaned = false, want true")
	}

	w, h, err := validateJPEGFile(out)
	if err != nil {
		t.Fatalf("output is not a valid JPEG: %v", err)
	}
	if w != 4000 || h != 6000 {
		t.Errorf("output dimensions = %dx%d, want 4000x6000", w, h)
	}
	outInfo, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat output: %v", err)
	}
	if outInfo.Size() < 1<<20 {
		t.Errorf("output size = %d bytes, want a full-size JPEG over 1 MiB", outInfo.Size())
	}
	orientation, gps, err := readOrientationAndGPS(context.Background(), mustExifTool(t), out)
	if err != nil {
		t.Fatalf("read output metadata: %v", err)
	}
	if orientation != 1 {
		t.Errorf("output EXIF orientation = %d, want 1", orientation)
	}
	if len(gps) != 0 {
		t.Errorf("output still has location metadata: %v", gps)
	}

	t.Logf("%s: %dx%d orientation %d, preview %s %dx%d",
		filepath.Base(src), info.Width, info.Height, info.Orientation, info.PreviewTag, info.PreviewWidth, info.PreviewHeight)
	t.Logf("export: %dx%d orientation %d rotated=%v warning=%q", res.Width, res.Height, res.Orientation, res.Rotated, res.Warning)
	t.Logf("output: %s (%d bytes), EXIF orientation %d", filepath.Base(out), outInfo.Size(), orientation)
	assertFileUnchanged(t, src, before)
	t.Logf("source unchanged: size %d, sha256 %s", before.size, before.sha256[:16])
}

// TestExportRealLandscape finds a real orientation-1 sample and checks that
// no rotation is attempted and the extracted bytes are passed through
// unchanged.
func TestExportRealLandscape(t *testing.T) {
	requireExifTool(t)
	requireJpegTran(t)
	bin := mustExifTool(t)

	entries, err := os.ReadDir(sampleDir())
	if err != nil {
		t.Skipf("sample folder not readable: %v", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	const maxProbe = 40
	var (
		src   string
		info  Info
		probe int
	)
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".cr2") {
			continue
		}
		if probe >= maxProbe {
			break
		}
		probe++
		candidate := filepath.Join(sampleDir(), entry.Name())
		probed, err := Inspect(context.Background(), candidate)
		if err != nil {
			continue
		}
		if probed.Orientation == 1 && probed.HasPreview {
			src, info = candidate, probed
			break
		}
	}
	if src == "" {
		t.Skip("no landscape orientation-1 sample found")
	}
	if needsRotation(info.Orientation, true) {
		t.Fatalf("landscape sample unexpectedly needs rotation: %+v", info)
	}
	before := statFile(t, src)

	out := filepath.Join(t.TempDir(), "landscape.jpg")
	res, err := Export(context.Background(), src, out, Options{CorrectOrientation: true})
	if err != nil {
		t.Fatalf("Export(%s) error: %v", src, err)
	}
	if res.Rotated {
		t.Error("Rotated = true, want false for orientation 1")
	}
	if res.Orientation != 1 {
		t.Errorf("Orientation = %d, want 1", res.Orientation)
	}
	if res.Warning != "" {
		t.Errorf("Warning = %q, want none", res.Warning)
	}
	if res.Width != info.PreviewWidth || res.Height != info.PreviewHeight {
		t.Errorf("dimensions = %dx%d, want preview size %dx%d", res.Width, res.Height, info.PreviewWidth, info.PreviewHeight)
	}
	if w, h, err := validateJPEGFile(out); err != nil {
		t.Fatalf("output is not a valid JPEG: %v", err)
	} else if w != res.Width || h != res.Height {
		t.Errorf("output decodes to %dx%d, want %dx%d", w, h, res.Width, res.Height)
	}

	// With no rotation and no GPS to remove, the output must be the embedded
	// JPEG byte for byte: extraction never re-encodes.
	extracted, err := captureTool(context.Background(), bin, []string{"-b", "-PreviewImage", "--", src})
	if err != nil {
		t.Fatalf("extract preview for comparison: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !bytes.Equal(extracted, got) {
		t.Errorf("output differs from the embedded JPEG (%d bytes vs %d bytes)", len(got), len(extracted))
	}
	assertFileUnchanged(t, src, before)
	t.Logf("%s: %dx%d orientation %d, preview %s %dx%d",
		filepath.Base(src), info.Width, info.Height, info.Orientation, info.PreviewTag, info.PreviewWidth, info.PreviewHeight)
	t.Logf("export: %dx%d orientation %d rotated=%v warning=%q", res.Width, res.Height, res.Orientation, res.Rotated, res.Warning)
	t.Logf("output: %s (%d bytes), byte-identical to embedded PreviewImage", filepath.Base(out), len(got))
	t.Logf("source unchanged: size %d, sha256 %s", before.size, before.sha256[:16])
}

// TestApplyMetadataPerfectFallback uses a small JPEG whose dimensions are not
// multiples of the MCU, so jpegtran -perfect refuses the rotation. The file
// must stay untouched, keep its orientation metadata, and produce a warning.
func TestApplyMetadataPerfectFallback(t *testing.T) {
	requireExifTool(t)
	requireJpegTran(t)
	path := filepath.Join(t.TempDir(), "unaligned.jpg")
	writeJPEGFile(t, path, 33, 17)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := validateJPEGFile(path); err != nil {
		t.Fatalf("test fixture is not a valid JPEG: %v", err)
	}

	rotated, cleaned, warning, err := applyMetadata(context.Background(), path, 6, Options{CorrectOrientation: true})
	if err != nil {
		t.Fatalf("applyMetadata() error: %v", err)
	}
	if rotated {
		t.Error("rotated = true, want false for a non-MCU-aligned JPEG")
	}
	if warning == "" {
		t.Error("warning = empty, want an explanation of the skipped rotation")
	}
	if !cleaned {
		t.Error("cleaned = false, want true")
	}
	orientation, gps, err := readOrientationAndGPS(context.Background(), mustExifTool(t), path)
	if err != nil {
		t.Fatalf("read orientation: %v", err)
	}
	if orientation != 6 {
		t.Errorf("orientation = %d, want the original 6 retained", orientation)
	}
	if len(gps) != 0 {
		t.Errorf("location metadata appeared: %v", gps)
	}
	if w, h, err := validateJPEGFile(path); err != nil {
		t.Fatalf("file no longer a valid JPEG: %v", err)
	} else if w != 33 || h != 17 {
		t.Errorf("dimensions = %dx%d, want the untouched 33x17", w, h)
	}
	// Only metadata may change: the compressed scan data must be untouched.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(jpegScanData(t, before), jpegScanData(t, after)) {
		t.Error("compressed image data changed even though no rotation was applied")
	}
}

// TestApplyMetadataRotatesSynthetic checks the successful lossless path on a
// small MCU-aligned JPEG.
func TestApplyMetadataRotatesSynthetic(t *testing.T) {
	requireExifTool(t)
	requireJpegTran(t)
	path := filepath.Join(t.TempDir(), "aligned.jpg")
	writeJPEGFile(t, path, 64, 48)

	rotated, _, warning, err := applyMetadata(context.Background(), path, 6, Options{CorrectOrientation: true})
	if err != nil {
		t.Fatalf("applyMetadata() error: %v", err)
	}
	if !rotated {
		t.Fatalf("rotated = false, want true (warning: %s)", warning)
	}
	if warning != "" {
		t.Errorf("warning = %q, want none", warning)
	}
	if w, h, err := validateJPEGFile(path); err != nil {
		t.Fatalf("rotated file is not a valid JPEG: %v", err)
	} else if w != 48 || h != 64 {
		t.Errorf("dimensions = %dx%d, want rotated 48x64", w, h)
	}
	orientation, _, err := readOrientationAndGPS(context.Background(), mustExifTool(t), path)
	if err != nil {
		t.Fatalf("read orientation: %v", err)
	}
	if orientation != 1 {
		t.Errorf("orientation = %d, want 1 after rotation", orientation)
	}
}

// TestStripLocationRemovesGPS plants real GPS metadata in a JPEG and checks
// that stripping leaves none behind.
func TestStripLocationRemovesGPS(t *testing.T) {
	bin := requireExifTool(t)
	path := filepath.Join(t.TempDir(), "gps.jpg")
	writeJPEGFile(t, path, 64, 48)
	args := []string{
		"-overwrite_original",
		"-GPSLatitude=27.7", "-GPSLongitude=85.3", "-GPSAltitude=1400",
		"-XMP:GPSLatitude=27.7", "-XMP:GPSLongitude=85.3",
		"--", path,
	}
	if _, err := captureTool(context.Background(), bin, args); err != nil {
		t.Fatalf("adding GPS metadata: %v", err)
	}
	if _, gps, err := readOrientationAndGPS(context.Background(), bin, path); err != nil {
		t.Fatalf("read planted GPS: %v", err)
	} else if len(gps) == 0 {
		t.Fatal("planted GPS metadata was not found")
	}
	if err := stripLocation(context.Background(), bin, path); err != nil {
		t.Fatalf("stripLocation() error: %v", err)
	}
	if _, gps, err := readOrientationAndGPS(context.Background(), bin, path); err != nil {
		t.Fatalf("read stripped metadata: %v", err)
	} else if len(gps) != 0 {
		t.Errorf("location metadata remains: %v", gps)
	}
}

// TestExportNoEmbeddedJPEG checks the typed skip error on a file that has no
// embedded JPEG at all, and that the temp path is cleaned up.
func TestExportNoEmbeddedJPEG(t *testing.T) {
	requireExifTool(t)
	src := filepath.Join(t.TempDir(), "plain.jpg")
	writeJPEGFile(t, src, 64, 48)
	out := filepath.Join(t.TempDir(), "out.jpg")
	_, err := Export(context.Background(), src, out, Options{})
	if !errors.Is(err, ErrNoFullJPEG) {
		t.Fatalf("Export() error = %v, want ErrNoFullJPEG", err)
	}
	if _, statErr := os.Stat(out); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("temp output still exists after failure (stat error: %v)", statErr)
	}
}

// TestExportRefusesExistingTemp checks that a taken temp name is never
// replaced.
func TestExportRefusesExistingTemp(t *testing.T) {
	requireExifTool(t)
	src := filepath.Join(t.TempDir(), "plain.jpg")
	writeJPEGFile(t, src, 64, 48)
	out := filepath.Join(t.TempDir(), "out.jpg")
	if err := os.WriteFile(out, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Export(context.Background(), src, out, Options{}); err == nil {
		t.Fatal("Export() = nil error, want a refusal to replace the existing temp")
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "precious" {
		t.Errorf("existing temp was modified: %q", got)
	}
}

// TestExportUnicodePaths copies a real RAW to a folder whose name contains
// spaces and non-ASCII characters and exports it to a similar destination.
func TestExportUnicodePaths(t *testing.T) {
	requireExifTool(t)
	requireJpegTran(t)
	sample := samplePhoto(t, "IMG_3128.CR2")
	base := t.TempDir()
	srcDir := filepath.Join(base, "fotos ñandú 2026")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(srcDir, "IMG 3128 cópia.CR2")
	copyFile(t, sample, src)

	outDir := filepath.Join(base, "salida ¿qué?")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(outDir, "exportación final.jpg")
	res, err := Export(context.Background(), src, out, Options{CorrectOrientation: true})
	if err != nil {
		t.Fatalf("Export() with unicode paths error: %v", err)
	}
	if res.Width != 4000 || res.Height != 6000 || res.Orientation != 1 || !res.Rotated {
		t.Errorf("Export result = %+v, want 4000x6000 orientation 1 rotated", res)
	}
	if w, h, err := validateJPEGFile(out); err != nil {
		t.Fatalf("output is not a valid JPEG: %v", err)
	} else if w != 4000 || h != 6000 {
		t.Errorf("output dimensions = %dx%d, want 4000x6000", w, h)
	}
}

// TestExportRealPortraitWithoutRotation keeps the orientation-8 sample in
// sensor layout and checks that the EXIF orientation tag alone makes it
// upright, with the image data untouched.
func TestExportRealPortraitWithoutRotation(t *testing.T) {
	requireExifTool(t)
	bin := mustExifTool(t)
	src := samplePhoto(t, "IMG_3128.CR2")
	before := statFile(t, src)

	out := filepath.Join(t.TempDir(), "IMG_3128-tagged.jpg")
	res, err := Export(context.Background(), src, out, Options{CorrectOrientation: false})
	if err != nil {
		t.Fatalf("Export() error: %v", err)
	}
	if res.Rotated {
		t.Error("Rotated = true, want false without orientation correction")
	}
	if res.Orientation != 8 {
		t.Errorf("Orientation = %d, want 8", res.Orientation)
	}
	if res.Width != 6000 || res.Height != 4000 {
		t.Errorf("dimensions = %dx%d, want the stored 6000x4000", res.Width, res.Height)
	}
	if res.Warning != "" {
		t.Errorf("Warning = %q, want none", res.Warning)
	}
	orientation, gps, err := readOrientationAndGPS(context.Background(), bin, out)
	if err != nil {
		t.Fatalf("read output metadata: %v", err)
	}
	if orientation != 8 {
		t.Errorf("output EXIF orientation = %d, want 8", orientation)
	}
	if len(gps) != 0 {
		t.Errorf("output still has location metadata: %v", gps)
	}

	extracted, err := captureTool(context.Background(), bin, []string{"-b", "-PreviewImage", "--", src})
	if err != nil {
		t.Fatalf("extract preview for comparison: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(jpegScanData(t, extracted), jpegScanData(t, got)) {
		t.Error("image data changed even though no rotation was requested")
	}
	assertFileUnchanged(t, src, before)
}

// TestInspectCancellation checks that canceling the context stops a running
// helper and surfaces as context.Canceled.
func TestInspectCancellation(t *testing.T) {
	// exec replaces the shell so canceling the command kills the sleep too.
	script := writeFakeTool(t, "exec sleep 30\n")
	t.Setenv(envExifTool, script)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	_, err := Inspect(ctx, filepath.Join(t.TempDir(), "photo.CR2"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Inspect() error = %v, want context.Canceled", err)
	}
}

// TestExportCancellation checks that canceling the context during extraction
// stops the helper and surfaces as context.Canceled, with no temp file left
// behind. The candidate carries offsets so Inspect reads its header straight
// from the source and the first -b call is the extraction itself.
func TestExportCancellation(t *testing.T) {
	dir := t.TempDir()
	jpegPath := filepath.Join(dir, "preview.jpg")
	writeJPEGFile(t, jpegPath, 400, 300)
	jpegData, err := os.ReadFile(jpegPath)
	if err != nil {
		t.Fatal(err)
	}
	srcPath := filepath.Join(dir, "fake.CR2")
	if err := os.WriteFile(srcPath, jpegData, 0o644); err != nil {
		t.Fatal(err)
	}
	meta := fmt.Sprintf(`[{"SourceFile":"fake.CR2","IFD0:ImageWidth":400,"IFD0:ImageHeight":300,`+
		`"IFD0:Orientation":1,`+
		`"IFD0:PreviewImage":"(Binary data %d bytes, use -b option to extract)",`+
		`"IFD0:PreviewImageStart":0,"IFD0:PreviewImageLength":%d}]`, len(jpegData), len(jpegData))
	// exec replaces the shell so canceling the command kills the sleep too.
	script := writeFakeTool(t,
		"json=0\nextract=0\n"+
			"for arg in \"$@\"; do\n"+
			"  if [ \"$arg\" = \"-j\" ]; then json=1; fi\n"+
			"  case \"$arg\" in -PreviewImage|*:PreviewImage) extract=1 ;; esac\n"+
			"done\n"+
			"if [ \"$json\" = \"1\" ]; then\n"+
			"  printf '%s' '"+meta+"'\n"+
			"elif [ \"$extract\" = \"1\" ]; then\n"+
			"  exec sleep 30\n"+
			"else\n"+
			"  cat '"+jpegPath+"'\n"+
			"fi\n")
	t.Setenv(envExifTool, script)

	out := filepath.Join(dir, "out.jpg")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	_, err = Export(ctx, srcPath, out, Options{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Export() error = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(out); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("temp output still exists after cancellation (stat error: %v)", statErr)
	}
}

// TestInspectProbesCandidateWithoutOffsets uses a fake exiftool to exercise
// the fallback that reads a candidate's header by streaming only the start of
// the tag, for formats that do not report Start/Length tags.
func TestInspectProbesCandidateWithoutOffsets(t *testing.T) {
	dir := t.TempDir()
	jpegPath := filepath.Join(dir, "preview.jpg")
	writeJPEGFile(t, jpegPath, 640, 480)
	meta := `[{"SourceFile":"fake.CR2","IFD0:ImageWidth":400,"IFD0:ImageHeight":300,` +
		`"IFD0:Orientation":1,"IFD0:JpgFromRaw":"(Binary data 1234 bytes, use -b option to extract)"}]`
	script := writeFakeTool(t,
		"json=0\n"+
			"for arg in \"$@\"; do if [ \"$arg\" = \"-j\" ]; then json=1; fi; done\n"+
			"if [ \"$json\" = \"1\" ]; then\n"+
			"  printf '%s' '"+meta+"'\n"+
			"else\n"+
			"  cat '"+jpegPath+"'\n"+
			"fi\n")
	t.Setenv(envExifTool, script)

	info, err := Inspect(context.Background(), filepath.Join(dir, "fake.CR2"))
	if err != nil {
		t.Fatalf("Inspect() error: %v", err)
	}
	if info.Width != 400 || info.Height != 300 {
		t.Errorf("camera dimensions = %dx%d, want 400x300", info.Width, info.Height)
	}
	if !info.HasPreview || info.PreviewTag != "JpgFromRaw" {
		t.Errorf("preview = has %v, tag %q; want true, JpgFromRaw", info.HasPreview, info.PreviewTag)
	}
	if info.PreviewWidth != 640 || info.PreviewHeight != 480 {
		t.Errorf("preview dimensions = %dx%d, want 640x480", info.PreviewWidth, info.PreviewHeight)
	}
}

// writeFakeTool writes an executable shell helper for tests that need to
// control a helper's behavior without the real binary.
func writeFakeTool(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake helper scripts require a POSIX shell")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not available: %v", err)
	}
	path := filepath.Join(t.TempDir(), "fake-exiftool")
	if err := os.WriteFile(path, []byte("#!"+sh+"\n"+body), 0o755); err != nil {
		t.Fatalf("write fake helper: %v", err)
	}
	return path
}

// TestCheckToolsMissingJpegTran checks that CheckTools names a missing
// helper. It points the jpegtran override at a path that does not exist.
func TestCheckToolsMissingJpegTran(t *testing.T) {
	requireExifTool(t)
	t.Setenv(envJpegTran, filepath.Join(t.TempDir(), "no-such-jpegtran"))
	err := CheckTools()
	if err == nil {
		t.Fatal("CheckTools() = nil, want a missing tool error")
	}
	if !strings.Contains(err.Error(), envJpegTran) {
		t.Errorf("CheckTools() = %v, want it to name %s", err, envJpegTran)
	}
}

// TestExportRejectsSmallPreview makes a fake exiftool report a tiny embedded
// JPEG. Export must return ErrNoFullJPEG instead of exporting the preview.
func TestExportRejectsSmallPreview(t *testing.T) {
	dir := t.TempDir()
	smallPath := filepath.Join(dir, "small.jpg")
	writeJPEGFile(t, smallPath, 320, 240)
	srcPath := filepath.Join(dir, "fake.CR2")
	if err := os.WriteFile(srcPath, []byte("this is not really a raw file"), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := `[{"SourceFile":"fake.CR2","IFD0:ImageWidth":6000,"IFD0:ImageHeight":4000,` +
		`"IFD0:Orientation":1,` +
		`"IFD0:PreviewImage":"(Binary data 100 bytes, use -b option to extract)",` +
		`"IFD0:PreviewImageStart":0,"IFD0:PreviewImageLength":100}]`
	t.Setenv(envExifTool, fakeCatTool(t, meta, smallPath))

	info, err := Inspect(context.Background(), srcPath)
	if err != nil {
		t.Fatalf("Inspect() error: %v", err)
	}
	if info.HasPreview {
		t.Errorf("Inspect HasPreview = true, want false for a 320x240 preview of a 6000x4000 camera")
	}
	out := filepath.Join(dir, "out.jpg")
	_, err = Export(context.Background(), srcPath, out, Options{})
	if !errors.Is(err, ErrNoFullJPEG) {
		t.Fatalf("Export() error = %v, want ErrNoFullJPEG", err)
	}
	if !strings.Contains(err.Error(), "smaller than the camera") {
		t.Errorf("Export() error = %v, want it to explain the size mismatch", err)
	}
	if _, statErr := os.Stat(out); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("temp output still exists after failure (stat error: %v)", statErr)
	}
}

// TestExportFallsBackToJpgFromRaw makes a fake exiftool return a truncated
// PreviewImage and a complete JpgFromRaw. Export must reject the truncated
// extraction and use the next candidate.
func TestExportFallsBackToJpgFromRaw(t *testing.T) {
	dir := t.TempDir()
	fullPath := filepath.Join(dir, "full.jpg")
	writeJPEGFile(t, fullPath, 640, 480)
	full, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatal(err)
	}
	truncated := encodeJPEG(t, 160, 120)
	truncated = truncated[:len(truncated)-2] // strip the EOI marker
	truncatedPath := filepath.Join(dir, "truncated.jpg")
	if err := os.WriteFile(truncatedPath, truncated, 0o644); err != nil {
		t.Fatal(err)
	}
	srcPath := filepath.Join(dir, "fake.CR2")
	if err := os.WriteFile(srcPath, []byte("this is not really a raw file"), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := `[{"SourceFile":"fake.CR2","IFD0:ImageWidth":400,"IFD0:ImageHeight":300,` +
		`"IFD0:Orientation":1,` +
		`"IFD0:PreviewImage":"(Binary data 100 bytes, use -b option to extract)",` +
		`"IFD0:JpgFromRaw":"(Binary data 200 bytes, use -b option to extract)"}]`
	script := writeFakeTool(t,
		"json=0\nraw=0\n"+
			"for arg in \"$@\"; do\n"+
			"  if [ \"$arg\" = \"-j\" ]; then json=1; fi\n"+
			"  case \"$arg\" in -JpgFromRaw|*:JpgFromRaw) raw=1 ;; esac\n"+
			"done\n"+
			"if [ \"$json\" = \"1\" ]; then\n"+
			"  printf '%s' '"+meta+"'\n"+
			"elif [ \"$raw\" = \"1\" ]; then\n"+
			"  cat '"+fullPath+"'\n"+
			"else\n"+
			"  cat '"+truncatedPath+"'\n"+
			"fi\n")
	t.Setenv(envExifTool, script)

	info, err := Inspect(context.Background(), srcPath)
	if err != nil {
		t.Fatalf("Inspect() error: %v", err)
	}
	t.Logf("inspect: has=%v tag=%s %dx%d (camera %dx%d orientation %d)",
		info.HasPreview, info.PreviewTag, info.PreviewWidth, info.PreviewHeight, info.Width, info.Height, info.Orientation)
	if !info.HasPreview || info.PreviewTag != "JpgFromRaw" {
		t.Errorf("Inspect preview = has %v, tag %q; want true, JpgFromRaw", info.HasPreview, info.PreviewTag)
	}
	out := filepath.Join(dir, "out.jpg")
	res, err := Export(context.Background(), srcPath, out, Options{})
	if err != nil {
		t.Fatalf("Export() error: %v", err)
	}
	if res.Width != 640 || res.Height != 480 {
		t.Errorf("Export dimensions = %dx%d, want 640x480 from JpgFromRaw", res.Width, res.Height)
	}
	if res.Rotated || res.Warning != "" {
		t.Errorf("Export rotated=%v warning=%q, want no rotation and no warning", res.Rotated, res.Warning)
	}
	if !bytes.Equal(full, mustReadFile(t, out)) {
		t.Error("output differs from the JpgFromRaw bytes")
	}
}

// fakeCatTool builds a fake exiftool that answers -j with meta and every
// binary extraction with the bytes of path.
func fakeCatTool(t *testing.T, meta, path string) string {
	t.Helper()
	return writeFakeTool(t,
		"json=0\n"+
			"for arg in \"$@\"; do if [ \"$arg\" = \"-j\" ]; then json=1; fi; done\n"+
			"if [ \"$json\" = \"1\" ]; then\n"+
			"  printf '%s' '"+meta+"'\n"+
			"else\n"+
			"  cat '"+path+"'\n"+
			"fi\n")
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// mustExifTool returns the exiftool path and fails the test when missing.
func mustExifTool(t *testing.T) string {
	t.Helper()
	bin, err := exiftoolPath()
	if err != nil {
		t.Fatalf("exiftool is required: %v", err)
	}
	return bin
}

// maxFormatSamples bounds TestInspectAndExportRealFormats. The harness is meant
// for a curated fixture directory with one or two representative samples per
// camera body/format, not a full photo library: each sample is exported at full
// resolution, so a directory of hundreds of photos runs far past the default
// `go test` timeout. Exceeding the bound fails fast with an explanation instead
// of timing out midway.
const maxFormatSamples = 20

// collectFormatSamples returns the recognized RAW files under root in sorted
// order, walking recursively and read-only. It returns an error when the
// directory holds more than max samples (max <= 0 disables the bound), so an
// accidental full library is rejected before any file is processed.
func collectFormatSamples(root string, max int) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if model.IsRawName(path) {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	if max > 0 && len(files) > max {
		return nil, fmt.Errorf("found %d RAW files, more than the %d this harness supports; point RAWDROP_TEST_FORMATS_DIR at a curated fixture directory with one or two representative samples per camera body/format, not a full photo library", len(files), max)
	}
	return files, nil
}

// TestCollectFormatSamplesRecursesAndBounds checks the fixture collector: it
// finds recognized RAW files recursively and in sorted order, ignores other
// files, and rejects an oversized directory before any sample is processed.
func TestCollectFormatSamplesRecursesAndBounds(t *testing.T) {
	root := t.TempDir()
	write := func(rel string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("b.CR2")
	write("a.nef")
	write("nested/c.arw")
	write("notes.txt") // not a RAW: ignored

	got, err := collectFormatSamples(root, 0)
	if err != nil {
		t.Fatalf("collectFormatSamples() error: %v", err)
	}
	want := []string{
		filepath.Join(root, "a.nef"),
		filepath.Join(root, "b.CR2"),
		filepath.Join(root, "nested", "c.arw"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("collectFormatSamples() = %v, want %v", got, want)
	}

	if _, err := collectFormatSamples(root, len(got)-1); err == nil {
		t.Fatal("collectFormatSamples() = nil error, want the bound to reject an oversized directory")
	} else if !strings.Contains(err.Error(), "curated fixture") {
		t.Errorf("bound error = %v, want guidance to curate the fixture directory", err)
	}
}

// TestInspectAndExportRealFormats runs the full inspect/export contract over
// every recognized RAW file in a curated fixture directory named by
// RAWDROP_TEST_FORMATS_DIR. It is opt-in because real camera samples are large
// and personal; nothing is committed to the repository.
//
// Build the fixture directory as a small, deliberate set: one or two
// representative samples per camera body/format being evaluated, for example
//
//	mkdir -p ~/rawdrop-fixtures
//	cp "/path/to/library/IMG_3128.CR2" ~/rawdrop-fixtures/   # Canon EOS 80D
//	# add one or two more per body, then:
//	RAWDROP_TEST_FORMATS_DIR=~/rawdrop-fixtures GOTOOLCHAIN=go1.27.1 \
//	  go test -run TestInspectAndExportRealFormats ./internal/raw/ -v
//
// Do not point it at a whole photo library: exporting every sample at full
// resolution runs past the default `go test` timeout, and the harness fails
// fast once the directory exceeds maxFormatSamples. Each sample is opened
// read-only; every output goes to t.TempDir(). A format is not "verified" in
// docs/FORMAT_SUPPORT.md until a representative sample from its camera family
// passes here.
func TestInspectAndExportRealFormats(t *testing.T) {
	root := os.Getenv("RAWDROP_TEST_FORMATS_DIR")
	if root == "" {
		t.Skip("set RAWDROP_TEST_FORMATS_DIR to a curated fixture directory of real RAW samples")
	}
	files, err := collectFormatSamples(root, maxFormatSamples)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if len(files) == 0 {
		t.Skipf("no recognized RAW files under %s", root)
	}
	requireExifTool(t)
	bin := mustExifTool(t)

	for _, src := range files {
		src := src
		t.Run(filepath.Base(src), func(t *testing.T) {
			before := statFile(t, src)

			info, err := Inspect(context.Background(), src)
			if err != nil {
				t.Fatalf("Inspect() error: %v", err)
			}
			t.Logf("%s: make/model %q %q, camera %dx%d orientation %d, preview %s %dx%d (has %v)",
				filepath.Base(src), info.Make, info.Model, info.Width, info.Height,
				info.Orientation, info.PreviewTag, info.PreviewWidth, info.PreviewHeight, info.HasPreview)
			if info.Make == "" || info.Model == "" {
				t.Errorf("Inspect() make/model empty; the metadata layout is not understood for this format")
			}
			if !info.HasPreview {
				t.Fatalf("no full-resolution embedded JPEG (candidate %s %dx%d, camera %dx%d)",
					info.PreviewTag, info.PreviewWidth, info.PreviewHeight, info.Width, info.Height)
			}

			out := filepath.Join(t.TempDir(), "export.jpg")
			res, err := Export(context.Background(), src, out, Options{CorrectOrientation: true})
			if err != nil {
				t.Fatalf("Export() error: %v", err)
			}
			w, h, err := validateJPEGFile(out)
			if err != nil {
				t.Fatalf("output is not a valid JPEG: %v", err)
			}
			if w != res.Width || h != res.Height {
				t.Errorf("output decodes to %dx%d, want the reported %dx%d", w, h, res.Width, res.Height)
			}
			orientation, gps, err := readOrientationAndGPS(context.Background(), bin, out)
			if err != nil {
				t.Fatalf("read output metadata: %v", err)
			}
			if orientation != res.Orientation {
				t.Errorf("output EXIF orientation = %d, want %d", orientation, res.Orientation)
			}
			if len(gps) != 0 {
				t.Errorf("output still has location metadata by default: %v", gps)
			}
			t.Logf("export: %dx%d orientation %d rotated=%v warning=%q cleaned=%v",
				res.Width, res.Height, res.Orientation, res.Rotated, res.Warning, res.MetadataCleaned)
			assertFileUnchanged(t, src, before)
		})
	}
}

// jpegScanData returns a JPEG's compressed image data: everything from the
// first SOS marker to the end of the file. Metadata rewrites keep these bytes
// identical; a rotation or any other image change does not.
func jpegScanData(t *testing.T, data []byte) []byte {
	t.Helper()
	idx := bytes.Index(data, []byte{0xFF, 0xDA})
	if idx < 0 {
		t.Fatal("JPEG has no SOS marker")
	}
	return data[idx:]
}
