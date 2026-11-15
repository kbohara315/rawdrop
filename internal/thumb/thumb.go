// Package thumb renders small, upright JPEG previews of RAW and JPEG photos for the
// photo grid. It extracts an embedded image with exiftool and scales it with
// golang.org/x/image/draw. Sources are only ever opened read-only.
package thumb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"math"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/image/draw"
)

const (
	// MaxEdge is the longest edge of an output thumbnail. Thumbnails are
	// never upscaled beyond their source.
	MaxEdge = 320
	// minEmbeddedEdge is the smallest acceptable long edge of the embedded
	// thumbnail. Smaller ones are replaced by a scaled-down preview.
	minEmbeddedEdge = 100
	// maxEmbeddedEdge rejects absurd preview dimensions before decoding, so
	// a corrupt or hostile embedded image cannot exhaust memory.
	maxEmbeddedEdge = 20000
	// jpegQuality keeps grid previews small while staying visually clean.
	jpegQuality = 80
)

const (
	tagThumbnail = "ThumbnailImage"
	tagPreview   = "PreviewImage"
)

var jpegMagic = []byte{0xFF, 0xD8}

// Make returns a small JPEG thumbnail of the photo at source. It prefers
// the embedded ThumbnailImage, falls back to a scaled-down PreviewImage when
// the thumbnail is missing or below 100 px, applies the EXIF orientation,
// and encodes JPEG at quality 80 with a 320 px long edge. The source is
// never modified.
func Make(ctx context.Context, source string) ([]byte, error) {
	if strings.TrimSpace(source) == "" {
		return nil, errors.New("thumb: empty source path")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	img, raw, err := pickEmbedded(ctx, source)
	if err != nil && isJPEGSource(source) {
		img, err = decodeJPEGFile(source)
		raw = nil
	}
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Prefer orientation stored next to the pixels that will be shown, then
	// the RAW container's IFD0, then exiftool for formats without a TIFF
	// header. Unknown orientation degrades to 1 (no rotation).
	orientation := 0
	if len(raw) > 0 {
		orientation = jpegEXIFOrientation(raw)
	}
	if orientation == 0 {
		orientation = sourceOrientation(ctx, source)
	}
	return render(img, orientation)
}

func isJPEGSource(source string) bool {
	switch strings.ToLower(filepath.Ext(source)) {
	case ".jpg", ".jpeg":
		return true
	default:
		return false
	}
}

func decodeJPEGFile(source string) (image.Image, error) {
	f, err := os.Open(source)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, err := jpeg.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("thumb: decode JPEG: %w", err)
	}
	return img, nil
}

// pickEmbedded selects the best embedded image: the thumbnail when it is
// large enough, otherwise the preview, with an undersized thumbnail kept as
// a last resort. The returned raw bytes are the ones that were decoded, so
// the caller can read their EXIF orientation.
func pickEmbedded(ctx context.Context, source string) (image.Image, []byte, error) {
	var (
		tiny    []byte
		reasons []string
	)

	thumbBytes, err := toolExtract(ctx, source, tagThumbnail)
	switch {
	case err != nil:
		reasons = append(reasons, "thumbnail: "+err.Error())
	case len(thumbBytes) > 0:
		cfg, cfgErr := imageConfig(thumbBytes)
		switch {
		case cfgErr != nil:
			reasons = append(reasons, "thumbnail: "+cfgErr.Error())
		case longEdge(cfg.Width, cfg.Height) >= minEmbeddedEdge:
			if img, err := decodeJPEG(thumbBytes); err == nil {
				return img, thumbBytes, nil
			} else {
				reasons = append(reasons, "thumbnail: "+err.Error())
			}
		default:
			// Too small for the grid; keep it while the preview is tried.
			tiny = thumbBytes
		}
	}

	previewBytes, err := toolExtract(ctx, source, tagPreview)
	switch {
	case err != nil:
		reasons = append(reasons, "preview: "+err.Error())
	case len(previewBytes) > 0:
		cfg, cfgErr := imageConfig(previewBytes)
		switch {
		case cfgErr != nil:
			reasons = append(reasons, "preview: "+cfgErr.Error())
		case cfg.Width > maxEmbeddedEdge || cfg.Height > maxEmbeddedEdge:
			reasons = append(reasons, fmt.Sprintf("preview: implausible size %dx%d", cfg.Width, cfg.Height))
		default:
			if img, err := decodeJPEG(previewBytes); err == nil {
				return img, previewBytes, nil
			} else {
				reasons = append(reasons, "preview: "+err.Error())
			}
		}
	}

	if len(tiny) > 0 {
		if img, err := decodeJPEG(tiny); err == nil {
			return img, tiny, nil
		}
	}
	if len(reasons) == 0 {
		return nil, nil, fmt.Errorf("thumb: %s has no embedded preview", filepath.Base(source))
	}
	return nil, nil, fmt.Errorf("thumb: no usable embedded preview in %s (%s)", filepath.Base(source), strings.Join(reasons, "; "))
}

