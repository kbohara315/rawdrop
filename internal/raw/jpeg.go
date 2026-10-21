package raw

import (
	"bufio"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"os"
)

// jpegHeaderLimit bounds how much of an embedded JPEG is read when only its
// header is needed.
const jpegHeaderLimit = 1 << 20

// jpegDimensions reads a JPEG's pixel dimensions from its header without
// decoding pixels. It fails for anything that is not a decodable JPEG or has
// non-positive dimensions.
func jpegDimensions(r io.Reader) (int, int, error) {
	cfg, err := jpeg.DecodeConfig(r)
	if err != nil {
		return 0, 0, fmt.Errorf("not a decodable JPEG: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return 0, 0, fmt.Errorf("JPEG has non-positive dimensions %dx%d", cfg.Width, cfg.Height)
	}
	return cfg.Width, cfg.Height, nil
}

// validateJPEGFile checks that path holds a complete, structurally sane JPEG
// and returns its stored dimensions. A truncated extraction that happens to
// keep a readable header still fails here because the end-of-image marker is
// missing.
func validateJPEGFile(path string) (int, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	w, h, err := jpegDimensions(f)
	if err != nil {
		return 0, 0, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, 0, err
	}
	if err := checkJPEGStructure(f); err != nil {
		return 0, 0, err
	}
	return w, h, nil
}

// checkJPEGStructure walks a JPEG's marker segments and requires the file to
// start with SOI and end with EOI, after at least one frame header and one
// scan. Segment lengths must be plausible and every segment must be present
// in full; a truncated or malformed file fails. Trailing bytes after EOI are
// tolerated, as some tools pad the end of the file.
func checkJPEGStructure(r io.Reader) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var soi [2]byte
	if _, err := io.ReadFull(br, soi[:]); err != nil || soi[0] != 0xFF || soi[1] != 0xD8 {
		return errors.New("JPEG does not start with an SOI marker")
	}
	inScan := false
	seenFrame := false
	seenScan := false
	for {
		marker, err := nextMarker(br, inScan)
		if err != nil {
			return err
		}
		inScan = false
		switch {
		case marker == 0xD9: // EOI
			if !seenFrame || !seenScan {
				return errors.New("JPEG ends before any image data")
			}
			return nil
		case marker == 0xD8:
			return errors.New("unexpected SOI marker inside JPEG")
		case marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7):
			// TEM and restart markers are standalone.
			continue
		case marker == 0xDA: // SOS
			if err := skipSegment(br); err != nil {
				return err
			}
			seenScan = true
			inScan = true
		default:
			if isStartOfFrame(marker) {
				seenFrame = true
			}
			if err := skipSegment(br); err != nil {
				return err
			}
		}
	}
}

// nextMarker returns the next marker code. Inside entropy-coded scan data the
// byte stuffing (FF00) and restart markers are data and are skipped; the
// first real marker ends the scan.
func nextMarker(br *bufio.Reader, inScan bool) (byte, error) {
	for {
		b, err := br.ReadByte()
		if err != nil {
			return 0, errors.New("truncated JPEG: missing EOI marker")
		}
		if b != 0xFF {
			if inScan {
				continue
			}
			return 0, fmt.Errorf("malformed JPEG: byte %#02x outside a scan segment", b)
		}
		for {
			m, err := br.ReadByte()
			if err != nil {
				return 0, errors.New("truncated JPEG: dangling marker prefix")
			}
			if m == 0xFF {
				continue // fill byte
			}
			if inScan {
				if m == 0x00 || (m >= 0xD0 && m <= 0xD7) {
					break // stuffed data byte or restart marker
				}
				return m, nil
			}
			if m == 0x00 {
				return 0, errors.New("malformed JPEG: stuffed byte outside a scan segment")
			}
			return m, nil
		}
	}
}

// skipSegment consumes a marker segment's two-byte length and its payload.
func skipSegment(br *bufio.Reader) error {
	var l [2]byte
	if _, err := io.ReadFull(br, l[:]); err != nil {
		return errors.New("truncated JPEG: missing segment length")
	}
	n := int(l[0])<<8 | int(l[1])
	if n < 2 {
		return fmt.Errorf("malformed JPEG: segment length %d", n)
	}
	if _, err := io.CopyN(io.Discard, br, int64(n-2)); err != nil {
		return errors.New("truncated JPEG: segment shorter than its length")
	}
	return nil
}

// isStartOfFrame reports whether a marker introduces a JPEG frame header
// (SOF0-SOF15, excluding DHT/DAC/JPGn).
func isStartOfFrame(marker byte) bool {
	switch marker {
	case 0xC0, 0xC1, 0xC2, 0xC3, 0xC5, 0xC6, 0xC7,
		0xC9, 0xCA, 0xCB, 0xCD, 0xCE, 0xCF:
		return true
	default:
		return false
	}
}
