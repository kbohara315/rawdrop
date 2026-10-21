package raw

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestJPEGTransform(t *testing.T) {
	tests := []struct {
		orientation int
		want        []string
	}{
		{1, nil},
		{2, []string{"-flip", "horizontal"}},
		{3, []string{"-rotate", "180"}},
		{4, []string{"-flip", "vertical"}},
		{5, []string{"-transpose"}},
		{6, []string{"-rotate", "90"}},
		{7, []string{"-transverse"}},
		{8, []string{"-rotate", "270"}},
		{0, nil},
		{9, nil},
		{-1, nil},
	}
	for _, tt := range tests {
		if got := jpegTransform(tt.orientation); !slices.Equal(got, tt.want) {
			t.Errorf("jpegTransform(%d) = %q, want %q", tt.orientation, got, tt.want)
		}
	}
}

func TestNeedsRotation(t *testing.T) {
	tests := []struct {
		orientation int
		correct     bool
		want        bool
	}{
		{1, true, false},
		{1, false, false},
		{2, true, true},
		{8, true, true},
		{8, false, false},
		{0, true, false},
	}
	for _, tt := range tests {
		if got := needsRotation(tt.orientation, tt.correct); got != tt.want {
			t.Errorf("needsRotation(%d, %v) = %v, want %v", tt.orientation, tt.correct, got, tt.want)
		}
	}
}

func TestFinalOrientation(t *testing.T) {
	tests := []struct {
		orientation int
		rotated     bool
		want        int
	}{
		{1, false, 1},
		{1, true, 1},
		{8, false, 8},
		{8, true, 1},
		{0, false, 1},
		{9, false, 1},
	}
	for _, tt := range tests {
		if got := finalOrientation(tt.orientation, tt.rotated); got != tt.want {
			t.Errorf("finalOrientation(%d, %v) = %d, want %d", tt.orientation, tt.rotated, got, tt.want)
		}
	}
}

func TestSwapsDims(t *testing.T) {
	for _, o := range []int{1, 2, 3, 4} {
		if swapsDims(o) {
			t.Errorf("swapsDims(%d) = true, want false", o)
		}
	}
	for _, o := range []int{5, 6, 7, 8} {
		if !swapsDims(o) {
			t.Errorf("swapsDims(%d) = false, want true", o)
		}
	}
}

func TestIsFullSize(t *testing.T) {
	camera := func(o int) Info { return Info{Width: 6000, Height: 4000, Orientation: o} }
	tests := []struct {
		name string
		info Info
		w, h int
		want bool
	}{
		{"sensor layout matches camera", camera(8), 6000, 4000, true},
		{"pre-rotated portrait accepted for 5-8", camera(8), 4000, 6000, true},
		{"pre-rotated rejected for orientation 1", camera(1), 4000, 6000, false},
		{"thumbnail rejected", camera(8), 524, 338, false},
		{"exactly 90 percent accepted", camera(1), 5400, 3600, true},
		{"just under 90 percent rejected", camera(1), 5399, 3600, false},
		{"one axis too small rejected", camera(1), 6000, 3599, false},
		{"raw sensor size accepted", camera(1), 6288, 4056, true},
		{"unknown camera dimensions rejected", Info{Orientation: 1}, 6000, 4000, false},
		{"zero jpeg dimensions rejected", camera(1), 0, 0, false},
	}
	for _, tt := range tests {
		if got := isFullSize(tt.w, tt.h, tt.info); got != tt.want {
			t.Errorf("%s: isFullSize(%d, %d, %+v) = %v, want %v", tt.name, tt.w, tt.h, tt.info, got, tt.want)
		}
	}
}

// encodeJPEG returns a complete baseline JPEG of the given size.
func encodeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 7), G: uint8(y * 5), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatalf("encode JPEG: %v", err)
	}
	return buf.Bytes()
}

