package journal

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rawdrop/internal/model"
)

func testJob() *model.Job {
	job := model.NewJob([]string{"/photos/a.CR2"}, model.DefaultSettings("/photos/out"))
	job.Items = []*model.Item{
		{ID: "id-a", Source: "/photos/a.CR2", Name: "a.CR2", OutputName: "a.jpg", Status: model.StatusQueued},
		{ID: "id-b", Source: "/photos/b.CR2", Name: "b.CR2", OutputName: "b.jpg", Status: model.StatusDone, OutputPath: "/photos/out/b.jpg"},
	}
	return job
}

func newStore(t *testing.T, dir string) *Store {
	t.Helper()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := newStore(t, dir)
	if store.Path != filepath.Join(dir, jobsFile) {
		t.Fatalf("Path = %q, want %q", store.Path, filepath.Join(dir, jobsFile))
	}

	job := testJob()
	before := time.Now().UTC().Add(-time.Millisecond)
	if err := store.Save(context.Background(), job); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !job.Updated.After(before) {
		t.Fatalf("Updated = %v, want the time of the save", job.Updated)
	}
	if job.Updated.Before(job.Created) {
		t.Fatalf("Updated = %v is before Created = %v", job.Updated, job.Created)
	}

	loaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded == nil {
		t.Fatal("Load returned nil after Save")
	}
	if loaded.Version != Version {
		t.Fatalf("Version = %d, want %d", loaded.Version, Version)
	}
	if loaded.ID != job.ID || loaded.Settings.OutputDir != "/photos/out" {
		t.Fatalf("loaded job does not match: %+v", loaded)
	}
	if len(loaded.Items) != 2 || loaded.Items[0].Source != "/photos/a.CR2" || loaded.Items[1].OutputPath != "/photos/out/b.jpg" {
		t.Fatalf("loaded items do not match: %+v", loaded.Items)
	}
	if loaded.Items[1].Status != model.StatusDone {
		t.Fatalf("status = %q, want done", loaded.Items[1].Status)
	}

	st, err := os.Stat(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o644 {
		t.Fatalf("file mode = %v, want 0644", perm)
	}
	data, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data) {
		t.Fatal("journal is not valid JSON")
	}
	var probe struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil || probe.Version != Version {
		t.Fatalf("journal version probe: %+v, err %v", probe, err)
	}
}

func TestLoadMissingJournal(t *testing.T) {
	store := newStore(t, t.TempDir())
	job, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if job != nil {
		t.Fatalf("Load = %+v, want nil", job)
	}
}

func TestClear(t *testing.T) {
	dir := t.TempDir()
	store := newStore(t, dir)
	if err := store.Save(context.Background(), testJob()); err != nil {
		t.Fatal(err)
	}
	if err := store.Clear(context.Background()); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if _, err := os.Stat(store.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal still exists: %v", err)
	}
	job, err := store.Load(context.Background())
	if err != nil || job != nil {
		t.Fatalf("Load after Clear = %+v, %v", job, err)
	}
	// Clearing again is harmless.
	if err := store.Clear(context.Background()); err != nil {
		t.Fatalf("second Clear: %v", err)
	}
}

func TestSaveOverwritesAtomically(t *testing.T) {
	dir := t.TempDir()
	store := newStore(t, dir)
	first := testJob()
	if err := store.Save(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	second := testJob()
	second.ID = "replacement"
	second.Items = second.Items[:1]
	if err := store.Save(context.Background(), second); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != "replacement" || len(loaded.Items) != 1 {
		t.Fatalf("Load = %+v, want the second job", loaded)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != jobsFile {
			t.Fatalf("unexpected leftover file %q", entry.Name())
		}
	}
}

func TestLoadRejectsBadJournal(t *testing.T) {
	cases := []struct {
		name    string
		data    string
		wantSub string
	}{
		{"future version", `{"version": 99, "items": []}`, "newer version"},
		{"missing version", `{"items": []}`, "version"},
		{"malformed json", `{not json`, "parse"},
		{"item without source", `{"version": 1, "items": [{"id": "x"}]}`, "empty source"},
		{"null item", `{"version": 1, "items": [null]}`, "null"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t, t.TempDir())
			if err := os.WriteFile(store.Path, []byte(tc.data), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := store.Load(context.Background())
			if err == nil {
				t.Fatal("Load succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not mention %q", err, tc.wantSub)
			}
		})
	}
}

func TestLoadNormalizesNilItems(t *testing.T) {
	store := newStore(t, t.TempDir())
	data := `{"version": 1, "inputs": ["/photos/a.CR2"], "items": null}`
	if err := os.WriteFile(store.Path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	job, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if job.Items == nil || len(job.Items) != 0 {
		t.Fatalf("Items = %#v, want an empty non-nil slice", job.Items)
	}
}

func TestSaveRejectsNilAndFuture(t *testing.T) {
	store := newStore(t, t.TempDir())
	if err := store.Save(context.Background(), nil); err == nil {
		t.Fatal("Save(nil) must fail")
	}
	job := testJob()
	job.Version = Version + 1
	if err := store.Save(context.Background(), job); err == nil {
		t.Fatal("Save of a future version must fail")
	}
}

func TestLoadRespectsCancellation(t *testing.T) {
	store := newStore(t, t.TempDir())
	if err := store.Save(context.Background(), testJob()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Load(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Load with canceled context = %v, want context.Canceled", err)
	}
	if err := store.Save(ctx, testJob()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Save with canceled context = %v, want context.Canceled", err)
	}
	if err := store.Clear(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Clear with canceled context = %v, want context.Canceled", err)
	}
}

func TestLockContention(t *testing.T) {
	dir := t.TempDir()
	first := newStore(t, dir)
	second := newStore(t, dir)

	if err := first.Lock(); err != nil {
		t.Fatalf("first Lock: %v", err)
	}
	if err := second.Lock(); err == nil {
		t.Fatal("second Lock succeeded while the first store holds the lock")
	}
	if err := first.Lock(); err != nil {
		t.Fatalf("repeated Lock on the owner: %v", err)
	}
	// The lock guards ownership, not access: reads and writes still work.
	if err := first.Save(context.Background(), testJob()); err != nil {
		t.Fatalf("Save while locked: %v", err)
	}
	if _, err := second.Load(context.Background()); err != nil {
		t.Fatalf("Load while another store holds the lock: %v", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("second Close must be a no-op: %v", err)
	}
	if err := second.Lock(); err != nil {
		t.Fatalf("Lock after release: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	third := newStore(t, dir)
	if err := third.Lock(); err != nil {
		t.Fatalf("third Lock: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatalf("third Close: %v", err)
	}
}

func TestNewStoreCreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "state")
	store := newStore(t, dir)
	st, err := os.Stat(filepath.Dir(store.Path))
	if err != nil {
		t.Fatalf("state directory was not created: %v", err)
	}
	if !st.IsDir() {
		t.Fatalf("%s is not a directory", filepath.Dir(store.Path))
	}
	if _, err := NewStore(""); err == nil {
		t.Fatal("NewStore(\"\") must fail")
	}
}
