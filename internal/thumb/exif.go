package thumb

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
)

// This file is a deliberately tiny EXIF reader. Thumbnails only need the
// Orientation tag, so instead of a full EXIF library it walks the IFD0 of a
// TIFF header and the APP1 segment of an embedded JPEG.

const (
	exifOrientationTag = 0x0112 // Orientation
	exifIFDPointerTag  = 0x8769 // Exif sub-IFD
	exifTypeShort      = 3
	exifTypeLong       = 4
)

// maxEXIFPrefix bounds how much of a RAW file is read to find IFD0.
const maxEXIFPrefix = 128 << 10

// sourceOrientation returns the orientation to apply to an embedded image
// that carries no EXIF of its own. It parses the RAW container first and
// falls back to exiftool for formats without a TIFF header (for example
// CR3's ISO BMFF). Unknown orientation degrades to 1.
func sourceOrientation(ctx context.Context, source string) int {
	if orientation := orientationFromFile(source); orientation != 0 {
		return orientation
	}
	if orientation, err := toolOrientation(ctx, source); err == nil && orientation >= 1 && orientation <= 8 {
		return orientation
	}
	return 1
}

// orientationFromFile parses the TIFF header of a RAW file (CR2, NEF, ARW,
// DNG, ORF, RW2 and other TIFF-derived formats) and returns its orientation,
// or 0 when it cannot be determined from the file prefix.
func orientationFromFile(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	buf := make([]byte, maxEXIFPrefix)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return 0
	}
	return tiffOrientation(buf[:n])
}

// jpegEXIFOrientation returns the EXIF orientation stored in a JPEG's APP1
// segment, or 0 when the JPEG carries none.
func jpegEXIFOrientation(data []byte) int {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return 0
	}
	for i := 2; i+4 <= len(data); {
		if data[i] != 0xFF {
			return 0 // Not a marker: the segment stream ended.
		}
		marker := data[i+1]
		switch {
		case marker == 0x01 || marker >= 0xD0 && marker <= 0xD7:
			i += 2 // Standalone markers carry no payload.
			continue
		case marker == 0xDA || marker == 0xD9:
			return 0 // Start of scan or end of image: no EXIF seen.
		}
		size := int(binary.BigEndian.Uint16(data[i+2 : i+4]))
		if size < 2 {
			return 0
		}
		end := i + 2 + size
		if end > len(data) {
			end = len(data)
		}
		if marker == 0xE1 {
			payload := data[i+4 : end]
			const header = "Exif\x00\x00"
			if len(payload) >= len(header) && string(payload[:len(header)]) == header {
				if orientation := tiffOrientation(payload[len(header):]); orientation != 0 {
					return orientation
				}
			}
		}
		i = end
	}
	return 0
}

// tiffOrientation returns the orientation stored in a TIFF-derived file, or
// 0 when it is absent.
func tiffOrientation(data []byte) int {
	order, ifd0, ok := tiffHeader(data)
	if !ok {
		return 0
	}
	if orientation := ifdOrientation(data, order, ifd0); orientation != 0 {
		return orientation
	}
	// Orientation normally lives in IFD0, but an Exif sub-IFD copied from a
	// JPEG source occasionally carries it; one bounded follow-up is cheap.
	if pointer, ok := tiffValue(data, order, ifd0, exifIFDPointerTag, exifTypeLong); ok {
		return ifdOrientation(data, order, int(pointer))
	}
	return 0
}

// tiffHeader validates the 8-byte TIFF header and returns its byte order and
// IFD0 offset.
func tiffHeader(data []byte) (binary.ByteOrder, int, bool) {
	if len(data) < 8 {
		return nil, 0, false
	}
	var order binary.ByteOrder
	switch {
	case data[0] == 'I' && data[1] == 'I':
		order = binary.LittleEndian
	case data[0] == 'M' && data[1] == 'M':
		order = binary.BigEndian
	default:
		return nil, 0, false
	}
	if order.Uint16(data[2:4]) != 42 {
		return nil, 0, false
	}
	return order, int(order.Uint32(data[4:8])), true
}

// ifdOrientation reads the Orientation tag from the IFD at offset off.
func ifdOrientation(data []byte, order binary.ByteOrder, off int) int {
	value, ok := tiffValue(data, order, off, exifOrientationTag, exifTypeShort)
	if !ok {
		return 0
	}
	if value < 1 || value > 8 {
		return 0
	}
	return int(value)
}

// tiffValue returns the first value of tag in the IFD at offset off when the
// entry has the expected type and an inline value (fits in four bytes).
func tiffValue(data []byte, order binary.ByteOrder, off int, tag uint16, wantType uint16) (uint32, bool) {
	if off < 8 || off+2 > len(data) {
		return 0, false
	}
	count := int(order.Uint16(data[off : off+2]))
	if count > (len(data)-off-2)/12 {
		count = (len(data) - off - 2) / 12 // Truncated IFD.
	}
	for i := 0; i < count; i++ {
		entry := off + 2 + i*12
		if order.Uint16(data[entry:entry+2]) != tag {
			continue
		}
		if order.Uint16(data[entry+2:entry+4]) != wantType {
			return 0, false
		}
		if order.Uint32(data[entry+4:entry+8]) < 1 {
			return 0, false
		}
		// A value that fits in four bytes starts at the field's first
		// byte, so read it with its own width instead of always as a
		// little-endian-friendly uint32.
		switch wantType {
		case exifTypeShort:
			return uint32(order.Uint16(data[entry+8 : entry+10])), true
		case exifTypeLong:
			return order.Uint32(data[entry+8 : entry+12]), true
		default:
			return 0, false
		}
	}
	return 0, false
}
