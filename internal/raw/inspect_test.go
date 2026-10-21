package raw

import (
	"strings"
	"testing"
)

// realInspectionJSON is ExifTool 13.50 output for a Canon EOS 80D portrait
// CR2 (IMG_3128.CR2), trimmed to the inspected tags.
const realInspectionJSON = `[{
  "SourceFile": "IMG_3128.CR2",
  "IFD2:ImageWidth": 524,
  "IFD0:ImageWidth": 6000,
  "IFD3:ImageWidth": 6288,
  "IFD0:ImageHeight": 4000,
  "IFD2:ImageHeight": 338,
  "IFD3:ImageHeight": 4056,
  "IFD0:Orientation": 8,
  "IFD0:Make": "Canon",
  "IFD0:Model": "Canon EOS 80D",
  "IFD0:PreviewImage": "(Binary data 2159818 bytes, use -b option to extract)",
  "IFD0:PreviewImageStart": 70180,
  "IFD0:PreviewImageLength": 2159818
}]`

func TestParseExifDoc(t *testing.T) {
	doc, err := parseExifDoc([]byte(realInspectionJSON))
	if err != nil {
		t.Fatalf("parseExifDoc() error: %v", err)
	}
	if got := stringTag(doc, "Model"); got != "Canon EOS 80D" {
		t.Errorf("Model = %q, want %q", got, "Canon EOS 80D")
	}
	if _, err := parseExifDoc([]byte(`[]`)); err == nil {
		t.Error("parseExifDoc(empty array) = nil error, want failure")
	}
	if _, err := parseExifDoc([]byte(`not json`)); err == nil {
		t.Error("parseExifDoc(garbage) = nil error, want failure")
	}
	if _, err := parseExifDoc([]byte(`[{"Error": "File format error"}]`)); err == nil || !strings.Contains(err.Error(), "File format error") {
		t.Errorf("parseExifDoc(error doc) = %v, want File format error", err)
	}
}

func TestParseInspectionRealCR2(t *testing.T) {
	doc, err := parseExifDoc([]byte(realInspectionJSON))
	if err != nil {
		t.Fatalf("parseExifDoc() error: %v", err)
	}
	insp, err := parseInspection(doc)
	if err != nil {
		t.Fatalf("parseInspection() error: %v", err)
	}
	// IFD0, the camera's rendered image, must win over IFD2 (thumbnail) and
	// IFD3 (raw sensor).
	if insp.info.Width != 6000 || insp.info.Height != 4000 {
		t.Errorf("camera dimensions = %dx%d, want 6000x4000", insp.info.Width, insp.info.Height)
	}
	if insp.info.Orientation != 8 {
		t.Errorf("Orientation = %d, want 8", insp.info.Orientation)
	}
	if insp.info.Make != "Canon" {
		t.Errorf("Make = %q, want Canon", insp.info.Make)
	}
	if len(insp.candidates) != 1 {
		t.Fatalf("candidates = %d, want 1", len(insp.candidates))
	}
	c := insp.candidates[0]
	if c.tag != "PreviewImage" || c.group != "IFD0" {
		t.Errorf("candidate = %s/%s, want PreviewImage/IFD0", c.group, c.tag)
	}
	if !c.hasOffset || c.start != 70180 || c.length != 2159818 {
		t.Errorf("candidate offset = %v,%d,%d, want true,70180,2159818", c.hasOffset, c.start, c.length)
	}
	if got, want := c.selector(), "IFD0:PreviewImage"; got != want {
		t.Errorf("selector = %q, want %q", got, want)
	}
}

func TestParseInspectionVariants(t *testing.T) {
	tests := []struct {
		name            string
		json            string
		wantWidth       int
		wantHeight      int
		wantOrientation int
		wantTags        []string
	}{
		{
			name:            "orientation missing defaults to 1",
			json:            `[{"IFD0:ImageWidth": 4000, "IFD0:ImageHeight": 3000}]`,
			wantWidth:       4000,
			wantHeight:      3000,
			wantOrientation: 1,
		},
		{
			name:            "orientation as string",
			json:            `[{"IFD0:ImageWidth": 4000, "IFD0:ImageHeight": 3000, "IFD0:Orientation": "6"}]`,
			wantWidth:       4000,
			wantHeight:      3000,
			wantOrientation: 6,
		},
		{
			name:            "invalid orientation falls back to 1",
			json:            `[{"IFD0:ImageWidth": 4000, "IFD0:ImageHeight": 3000, "IFD0:Orientation": 9}]`,
			wantWidth:       4000,
			wantHeight:      3000,
			wantOrientation: 1,
		},
		{
			name:            "dims from another group when ifd0 missing",
			json:            `[{"EXIF:ImageWidth": 5000, "EXIF:ImageHeight": 4000}]`,
			wantWidth:       5000,
			wantHeight:      4000,
			wantOrientation: 1,
		},
		{
			name:            "all three candidate tags",
			json:            `[{"IFD0:ImageWidth": 100, "IFD0:ImageHeight": 100, "IFD0:PreviewImage": "x", "IFD0:JpgFromRaw": "y", "IFD0:OtherImage": "z"}]`,
			wantWidth:       100,
			wantHeight:      100,
			wantOrientation: 1,
			wantTags:        []string{"PreviewImage", "JpgFromRaw", "OtherImage"},
		},
		{
			name:            "empty preview value is not a candidate",
			json:            `[{"IFD0:ImageWidth": 100, "IFD0:ImageHeight": 100, "IFD0:PreviewImage": ""}]`,
			wantWidth:       100,
			wantHeight:      100,
			wantOrientation: 1,
			wantTags:        nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, err := parseExifDoc([]byte(tt.json))
			if err != nil {
				t.Fatalf("parseExifDoc() error: %v", err)
			}
			insp, err := parseInspection(doc)
			if err != nil {
				t.Fatalf("parseInspection() error: %v", err)
			}
			if insp.info.Width != tt.wantWidth || insp.info.Height != tt.wantHeight {
				t.Errorf("dimensions = %dx%d, want %dx%d", insp.info.Width, insp.info.Height, tt.wantWidth, tt.wantHeight)
			}
			if insp.info.Orientation != tt.wantOrientation {
				t.Errorf("Orientation = %d, want %d", insp.info.Orientation, tt.wantOrientation)
			}
			var tags []string
			for _, c := range insp.candidates {
				tags = append(tags, c.tag)
			}
			if len(tags) != len(tt.wantTags) {
				t.Fatalf("candidates = %v, want %v", tags, tt.wantTags)
			}
			for i := range tags {
				if tags[i] != tt.wantTags[i] {
					t.Fatalf("candidates = %v, want %v", tags, tt.wantTags)
				}
			}
		})
	}
}

