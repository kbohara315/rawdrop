// Package journal persists the one active RawDrop job as versioned JSON in
// the application data directory. Writes are atomic and a separate lock file
// lets only one app instance own a resumable job at a time.
//
// A Store is not safe for concurrent use. The caller serializes Load, Save
// and Clear (RawDrop's controller does so under its state mutex) so Save can
// marshal a consistent snapshot without an internal copy.
package journal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"rawdrop/internal/fsutil"
	"rawdrop/internal/model"
)

// Version is the journal schema version this package writes. Load rejects
// files written by newer versions instead of guessing at their meaning.
const Version = 1

const (
	jobsFile = "jobs.json"
	lockFile = "jobs.lock"
)

// Store owns one jobs file. Path is the absolute location of that file.
type Store struct {
	Path string

	// lock is the held lock file, nil while the store is unlocked.
	lock *os.File
}

// NewStore prepares stateDir for journaling and returns the store for
// stateDir/jobs.json. The directory is created when missing.
func NewStore(stateDir string) (*Store, error) {
	if strings.TrimSpace(stateDir) == "" {
		return nil, errors.New("journal: empty state directory")
	}
	dir, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, fmt.Errorf("journal: resolve state directory: %w", err)
	}
	dir = filepath.Clean(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("journal: create state directory: %w", err)
	}
	return &Store{Path: filepath.Join(dir, jobsFile)}, nil
}

// Load returns the stored job. A missing journal is not an error: it returns
// (nil, nil). A journal with a missing or invalid version, an unknown future
// version, malformed JSON, or an item without a source is rejected with a
// clear error. Missing item lists are normalized to an empty slice.
func (s *Store) Load(ctx context.Context) (*model.Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("journal: read %s: %w", s.Path, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var job model.Job
	if err := json.Unmarshal(data, &job); err != nil {
		return nil, fmt.Errorf("journal: parse %s: %w", s.Path, err)
	}
	switch {
	case job.Version < 1:
		return nil, fmt.Errorf("journal: %s has a missing or invalid version %d", s.Path, job.Version)
	case job.Version > Version:
		return nil, fmt.Errorf("journal: %s was written by a newer version (%d, this build supports %d)", s.Path, job.Version, Version)
	}
	if job.Items == nil {
		job.Items = []*model.Item{}
	}
	for i, item := range job.Items {
		if item == nil {
			return nil, fmt.Errorf("journal: %s item %d is null", s.Path, i)
		}
		if strings.TrimSpace(item.Source) == "" {
			return nil, fmt.Errorf("journal: %s item %d has an empty source", s.Path, i)
		}
	}
	return &job, nil
}

// Save atomically replaces the journal with job. It sets job.Updated to the
// current UTC time and returns after the new file is durable. The caller
// must guarantee that nothing else mutates job while Save runs.
func (s *Store) Save(ctx context.Context, job *model.Job) error {
	if job == nil {
		return errors.New("journal: nil job")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if job.Version == 0 {
		job.Version = Version
	}
	if job.Version > Version {
		return fmt.Errorf("journal: refusing to write version %d (this build supports %d)", job.Version, Version)
	}
	job.Updated = time.Now().UTC()

	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return fmt.Errorf("journal: encode job: %w", err)
	}
	data = append(data, '\n')
	if err := writeAtomic(s.Path, data, 0o644); err != nil {
		return fmt.Errorf("journal: save %s: %w", s.Path, err)
	}
	return nil
}

// Clear removes the journal. A missing journal is not an error.
func (s *Store) Clear(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Remove(s.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("journal: clear %s: %w", s.Path, err)
	}
	if err := fsutil.SyncDir(filepath.Dir(s.Path)); err != nil {
		return fmt.Errorf("journal: sync state directory: %w", err)
	}
	return nil
}

// writeAtomic writes data to path through a unique temporary file in the
// same directory: the file is synced, renamed over path, and the directory
// entry is synced so the new content survives a crash. The temporary file
// never replaces a concurrent writer's result after a failure.
func writeAtomic(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	tmpName = "" // Renamed; nothing to clean up.
	return fsutil.SyncDir(dir)
}
