// Package model contains the data shared by RawDrop's scanner, exporter, job
// store and native UI. It deliberately has no dependency on MyGo or on any
// external tool so that the processing core can be tested on its own.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"time"
)

// Status is the lifecycle state of one source photo.
type Status string

const (
	StatusQueued     Status = "queued"
	StatusInspecting Status = "inspecting"
	StatusExtracting Status = "extracting"
	StatusRotating   Status = "rotating"
	StatusVerifying  Status = "verifying"
	StatusDone       Status = "done"
	StatusSkipped    Status = "skipped"
	StatusFailed     Status = "failed"
	StatusCanceled   Status = "canceled"
)

// Terminal reports whether no further processing is expected for s.
func (s Status) Terminal() bool {
	switch s {
	case StatusDone, StatusSkipped, StatusFailed, StatusCanceled:
		return true
	default:
		return false
	}
}

// Item is one photo in a job.
type Item struct {
	// ID is stable for a job: the SHA-256 of the cleaned absolute source path.
	ID string `json:"id"`
	// Source is the absolute path of the original file.
	Source string `json:"source"`
	// Name is the file's base name, used for the output and for display.
	Name string `json:"name"`
	// OutputName is the base name chosen for the exported JPEG, including any
	// collision suffix. Empty until the plan is created.
	OutputName string `json:"outputName,omitempty"`
	// OutputPath is the committed output, set only after a successful export.
	OutputPath string `json:"outputPath,omitempty"`
	// OutputSHA256 is the hash recorded after the output is verified and
	// committed. It is used to validate a resumed job.
	OutputSHA256 string `json:"outputSha256,omitempty"`

	Size        int64     `json:"size"`
	ModTime     time.Time `json:"modTime"`
	Status      Status    `json:"status"`
	Message     string    `json:"message,omitempty"`
	Warning     string    `json:"warning,omitempty"`
	Width       int       `json:"width,omitempty"`
	Height      int       `json:"height,omitempty"`
	Orientation int       `json:"orientation,omitempty"`
	Rotated     bool      `json:"rotated,omitempty"`
	// Thumbnail is a small encoded JPEG for the grid. It is never persisted.
	Thumbnail []byte `json:"-"`
}

// Settings controls a conversion.
type Settings struct {
	// OutputDir is the absolute destination directory.
	OutputDir string `json:"outputDir"`
	// CorrectOrientation rotates photos to their upright orientation. The
	// preferred implementation is a lossless JPEG DCT transform.
	CorrectOrientation bool `json:"correctOrientation"`
	// PreserveLocation keeps GPS tags. Off by default.
	PreserveLocation bool `json:"preserveLocation"`
}

// DefaultSettings returns the safe defaults used for a new job.
func DefaultSettings(outputDir string) Settings {
	return Settings{
		OutputDir:          outputDir,
		CorrectOrientation: true,
		PreserveLocation:   false,
	}
}

// Job is a resumable conversion of one set of inputs.
type Job struct {
	Version  int       `json:"version"`
	ID       string    `json:"id"`
	Inputs   []string  `json:"inputs"`
	Settings Settings  `json:"settings"`
	Items    []*Item   `json:"items"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
}

// NewJob creates an empty job for inputs and settings.
func NewJob(inputs []string, settings Settings) *Job {
	now := time.Now().UTC()
	j := &Job{
		Version:  1,
		Inputs:   append([]string(nil), inputs...),
		Settings: settings,
		Created:  now,
		Updated:  now,
	}
	h := sha256.New()
	for _, in := range inputs {
		h.Write([]byte(filepath.Clean(in)))
		h.Write([]byte{0})
	}
	h.Write([]byte(settings.OutputDir))
	j.ID = hex.EncodeToString(h.Sum(nil))[:16]
	return j
}

// Counts summarizes item states for the UI and the result screen.
type Counts struct {
	Total     int
	Done      int
	Skipped   int
	Failed    int
	Canceled  int
	Remaining int
}

// Count returns the states of all items.
func (j *Job) Count() Counts {
	c := Counts{Total: len(j.Items)}
	for _, item := range j.Items {
		switch item.Status {
		case StatusDone:
			c.Done++
		case StatusSkipped:
			c.Skipped++
		case StatusFailed:
			c.Failed++
		case StatusCanceled:
			c.Canceled++
		default:
			c.Remaining++
		}
	}
	return c
}

// Finished reports whether every item reached a terminal state.
func (j *Job) Finished() bool {
	for _, item := range j.Items {
		if !item.Status.Terminal() {
			return false
		}
	}
	return len(j.Items) > 0
}

// rawExtensionList is the single source of truth for recognized RAW
// extensions. They are lowercase, dot-free and sorted so the scanner and the
// native file-picker cannot drift apart. Being listed here only means "route
// this file to the exporter"; it is not a promise that a camera model's
// embedded-JPEG layout is exportable. See docs/FORMAT_SUPPORT.md for the
// recognized/exportable/verified distinction.
var rawExtensionList = []string{
	"3fr", "arw", "cr2", "cr3", "dng", "erf", "iiq", "mef", "mos",
	"nef", "nrw", "orf", "pef", "raf", "raw", "rw2", "rwl", "sr2",
	"srf", "srw",
}

// RawExtensions maps a lowercase, dotted extension to true. It is derived from
// rawExtensionList so the recognized set has exactly one definition.
var RawExtensions = func() map[string]bool {
	m := make(map[string]bool, len(rawExtensionList))
	for _, ext := range rawExtensionList {
		m["."+ext] = true
	}
	return m
}()

// RawExtensionList returns the recognized extensions without their leading
// dots, lowercase and sorted, as a copy. The native picker builds its filter
// from this list so it always matches the scanner.
func RawExtensionList() []string {
	return append([]string(nil), rawExtensionList...)
}

// IsRawName reports whether name has a known RAW extension. Matching is
// case-insensitive, so ".CR2" and ".Arw" are recognized.
func IsRawName(name string) bool {
	return RawExtensions[strings.ToLower(filepath.Ext(name))]
}