func TestCheckJPEGStructure(t *testing.T) {
	valid := encodeJPEG(t, 16, 16)
	// A minimal but structurally complete JPEG with entropy data containing a
	// stuffed FF byte and a restart marker.
	segment := func(marker byte, payload []byte) []byte {
		return append([]byte{0xFF, marker, 0x00, byte(len(payload) + 2)}, payload...)
	}
	crafted := []byte{0xFF, 0xD8}                                                   // SOI
	crafted = append(crafted, segment(0xC0, make([]byte, 15))...)                   // SOF0
	crafted = append(crafted, segment(0xDA, make([]byte, 10))...)                   // SOS header
	crafted = append(crafted, 0x01, 0x02, 0xFF, 0x00, 0x03, 0xFF, 0xD0, 0x04, 0x05) // entropy
	crafted = append(crafted, 0xFF, 0xD9)                                           // EOI
	tests := []struct {
		name    string
		data    []byte
		wantErr string
	}{
		{"encoded jpeg", valid, ""},
		{"crafted with stuffing and restart", crafted, ""},
		{"trailing bytes after eoi", append(slices.Clone(valid), "trailing"...), ""},
		{"truncated by one byte", valid[:len(valid)-1], "truncated"},
		{"eoi removed", valid[:len(valid)-2], "truncated"},
		{"empty", nil, "SOI"},
		{"not a jpeg", []byte("this is not a jpeg"), "SOI"},
		{"soi and eoi only", []byte{0xFF, 0xD8, 0xFF, 0xD9}, "before any image data"},
		{"segment length below minimum", []byte{0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x01, 0xFF, 0xD9}, "segment length 1"},
		{"segment longer than file", []byte{0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x10, 0x00, 0x00, 0x00, 0x00, 0xFF, 0xD9}, "truncated"},
		{"data outside a scan", []byte{0xFF, 0xD8, 0x00, 0xFF, 0xD9}, "outside a scan"},
		{"dangling marker prefix", []byte{0xFF, 0xD8, 0xFF}, "truncated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkJPEGStructure(bytes.NewReader(tt.data))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("checkJPEGStructure() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkJPEGStructure() = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("checkJPEGStructure() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestJPEGDimensions(t *testing.T) {
	valid := encodeJPEG(t, 24, 13)
	w, h, err := jpegDimensions(bytes.NewReader(valid))
	if err != nil || w != 24 || h != 13 {
		t.Fatalf("jpegDimensions(valid) = %d, %d, %v; want 24, 13, nil", w, h, err)
	}
	if _, _, err := jpegDimensions(bytes.NewReader(valid[:10])); err == nil {
		t.Error("jpegDimensions(truncated header) = nil error, want failure")
	}
	if _, _, err := jpegDimensions(strings.NewReader("not a jpeg at all")); err == nil {
		t.Error("jpegDimensions(garbage) = nil error, want failure")
	}
}

func TestCappedBuffer(t *testing.T) {
	b := cappedBuffer{max: 4}
	if n, err := b.Write([]byte("abcdefgh")); n != 8 || err != nil {
		t.Fatalf("Write = %d, %v; want 8, nil", n, err)
	}
	if got := b.String(); !strings.Contains(got, "abcd") || !strings.Contains(got, "truncated") {
		t.Fatalf("String() = %q, want kept prefix and truncation note", got)
	}
	b2 := cappedBuffer{max: 16}
	b2.Write([]byte(" short "))
	if got := b2.String(); got != "short" {
		t.Fatalf("String() = %q, want %q", got, "short")
	}
}

func TestCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Inspect(ctx, "/nonexistent/photo.cr2"); !errors.Is(err, context.Canceled) {
		t.Errorf("Inspect with canceled context = %v, want context.Canceled", err)
	}
	out := filepath.Join(t.TempDir(), "out.jpg")
	if _, err := Export(ctx, "/nonexistent/photo.cr2", out, Options{}); !errors.Is(err, context.Canceled) {
		t.Errorf("Export with canceled context = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temp file was created despite cancellation (stat error: %v)", err)
	}
}
