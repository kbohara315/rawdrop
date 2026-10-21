package thumb

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- helpers -----------------------------------------------------------------

func gradient(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(x * 255 / max(1, w-1)),
				G: uint8(y * 255 / max(1, h-1)),
				B: uint8((x + y) % 256),
				A: 255,
			})
		}
	}
	return img
}

func testJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, gradient(w, h), &jpeg.Options{Quality: 92}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func dims(t *testing.T, data []byte) (int, int) {
	t.Helper()
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode config: %v", err)
	}
	return cfg.Width, cfg.Height
}

func stubExtract(t *testing.T, byTag map[string][]byte) (calls *[]string) {
	t.Helper()
	old := toolExtract
	t.Cleanup(func() { toolExtract = old })
	var seen []string
	toolExtract = func(_ context.Context, _, tag string) ([]byte, error) {
		seen = append(seen, tag)
		data, ok := byTag[tag]
		if !ok || data == nil {
			return nil, errors.New("stub: " + tag + " unavailable")
		}
		return data, nil
	}
	return &seen
}

func stubOrientation(t *testing.T, value int) {
	t.Helper()
	old := toolOrientation
	t.Cleanup(func() { toolOrientation = old })
	toolOrientation = func(context.Context, string) (int, error) { return value, nil }
}

func put16(buf *bytes.Buffer, order binary.ByteOrder, v uint16) {
	var b [2]byte
	order.PutUint16(b[:], v)
	buf.Write(b[:])
}

func put32(buf *bytes.Buffer, order binary.ByteOrder, v uint32) {
	var b [4]byte
	order.PutUint32(b[:], v)
	buf.Write(b[:])
}

// tiffWithOrientation builds a minimal TIFF file whose IFD0 carries the
// orientation tag.
func tiffWithOrientation(order binary.ByteOrder, orientation int) []byte {
	var buf bytes.Buffer
	if order == binary.LittleEndian {
		buf.WriteString("II")
	} else {
		buf.WriteString("MM")
	}
	put16(&buf, order, 42)
	put32(&buf, order, 8) // IFD0 offset
	put16(&buf, order, 1) // entry count
	put16(&buf, order, exifOrientationTag)
	put16(&buf, order, exifTypeShort)
	put32(&buf, order, 1)
	put16(&buf, order, uint16(orientation))
	put16(&buf, order, 0) // pad the inline value to four bytes
	put32(&buf, order, 0) // no next IFD
	return buf.Bytes()
}

// tiffWithCR2Header mirrors the real CR2 layout: the TIFF header points past
// the eight CR2 magic bytes to IFD0 at offset 16.
func tiffWithCR2Header(order binary.ByteOrder, orientation int) []byte {
	ifd := tiffWithOrientation(order, orientation)[8:]
	var buf bytes.Buffer
	if order == binary.LittleEndian {
		buf.WriteString("II")
	} else {
		buf.WriteString("MM")
	}
	put16(&buf, order, 42)
	put32(&buf, order, 16) // IFD0 after the CR2 header
	buf.Write([]byte{'C', 'R', 2, 0, 0, 0, 0, 0})
	buf.Write(ifd)
	return buf.Bytes()
}

// tiffWithSubIFDOrientation stores the orientation in the Exif sub-IFD
// instead of IFD0.
func tiffWithSubIFDOrientation(order binary.ByteOrder, orientation int) []byte {
	var buf bytes.Buffer
	if order == binary.LittleEndian {
		buf.WriteString("II")
	} else {
		buf.WriteString("MM")
	}
	put16(&buf, order, 42)
	put32(&buf, order, 8) // IFD0 offset
	put16(&buf, order, 1) // entry count
	put16(&buf, order, exifIFDPointerTag)
	put16(&buf, order, exifTypeLong)
	put32(&buf, order, 1)
	put32(&buf, order, 26) // sub-IFD offset
	put32(&buf, order, 0)  // no next IFD
	put16(&buf, order, 1)  // sub-IFD entry count
	put16(&buf, order, exifOrientationTag)
	put16(&buf, order, exifTypeShort)
	put32(&buf, order, 1)
	put16(&buf, order, uint16(orientation))
	put16(&buf, order, 0)
	put32(&buf, order, 0)
	return buf.Bytes()
}