func TestFindCandidateOffsetsPreferSameGroup(t *testing.T) {
	doc, err := parseExifDoc([]byte(`[{
		"IFD0:JpgFromRaw": "(Binary data 100 bytes, use -b option to extract)",
		"IFD0:JpgFromRawStart": 500,
		"IFD0:JpgFromRawLength": 100,
		"Canon:JpgFromRawStart": 9999
	}]`))
	if err != nil {
		t.Fatalf("parseExifDoc() error: %v", err)
	}
	c, ok := findCandidate(doc, "JpgFromRaw")
	if !ok {
		t.Fatal("findCandidate() = not found, want candidate")
	}
	if !c.hasOffset || c.start != 500 || c.length != 100 {
		t.Errorf("offset = %v,%d,%d, want true,500,100", c.hasOffset, c.start, c.length)
	}
}

func TestChoosePreview(t *testing.T) {
	cands := func(dims ...[3]int) []candidate {
		out := make([]candidate, 0, len(dims))
		for _, d := range dims {
			out = append(out, candidate{
				tag:       embeddedJPEGTags[d[0]],
				width:     d[1],
				height:    d[2],
				dimsKnown: d[1] > 0 && d[2] > 0,
			})
		}
		return out
	}
	const (
		preview = 0
		raw     = 1
		other   = 2
	)
	tests := []struct {
		name       string
		info       Info
		candidates []candidate
		wantHas    bool
		wantTag    string
		wantW      int
		wantH      int
	}{
		{
			name:       "full preview wins over small raw",
			info:       Info{Width: 6000, Height: 4000, Orientation: 8},
			candidates: cands([3]int{raw, 524, 338}, [3]int{preview, 6000, 4000}),
			wantHas:    true,
			wantTag:    "PreviewImage",
			wantW:      6000,
			wantH:      4000,
		},
		{
			name:       "pre-rotated portrait preview accepted",
			info:       Info{Width: 6000, Height: 4000, Orientation: 6},
			candidates: cands([3]int{preview, 4000, 6000}),
			wantHas:    true,
			wantTag:    "PreviewImage",
			wantW:      4000,
			wantH:      6000,
		},
		{
			name:       "small preview only is reported but not usable",
			info:       Info{Width: 6000, Height: 4000, Orientation: 1},
			candidates: cands([3]int{preview, 524, 338}),
			wantHas:    false,
			wantTag:    "PreviewImage",
			wantW:      524,
			wantH:      338,
		},
		{
			name:       "largest known candidate reported when none qualify",
			info:       Info{Width: 6000, Height: 4000, Orientation: 1},
			candidates: cands([3]int{raw, 524, 338}, [3]int{other, 3000, 2000}),
			wantHas:    false,
			wantTag:    "OtherImage",
			wantW:      3000,
			wantH:      2000,
		},
		{
			name:       "unknown dimensions are not usable",
			info:       Info{Width: 6000, Height: 4000, Orientation: 1},
			candidates: cands([3]int{preview, 0, 0}),
			wantHas:    false,
			wantTag:    "PreviewImage",
			wantW:      0,
			wantH:      0,
		},
		{
			name:       "no candidates",
			info:       Info{Width: 6000, Height: 4000, Orientation: 1},
			candidates: nil,
			wantHas:    false,
			wantTag:    "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := tt.info
			info.choosePreview(tt.candidates)
			if info.HasPreview != tt.wantHas || info.PreviewTag != tt.wantTag ||
				info.PreviewWidth != tt.wantW || info.PreviewHeight != tt.wantH {
				t.Errorf("choosePreview() = has=%v tag=%q %dx%d, want has=%v tag=%q %dx%d",
					info.HasPreview, info.PreviewTag, info.PreviewWidth, info.PreviewHeight,
					tt.wantHas, tt.wantTag, tt.wantW, tt.wantH)
			}
		})
	}
}