// render scales and rotates img into the final thumbnail. Scaling happens
// first: the transforms then run on at most MaxEdge pixels, so a 24 MP
// preview is never transformed or encoded at full size. It never upscales.
func render(img image.Image, orientation int) ([]byte, error) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil, errors.New("thumb: empty embedded image")
	}
	if orientation < 1 || orientation > 8 {
		orientation = 1
	}

	// The display size is the source size with the swapped axes of
	// orientations 5-8; the long edge of that is capped at MaxEdge.
	dispW, dispH := w, h
	if orientation >= 5 {
		dispW, dispH = h, w
	}
	scale := 1.0
	if long := longEdge(dispW, dispH); long > MaxEdge {
		scale = float64(MaxEdge) / float64(long)
	}
	tw := max(1, int(math.Round(float64(dispW)*scale)))
	th := max(1, int(math.Round(float64(dispH)*scale)))
	rawW, rawH := tw, th
	if orientation >= 5 {
		rawW, rawH = th, tw
	}

	var scaled image.Image = img
	if rawW != w || rawH != h {
		dst := image.NewRGBA(image.Rect(0, 0, rawW, rawH))
		draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Src, nil)
		scaled = dst
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, orient(scaled, orientation), &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, fmt.Errorf("thumb: encode JPEG: %w", err)
	}
	return buf.Bytes(), nil
}

// orient returns img transformed so that the EXIF orientation value displays
// upright. Values outside 1-8 are treated as 1. The result has the pixel
// dimensions viewers expect; the caller must keep it small.
func orient(img image.Image, orientation int) image.Image {
	if orientation <= 1 || orientation > 8 {
		return img
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	dw, dh := w, h
	if orientation >= 5 {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			var sx, sy int
			switch orientation {
			case 2: // Mirror horizontal.
				sx, sy = w-1-x, y
			case 3: // Rotate 180.
				sx, sy = w-1-x, h-1-y
			case 4: // Mirror vertical.
				sx, sy = x, h-1-y
			case 5: // Transpose.
				sx, sy = y, x
			case 6: // Rotate 90 CW.
				sx, sy = y, h-1-x
			case 7: // Transverse.
				sx, sy = w-1-y, h-1-x
			case 8: // Rotate 270 CW.
				sx, sy = w-1-y, x
			}
			dst.Set(x, y, img.At(b.Min.X+sx, b.Min.Y+sy))
		}
	}
	return dst
}

// decodeJPEG decodes embedded image bytes. Every RAW format RawDrop supports
// stores JPEG previews.
func decodeJPEG(data []byte) (image.Image, error) {
	if !bytes.HasPrefix(data, jpegMagic) {
		return nil, errors.New("not JPEG data")
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode JPEG: %w", err)
	}
	if img.Bounds().Dx() <= 0 || img.Bounds().Dy() <= 0 {
		return nil, errors.New("empty image")
	}
	return img, nil
}

// imageConfig reads only the header: it is used to reject tiny or absurd
// embedded images before paying for a full decode.
func imageConfig(data []byte) (image.Config, error) {
	if !bytes.HasPrefix(data, jpegMagic) {
		return image.Config{}, errors.New("not JPEG data")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return image.Config{}, fmt.Errorf("read JPEG header: %w", err)
	}
	return cfg, nil
}

func longEdge(w, h int) int {
	if w > h {
		return w
	}
	return h
}