func app1EXIF(order binary.ByteOrder, orientation int) []byte {
	payload := append([]byte("Exif\x00\x00"), tiffWithOrientation(order, orientation)...)
	seg := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(seg[2:4], uint16(len(payload)+2))
	return append(seg, payload...)
}

func app0JFIF() []byte {
	return []byte{
		0xFF, 0xE0, 0x00, 0x10,
		'J', 'F', 'I', 'F', 0x00,
		0x01, 0x01, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00,
	}
}

func jpegSegments(segments ...[]byte) []byte {
	out := []byte{0xFF, 0xD8}
	for _, seg := range segments {
		out = append(out, seg...)
	}
	return append(out, 0xFF, 0xD9)
}

// withEXIF inserts an APP1 EXIF segment right after a JPEG's SOI marker.
func withEXIF(jpegData []byte, order binary.ByteOrder, orientation int) []byte {
	out := append([]byte{}, jpegData[:2]...)
	out = append(out, app1EXIF(order, orientation)...)
	return append(out, jpegData[2:]...)
}

func writeSource(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// --- orientation transforms --------------------------------------------------

func pixels(img image.Image) [][]color.RGBA {
	b := img.Bounds()
	out := make([][]color.RGBA, b.Dy())
	for y := 0; y < b.Dy(); y++ {
		out[y] = make([]color.RGBA, b.Dx())
		for x := 0; x < b.Dx(); x++ {
			out[y][x] = color.RGBAModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.RGBA)
		}
	}
	return out
}

func TestOrientTransforms(t *testing.T) {
	a := color.RGBA{255, 0, 0, 255}
	b := color.RGBA{0, 255, 0, 255}
	c := color.RGBA{0, 0, 255, 255}
	d := color.RGBA{255, 255, 0, 255}

	src := image.NewRGBA(image.Rect(0, 0, 2, 2))
	src.SetRGBA(0, 0, a)
	src.SetRGBA(1, 0, b)
	src.SetRGBA(0, 1, c)
	src.SetRGBA(1, 1, d)

	cases := map[int][][]color.RGBA{
		1: {{a, b}, {c, d}}, // as stored
		2: {{b, a}, {d, c}}, // mirror horizontal
		3: {{d, c}, {b, a}}, // rotate 180
		4: {{c, d}, {a, b}}, // mirror vertical
		5: {{a, c}, {b, d}}, // transpose
		6: {{c, a}, {d, b}}, // rotate 90 CW
		7: {{d, b}, {c, a}}, // transverse
		8: {{b, d}, {a, c}}, // rotate 270 CW
	}
	for orientation, want := range cases {
		got := pixels(orient(src, orientation))
		if len(got) != len(want) {
			t.Fatalf("orientation %d: %d rows, want %d", orientation, len(got), len(want))
		}
		for y := range want {
			for x := range want[y] {
				if got[y][x] != want[y][x] {
					t.Fatalf("orientation %d pixel (%d,%d) = %v, want %v", orientation, x, y, got[y][x], want[y][x])
				}
			}
		}
	}

	// Unknown orientations pass the image through untouched.
	for _, value := range []int{-1, 0, 9} {
		if got := orient(src, value); got != image.Image(src) {
			t.Fatalf("orientation %d changed the image", value)
		}
	}
}

// --- EXIF parsing ------------------------------------------------------------

