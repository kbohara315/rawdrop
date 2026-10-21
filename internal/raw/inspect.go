package raw

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
)

// inspection is the internal result of reading a RAW file: the public Info
// plus every embedded JPEG candidate, which Export uses to pick what to
// extract.
type inspection struct {
	info       Info
	candidates []candidate
}

// candidate is one embedded JPEG tag found in a RAW file. Offsets are byte
// positions inside the RAW file when ExifTool reports them; without offsets
// the image data can only be reached by asking ExifTool to extract the tag.
type candidate struct {
	tag       string
	group     string
	start     int64
	length    int64
	hasOffset bool
	width     int
	height    int
	dimsKnown bool
}

// selector is the group-qualified tag name passed back to ExifTool, which
// picks the exact occurrence seen during inspection.
func (c candidate) selector() string {
	if c.group == "" {
		return c.tag
	}
	return c.group + ":" + c.tag
}

func (c candidate) area() int { return c.width * c.height }

func (c candidate) qualified(info Info) bool {
	return c.dimsKnown && isFullSize(c.width, c.height, info)
}

// embeddedJPEGTags lists the embedded full-resolution JPEG tags RawDrop
// considers, in preference order. Thumbnails are deliberately not considered.
var embeddedJPEGTags = []string{"PreviewImage", "JpgFromRaw", "OtherImage"}

// inspectSelectors is the ExifTool tag list used for inspection. ImageWidth
// and ImageHeight are the camera's rendered dimensions; the raw sensor size
// (IFD3 on CR2) and thumbnail sizes (IFD2/IFD1) are reported as well and are
// ranked below IFD0 when the JSON is parsed.
var inspectSelectors = []string{
	"-ImageWidth", "-ImageHeight",
	"-Orientation",
	"-Make", "-Model",
	"-PreviewImage", "-JpgFromRaw", "-OtherImage",
	"-PreviewImageStart", "-PreviewImageLength",
	"-JpgFromRawStart", "-JpgFromRawLength",
	"-OtherImageStart", "-OtherImageLength",
}

// exifDoc is one ExifTool -j document. With -G1 every metadata key carries a
// group prefix such as "IFD0:ImageWidth".
type exifDoc map[string]any

// inspectFile reads src's metadata, probes the embedded JPEG headers, and
// reports the best candidate.
func inspectFile(ctx context.Context, src string) (inspection, error) {
	if err := ctx.Err(); err != nil {
		return inspection{}, err
	}
	src, err := absolutePath(src)
	if err != nil {
		return inspection{}, err
	}
	bin, err := exiftoolPath()
	if err != nil {
		return inspection{}, err
	}
	doc, err := readExifDoc(ctx, bin, src, inspectSelectors)
	if err != nil {
		return inspection{}, err
	}
	insp, err := parseInspection(doc)
	if err != nil {
		return inspection{}, err
	}
	for i := range insp.candidates {
		insp.candidates[i].probe(ctx, src)
	}
	if err := ctx.Err(); err != nil {
		return inspection{}, err
	}
	insp.info.choosePreview(insp.candidates)
	return insp, nil
}

// readExifDoc asks ExifTool for specific tags as JSON.
func readExifDoc(ctx context.Context, bin, path string, selectors []string) (exifDoc, error) {
	args := append([]string{"-j", "-n", "-G1"}, selectors...)
	args = append(args, "--", path)
	out, err := captureTool(ctx, bin, args)
	if err != nil {
		return nil, err
	}
	return parseExifDoc(out)
}

func parseExifDoc(raw []byte) (exifDoc, error) {
	var docs []exifDoc
	if err := json.Unmarshal(raw, &docs); err != nil {
		return nil, fmt.Errorf("exiftool: invalid JSON output: %w", err)
	}
	if len(docs) == 0 {
		return nil, errors.New("exiftool: empty metadata output")
	}
	doc := docs[0]
	if msg := stringTag(doc, "Error"); msg != "" {
		return nil, fmt.Errorf("exiftool: %s", msg)
	}
	return doc, nil
}

// parseInspection converts an ExifTool document into RawDrop's view of the
// file. Camera dimensions prefer IFD0, the camera's rendered image, over the
// raw sensor area (IFD3) and thumbnails (IFD2/IFD1).
func parseInspection(doc exifDoc) (inspection, error) {
	var insp inspection
	if w, _, ok := intTag(doc, "ImageWidth", "IFD0"); ok {
		insp.info.Width = w
	}
	if h, _, ok := intTag(doc, "ImageHeight", "IFD0"); ok {
		insp.info.Height = h
	}
	orientation := 1
	if v, _, ok := intTag(doc, "Orientation", "IFD0"); ok && v >= 1 && v <= 8 {
		orientation = v
	}
	insp.info.Orientation = orientation
	insp.info.Make = stringTag(doc, "Make")
	insp.info.Model = stringTag(doc, "Model")
	for _, tag := range embeddedJPEGTags {
		if c, ok := findCandidate(doc, tag); ok {
			insp.candidates = append(insp.candidates, c)
		}
	}
	return insp, nil
}

