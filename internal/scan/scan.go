// Package scan turns dropped paths into a stable, deduplicated list of photo
// photos for a job. It never opens photo contents: eligibility is decided by
// extension and the exporter still validates each file's metadata.
package scan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"rawdrop/internal/model"
)

// Result is the outcome of one scan.
type Result struct {
	// Items are the supported photos to process, in display order. Every item is
	// queued and has a stable ID and a planned output name.
	Items []*model.Item
	// Unsupported are explicitly dropped files whose extension is not a
	// known photo type. Files with other extensions found inside dropped
	// folders are ignored silently.
	Unsupported []string
	// SkippedDirs are dropped or walked paths that were excluded or could
	// not be read, for example the output folder or an unreadable
	// subfolder. They never fail the scan on their own.
	SkippedDirs []string
}

// ItemID returns the stable identity of a source path: the hexadecimal
// SHA-256 of its cleaned absolute path. It is exported so callers can relate
// journal entries to freshly scanned paths without reimplementing the rule.
func ItemID(source string) string {
	sum := sha256.Sum256([]byte(source))
	return hex.EncodeToString(sum[:])
}

// Collect walks inputs and returns one item per unique supported photo.
//
// File inputs are accepted only when their extension is a known RAW or JPEG type;
// other dropped files are recorded in Unsupported. Directory inputs are
// walked recursively: non-photo files inside them are ignored silently and
// unreadable directories are recorded in SkippedDirs without failing the
// scan. Directory symlinks are never followed. The output directory and
// everything inside it are excluded even when they are inputs or do not
// exist yet.
//
// Paths are absolute and cleaned, but symbolic links are not resolved, so
// items show the location the user dropped. Items come back in input order
// and lexicographically within each folder, with duplicate basenames
// disambiguated as "IMG_1234 (2).jpg".
//
// Collect returns an error when ctx is done, when inputs is empty, or when
// no input could be used at all. A single unusable input among usable ones
// is recorded in SkippedDirs instead.
func Collect(ctx context.Context, inputs []string, outputDir string) (Result, error) {
	if len(inputs) == 0 {
		return Result{}, errors.New("scan: no inputs")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	out := ""
	if strings.TrimSpace(outputDir) != "" {
		abs, err := filepath.Abs(outputDir)
		if err != nil {
			return Result{}, fmt.Errorf("scan: resolve output directory %q: %w", outputDir, err)
		}
		out = filepath.Clean(abs)
	}

	c := &collector{
		seen:        make(map[string]bool),
		skipped:     make(map[string]bool),
		unsupported: make(map[string]bool),
	}
	usable, unusable := 0, []string{}

	for _, input := range inputs {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if strings.TrimSpace(input) == "" {
			unusable = append(unusable, input)
			continue
		}
		abs, err := filepath.Abs(input)
		if err != nil {
			c.skipDir(input)
			unusable = append(unusable, input)
			continue
		}
		abs = filepath.Clean(abs)

		// An input inside the output folder is deliberately excluded, not
		// broken, so it does not count as unusable.
		if out != "" && inside(abs, out) {
			c.skipDir(abs)
			usable++
			continue
		}

		// Stat follows symlinks here: an explicitly dropped symlink is a
		// deliberate choice, unlike a link discovered during a walk.
		info, err := os.Stat(abs)
		if err != nil {
			c.skipDir(abs)
			unusable = append(unusable, abs)
			continue
		}

		switch {
		case info.IsDir():
			usable++
			if err := c.walk(ctx, abs, out); err != nil {
				return Result{}, err
			}
		case info.Mode().IsRegular():
			usable++
			if model.IsPhotoName(abs) {
				c.addItem(abs, info)
			} else {
				c.addUnsupported(abs)
			}
		default:
			// Devices, sockets and FIFOs are not photos.
			c.skipDir(abs)
			unusable = append(unusable, abs)
		}
	}

	if usable == 0 {
		return c.result(), fmt.Errorf("scan: every dropped item was unusable: %s", strings.Join(unusable, ", "))
	}

	c.nameOutputs()
	return c.result(), nil
}

// collector accumulates one scan. Maps keep the result deduplicated and the
// item list keeps the deterministic display order.
type collector struct {
	res         Result
	items       []*model.Item
	seen        map[string]bool
	skipped     map[string]bool
	unsupported map[string]bool
}

func (c *collector) result() Result {
	c.res.Items = c.items
	return c.res
}

// skipDir records a path that was excluded or could not be read.
func (c *collector) skipDir(path string) {
	if c.skipped[path] {
		return
	}
	c.skipped[path] = true
	c.res.SkippedDirs = append(c.res.SkippedDirs, path)
}

func (c *collector) addUnsupported(path string) {
	if c.unsupported[path] {
		return
	}
	c.unsupported[path] = true
	c.res.Unsupported = append(c.res.Unsupported, path)
}

// addItem appends one newly discovered photo. Repeated paths are dropped.
func (c *collector) addItem(path string, info fs.FileInfo) {
	if c.seen[path] {
		return
	}
	c.seen[path] = true
	c.items = append(c.items, &model.Item{
		ID:      ItemID(path),
		Source:  path,
		Name:    filepath.Base(path),
		Size:    info.Size(),
		ModTime: info.ModTime(),
		Status:  model.StatusQueued,
	})
}

// walk descends one directory without ever following directory symlinks.
// Entries are visited in os.ReadDir's sorted order, which makes the scan
// deterministic. Unreadable directories are recorded and skipped.
func (c *collector) walk(ctx context.Context, dir, out string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		c.skipDir(dir)
		return nil
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := filepath.Join(dir, entry.Name())

		if entry.Type()&fs.ModeSymlink != 0 {
			// A symlinked directory could leave the dropped tree or repeat
			// forever; record it so the user can see what was left out.
			if target, err := os.Stat(path); err == nil && target.IsDir() {
				c.skipDir(path)
			}
			continue
		}
		if entry.IsDir() {
			if out != "" && inside(path, out) {
				c.skipDir(path)
				continue
			}
			if err := c.walk(ctx, path, out); err != nil {
				return err
			}
			continue
		}
		if !model.IsPhotoName(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			c.skipDir(path)
			continue
		}
		c.addItem(path, info)
	}
	return nil
}

// nameOutputs assigns the JPEG base name of every item, adding " (n)" before
// the extension when sources share a base name. It runs on the final order
// so the suffixes are deterministic, and it also avoids colliding with a
// literal name such as "IMG_1234 (2).CR2" that is already queued.
func (c *collector) nameOutputs() {
	used := make(map[string]bool, len(c.items))
	for _, item := range c.items {
		stem := strings.TrimSuffix(item.Name, filepath.Ext(item.Name))
		name := stem + ".jpg"
		if used[name] {
			for n := 2; ; n++ {
				candidate := fmt.Sprintf("%s (%d).jpg", stem, n)
				if !used[candidate] {
					name = candidate
					break
				}
			}
		}
		used[name] = true
		item.OutputName = name
	}
}

// inside reports whether path is dir or lies below it. Both arguments must
// be cleaned absolute paths.
func inside(path, dir string) bool {
	if path == dir {
		return true
	}
	return strings.HasPrefix(path, dir+string(filepath.Separator))
}