func TestTIFFOrientation(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want int
	}{
		{"little endian", tiffWithOrientation(binary.LittleEndian, 6), 6},
		{"big endian", tiffWithOrientation(binary.BigEndian, 3), 3},
		{"cr2 layout", tiffWithCR2Header(binary.LittleEndian, 8), 8},
		{"exif sub-ifd", tiffWithSubIFDOrientation(binary.LittleEndian, 5), 5},
		{"invalid value", tiffWithOrientation(binary.LittleEndian, 9), 0},
		{"empty", nil, 0},
		{"not tiff", []byte("hello world"), 0},
		{"truncated", []byte{'I', 'I', 42, 0, 8, 0}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tiffOrientation(tc.data); got != tc.want {
				t.Fatalf("tiffOrientation = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestOrientationFromFile(t *testing.T) {
	path := writeSource(t, "photo.CR2", tiffWithCR2Header(binary.LittleEndian, 8))
	if got := orientationFromFile(path); got != 8 {
		t.Fatalf("orientationFromFile = %d, want 8", got)
	}
	if got := orientationFromFile(filepath.Join(t.TempDir(), "missing.CR2")); got != 0 {
		t.Fatalf("orientationFromFile(missing) = %d, want 0", got)
	}
}

func TestJPEGEXIFOrientation(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want int
	}{
		{"app1 only", jpegSegments(app1EXIF(binary.LittleEndian, 8)), 8},
		{"after jfif", jpegSegments(app0JFIF(), app1EXIF(binary.BigEndian, 5)), 5},
		{"no exif", jpegSegments(app0JFIF()), 0},
		{"empty", nil, 0},
		{"short segment", []byte{0xFF, 0xD8, 0xFF, 0xE1, 0xFF, 0xFF}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := jpegEXIFOrientation(tc.data); got != tc.want {
				t.Fatalf("jpegEXIFOrientation = %d, want %d", got, tc.want)
			}
		})
	}
	// The JPEG encoder emits no EXIF.
	if got := jpegEXIFOrientation(testJPEG(t, 4, 4)); got != 0 {
		t.Fatalf("encoded JPEG reports orientation %d, want 0", got)
	}
}

// --- Make --------------------------------------------------------------------

func TestMakeUsesLargeThumbnail(t *testing.T) {
	source := writeSource(t, "photo.CR2", tiffWithOrientation(binary.LittleEndian, 6))
	calls := stubExtract(t, map[string][]byte{tagThumbnail: testJPEG(t, 160, 120)})

	out, err := Make(context.Background(), source)
	if err != nil {
		t.Fatalf("Make: %v", err)
	}
	if w, h := dims(t, out); w != 120 || h != 160 {
		t.Fatalf("size = %dx%d, want the orientation-6 display size 120x160", w, h)
	}
	if len(*calls) != 1 || (*calls)[0] != tagThumbnail {
		t.Fatalf("extracted tags = %v, want just the thumbnail", *calls)
	}
}

func TestMakeFallsBackToPreview(t *testing.T) {
	source := writeSource(t, "photo.CR2", tiffWithOrientation(binary.LittleEndian, 1))
	calls := stubExtract(t, map[string][]byte{
		tagThumbnail: testJPEG(t, 80, 60), // Below the 100 px floor.
		tagPreview:   testJPEG(t, 1280, 960),
	})

	out, err := Make(context.Background(), source)
	if err != nil {
		t.Fatalf("Make: %v", err)
	}
	if w, h := dims(t, out); w != 320 || h != 240 {
		t.Fatalf("size = %dx%d, want the scaled preview 320x240", w, h)
	}
	if len(*calls) != 2 {
		t.Fatalf("extracted tags = %v, want thumbnail then preview", *calls)
	}
}

func TestMakeScalesPreviewWithOrientation(t *testing.T) {
	source := writeSource(t, "photo.CR2", tiffWithOrientation(binary.LittleEndian, 8))
	stubExtract(t, map[string][]byte{
		tagThumbnail: testJPEG(t, 80, 60),
		tagPreview:   testJPEG(t, 1280, 960),
	})

	out, err := Make(context.Background(), source)
	if err != nil {
		t.Fatalf("Make: %v", err)
	}
	if w, h := dims(t, out); w != 240 || h != 320 {
		t.Fatalf("size = %dx%d, want the rotated preview 240x320", w, h)
	}
}

func TestMakeUsesTinyThumbnailAsLastResort(t *testing.T) {
	source := writeSource(t, "photo.CR2", tiffWithOrientation(binary.LittleEndian, 1))
	stubExtract(t, map[string][]byte{tagThumbnail: testJPEG(t, 80, 60)})

	out, err := Make(context.Background(), source)
	if err != nil {
		t.Fatalf("Make: %v", err)
	}
	if w, h := dims(t, out); w != 80 || h != 60 {
		t.Fatalf("size = %dx%d, want the tiny thumbnail without upscaling", w, h)
	}
}

func TestMakePrefersEmbeddedEXIFOrientation(t *testing.T) {
	// The source is not a TIFF container, so only the embedded image's own
	// EXIF can decide the orientation; the exiftool fallback says 1.
	source := writeSource(t, "photo.cr3", []byte("not a tiff container"))
	stubOrientation(t, 1)
	stubExtract(t, map[string][]byte{tagThumbnail: withEXIF(testJPEG(t, 160, 120), binary.LittleEndian, 8)})

	out, err := Make(context.Background(), source)
	if err != nil {
		t.Fatalf("Make: %v", err)
	}
	if w, h := dims(t, out); w != 120 || h != 160 {
		t.Fatalf("size = %dx%d, want the embedded orientation 8 to win", w, h)
	}
}

func TestMakeFallsBackToExiftoolOrientation(t *testing.T) {
	source := writeSource(t, "photo.cr3", []byte("not a tiff container"))
	stubOrientation(t, 6)
	stubExtract(t, map[string][]byte{tagThumbnail: testJPEG(t, 160, 120)})

	out, err := Make(context.Background(), source)
	if err != nil {
		t.Fatalf("Make: %v", err)
	}
	if w, h := dims(t, out); w != 120 || h != 160 {
		t.Fatalf("size = %dx%d, want the exiftool orientation 6", w, h)
	}
}

func TestMakeNoEmbeddedPreview(t *testing.T) {
	source := writeSource(t, "photo.CR2", tiffWithOrientation(binary.LittleEndian, 1))
	stubExtract(t, map[string][]byte{}) // Every tag is unavailable.

	if _, err := Make(context.Background(), source); err == nil {
		t.Fatal("Make succeeded without any embedded image")
	} else if !strings.Contains(err.Error(), "no usable embedded preview") {
		t.Fatalf("error = %v", err)
	}
}

func TestMakeRejectsInvalidEmbeddedData(t *testing.T) {
	source := writeSource(t, "photo.CR2", tiffWithOrientation(binary.LittleEndian, 1))
	stubExtract(t, map[string][]byte{
		tagThumbnail: []byte("not a jpeg"),
		tagPreview:   []byte("still not a jpeg"),
	})

	out, err := Make(context.Background(), source)
	if err == nil || out != nil {
		t.Fatalf("Make = %q, %v; want an error", out, err)
	}
	if !strings.Contains(err.Error(), "no usable embedded preview") {
		t.Fatalf("error = %v", err)
	}
}

func TestMakeRejectsImplausiblePreview(t *testing.T) {
	source := writeSource(t, "photo.CR2", tiffWithOrientation(binary.LittleEndian, 1))
	// Patch the SOF dimensions so the header claims a 20001 px wide image
	// without allocating one.
	patched := patchSOF(t, testJPEG(t, 4, 4), 20001, 4)
	stubExtract(t, map[string][]byte{
		tagThumbnail: nil,
		tagPreview:   patched,
	})

	_, err := Make(context.Background(), source)
	if err == nil || !strings.Contains(err.Error(), "implausible") {
		t.Fatalf("error = %v, want an implausible-size rejection", err)
	}
}

func TestMakeMissingExiftool(t *testing.T) {
	t.Setenv("RAWDROP_EXIFTOOL", filepath.Join(t.TempDir(), "absent-exiftool"))
	source := writeSource(t, "photo.CR2", tiffWithOrientation(binary.LittleEndian, 1))

	if _, err := Make(context.Background(), source); err == nil {
		t.Fatal("Make succeeded without exiftool")
	} else if !strings.Contains(err.Error(), "exiftool") {
		t.Fatalf("error = %v, want an exiftool hint", err)
	}
}

func TestMakeRejectsEmptySource(t *testing.T) {
	if _, err := Make(context.Background(), "  "); err == nil {
		t.Fatal("Make with an empty source must fail")
	}
}

// patchSOF rewrites the frame dimensions in a baseline JPEG's SOF marker.
func patchSOF(t *testing.T, data []byte, width, height int) []byte {
	t.Helper()
	out := append([]byte(nil), data...)
	for i := 2; i+10 <= len(out); {
		if out[i] != 0xFF {
			t.Fatalf("malformed JPEG at %d", i)
		}
		marker := out[i+1]
		size := int(binary.BigEndian.Uint16(out[i+2 : i+4]))
		if marker == 0xC0 || marker == 0xC2 {
			out[i+5] = byte(height >> 8)
			out[i+6] = byte(height)
			out[i+7] = byte(width >> 8)
			out[i+8] = byte(width)
			return out
		}
		if marker == 0xDA {
			break
		}
		i += 2 + size
	}
	t.Fatal("no SOF marker found")
	return nil
}

// --- exiftool runner ---------------------------------------------------------

func fakeTool(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-exiftool")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunExiftoolEnvOverride(t *testing.T) {
	t.Setenv("RAWDROP_EXIFTOOL", fakeTool(t, `#!/bin/sh
case "$*" in
  *ThumbnailImage*) printf 'thumb-bytes' ;;
  *PreviewImage*) printf 'preview-bytes' ;;
  *) printf '8' ;;
esac
`))
	out, err := extractExiftool(context.Background(), "ignored.CR2", tagThumbnail)
	if err != nil || string(out) != "thumb-bytes" {
		t.Fatalf("extractExiftool = %q, %v", out, err)
	}
	out, err = extractExiftool(context.Background(), "ignored.CR2", tagPreview)
	if err != nil || string(out) != "preview-bytes" {
		t.Fatalf("extractExiftool = %q, %v", out, err)
	}
	orientation, err := orientationExiftool(context.Background(), "ignored.CR2")
	if err != nil || orientation != 8 {
		t.Fatalf("orientationExiftool = %d, %v", orientation, err)
	}
}

func TestRunExiftoolBoundsOutput(t *testing.T) {
	t.Setenv("RAWDROP_EXIFTOOL", fakeTool(t, `#!/bin/sh
printf '0123456789abcdefghijklmnopqrstuvwxyz'
`))
	_, err := runExiftool(context.Background(), 16, "-b", "-ThumbnailImage")
	if err == nil || !strings.Contains(err.Error(), "exceeded 16 bytes") {
		t.Fatalf("error = %v, want an output-limit error", err)
	}
}

func TestRunExiftoolRespectsContext(t *testing.T) {
	t.Setenv("RAWDROP_EXIFTOOL", fakeTool(t, `#!/bin/sh
exec sleep 5
`))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := runExiftool(ctx, 1024, "-b", "-ThumbnailImage")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("run took %v, want the context deadline to stop it", elapsed)
	}
}