// findCandidate records a embedded JPEG tag and, when ExifTool reports the
// tag's position in the file, its byte range.
func findCandidate(doc exifDoc, tag string) (candidate, bool) {
	group := ""
	found := false
	for _, v := range findTags(doc, tag) {
		if !isAbsentValue(v.value) {
			group, found = v.group, true
			break
		}
	}
	if !found {
		return candidate{}, false
	}
	c := candidate{tag: tag, group: group}
	if start, _, ok := intTag(doc, tag+"Start", group); ok {
		if length, _, ok := intTag(doc, tag+"Length", group); ok && start >= 0 && length > 0 {
			c.start, c.length, c.hasOffset = int64(start), int64(length), true
		}
	}
	return c, true
}

// probe fills in the candidate's dimensions without extracting the JPEG. It
// reads the header at the tag's reported offset when possible, and otherwise
// streams only the beginning of the tag from ExifTool. Failures are not
// fatal: Export re-extracts and validates the candidate anyway.
func (c *candidate) probe(ctx context.Context, src string) {
	if c.hasOffset {
		if w, h, err := jpegDimensionsAt(src, c.start, c.length); err == nil {
			c.width, c.height, c.dimsKnown = w, h, true
			return
		}
	}
	bin, err := exiftoolPath()
	if err != nil {
		return
	}
	data, err := toolPrefix(ctx, bin, []string{"-b", "-" + c.selector(), "--", src}, jpegHeaderLimit)
	if err != nil {
		return
	}
	w, h, err := jpegDimensions(bytes.NewReader(data))
	if err != nil {
		return
	}
	c.width, c.height, c.dimsKnown = w, h, true
}

// choosePreview fills Info's preview fields. The first candidate in
// preference order that reaches the camera's usable size wins; when none do,
// the largest known candidate is reported for diagnostics and HasPreview
// stays false.
func (info *Info) choosePreview(cands []candidate) {
	var diag *candidate
	for i := range cands {
		c := &cands[i]
		if c.qualified(*info) {
			info.HasPreview = true
			info.PreviewTag = c.tag
			info.PreviewWidth, info.PreviewHeight = c.width, c.height
			return
		}
		if diag == nil {
			diag = c
		} else if c.area() > diag.area() {
			diag = c
		}
	}
	if diag != nil {
		info.PreviewTag = diag.tag
		info.PreviewWidth, info.PreviewHeight = diag.width, diag.height
	}
}

// tagValue is one tag occurrence in an ExifTool document.
type tagValue struct {
	group string
	name  string
	value any
}

// preferredGroups ranks ExifTool groups so the camera's rendered image
// description (IFD0) is chosen over raw sensor data (IFD3) and thumbnails
// (IFD2, IFD1).
var preferredGroups = []string{"IFD0", "EXIF", "IFD", "Main", "QuickTime", "XMP", "IFD1"}

func groupRank(group string) int {
	for i, g := range preferredGroups {
		if strings.EqualFold(group, g) {
			return i
		}
	}
	return len(preferredGroups)
}

// splitTagKey splits "IFD0:ImageWidth" into its group and bare tag name.
func splitTagKey(key string) (group, name string) {
	if i := strings.LastIndex(key, ":"); i >= 0 {
		return key[:i], key[i+1:]
	}
	return "", key
}

// findTags returns every occurrence of a bare tag name, ordered by group
// preference so the first entry is the one RawDrop prefers.
func findTags(doc exifDoc, name string) []tagValue {
	var out []tagValue
	for key, value := range doc {
		group, bare := splitTagKey(key)
		if bare != "" && strings.EqualFold(bare, name) {
			out = append(out, tagValue{group: group, name: bare, value: value})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		ri, rj := groupRank(out[i].group), groupRank(out[j].group)
		if ri != rj {
			return ri < rj
		}
		return out[i].group < out[j].group
	})
	return out
}

// intTag returns the first numeric value of a tag, preferring preferGroup.
func intTag(doc exifDoc, name, preferGroup string) (int, string, bool) {
	values := findTags(doc, name)
	if preferGroup != "" {
		for _, v := range values {
			if strings.EqualFold(v.group, preferGroup) {
				if n, ok := asInt(v.value); ok {
					return n, v.group, true
				}
			}
		}
	}
	for _, v := range values {
		if n, ok := asInt(v.value); ok {
			return n, v.group, true
		}
	}
	return 0, "", false
}

// stringTag returns the first non-empty string value of a tag.
func stringTag(doc exifDoc, name string) string {
	for _, v := range findTags(doc, name) {
		if s, ok := v.value.(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func asInt(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		if t != math.Trunc(t) {
			return 0, false
		}
		return int(t), true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

// isAbsentValue reports whether a JSON value means "tag not present".
func isAbsentValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(t) == ""
	default:
		return false
	}
}

// jpegDimensionsAt reads the dimensions of an embedded JPEG from [start,
// start+length) inside the RAW file, reading only as much of the header as
// DecodeConfig needs.
func jpegDimensionsAt(path string, start, length int64) (int, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	if _, err := f.Seek(start, 0); err != nil {
		return 0, 0, err
	}
	if length > jpegHeaderLimit {
		length = jpegHeaderLimit
	}
	return jpegDimensions(&io.LimitedReader{R: f, N: length})
}