func TestRunExiftoolReportsFailure(t *testing.T) {
	t.Setenv("RAWDROP_EXIFTOOL", fakeTool(t, `#!/bin/sh
echo 'boom' >&2
exit 3
`))
	_, err := runExiftool(context.Background(), 1024, "-b", "-ThumbnailImage")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error = %v, want the exiftool stderr", err)
	}
}

// --- real fixture ------------------------------------------------------------

// realCR2 returns a sample CR2 path for the read-only integration test. Point
// RAWDROP_TEST_CR2 at one to run the test on another machine.
func realCR2() string {
	if path := os.Getenv("RAWDROP_TEST_CR2"); path != "" {
		return path
	}
	for _, pattern := range []string{
		"/home/kshitij/Pictures/dashain photos/*.CR2",
		"/home/kshitij/Pictures/Camera/*.CR2",
	} {
		if matches, _ := filepath.Glob(pattern); len(matches) > 0 {
			return matches[0]
		}
	}
	return ""
}

func TestMakeRealCR2(t *testing.T) {
	bin := os.Getenv("RAWDROP_EXIFTOOL")
	if bin == "" {
		bin = "exiftool"
	}
	if _, err := exec.LookPath(bin); err != nil {
		t.Skip("exiftool is not installed")
	}
	source := realCR2()
	if source == "" {
		t.Skip("no sample CR2 available; set RAWDROP_TEST_CR2")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := Make(ctx, source)
	if err != nil {
		t.Fatalf("Make(%s): %v", source, err)
	}
	if !bytes.HasPrefix(out, jpegMagic) {
		t.Fatal("output is not a JPEG")
	}
	w, h := dims(t, out)
	if w > MaxEdge || h > MaxEdge {
		t.Fatalf("size = %dx%d, want at most %d px on the long edge", w, h, MaxEdge)
	}

	orientation := orientationFromFile(source)
	if orientation == 0 {
		t.Fatalf("could not parse the orientation of the real CR2 %s", source)
	}
	if orientation >= 5 && h <= w {
		t.Fatalf("orientation %d must display as portrait, got %dx%d", orientation, w, h)
	}
	if orientation < 5 && w < h {
		t.Fatalf("orientation %d must display as landscape, got %dx%d", orientation, w, h)
	}
}
