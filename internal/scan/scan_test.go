package scan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"rawdrop/internal/model"
)

func writeFile(t *testing.T, path, data string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func sources(items []*model.Item) []string {
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = item.Source
	}
	return out
}

func outputNames(items []*model.Item) []string {
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = item.OutputName
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func TestCollectNestedDuplicateNames(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(root, "JPEG - Full Resolution")
	want := []string{
		writeFile(t, filepath.Join(root, "IMG_0001.CR2"), "a"),
		writeFile(t, filepath.Join(root, "sub", "IMG_0001 (2).CR2"), "b"),
		writeFile(t, filepath.Join(root, "sub", "IMG_0001.CR2"), "c"),
		writeFile(t, filepath.Join(root, "sub", "nested", "PHOTO.NEF"), "d"),
	}
	writeFile(t, filepath.Join(root, "notes.txt"), "ignored inside folders")
	writeFile(t, filepath.Join(out, "IMG_9999.CR2"), "excluded output")
	writeFile(t, filepath.Join(out, "export.jpg"), "excluded output")

	res, err := Collect(context.Background(), []string{root}, out)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := sources(res.Items); !equal(got, want) {
		t.Fatalf("sources = %v, want %v", got, want)
	}
	wantNames := []string{"IMG_0001.jpg", "IMG_0001 (2).jpg", "IMG_0001 (3).jpg", "PHOTO.jpg"}
	if got := outputNames(res.Items); !equal(got, wantNames) {
		t.Fatalf("output names = %v, want %v", got, wantNames)
	}
	if len(res.Unsupported) != 0 {
		t.Fatalf("Unsupported = %v, want none for files inside folders", res.Unsupported)
	}
	if !contains(res.SkippedDirs, out) {
		t.Fatalf("SkippedDirs = %v, want the output folder", res.SkippedDirs)
	}
	for _, item := range res.Items {
		if item.Status != model.StatusQueued {
			t.Fatalf("item %s status = %q, want queued", item.Name, item.Status)
		}
		if item.Size == 0 {
			t.Fatalf("item %s has no size", item.Name)
		}
		if item.ModTime.IsZero() {
			t.Fatalf("item %s has no mod time", item.Name)
		}
		if item.Name != filepath.Base(item.Source) {
			t.Fatalf("item name %q does not match source %q", item.Name, item.Source)
		}
	}
	sum := sha256.Sum256([]byte(want[0]))
	if res.Items[0].ID != hex.EncodeToString(sum[:]) {
		t.Fatalf("ID = %q, want the SHA-256 of the cleaned path", res.Items[0].ID)
	}
}

func TestCollectExplicitFilesDeduplicates(t *testing.T) {
	root := t.TempDir()
	raw := writeFile(t, filepath.Join(root, "one.CR2"), "raw")
	cleaned := writeFile(t, filepath.Join(root, "sub", "..", "one.CR2"), "raw")
	upper := writeFile(t, filepath.Join(root, "TWO.Cr3"), "raw")
	video := writeFile(t, filepath.Join(root, "clip.MP4"), "video")

	res, err := Collect(context.Background(), []string{raw, video, raw, cleaned, upper, video}, filepath.Join(root, "out"))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := []string{filepath.Clean(raw), filepath.Clean(upper)}
	if got := sources(res.Items); !equal(got, want) {
		t.Fatalf("sources = %v, want %v", got, want)
	}
	if got := outputNames(res.Items); !equal(got, []string{"one.jpg", "TWO.jpg"}) {
		t.Fatalf("output names = %v", got)
	}
	if len(res.Unsupported) != 1 || res.Unsupported[0] != filepath.Clean(video) {
		t.Fatalf("Unsupported = %v, want just %s", res.Unsupported, video)
	}
}

func TestCollectExcludesOutputDirAndNothingElse(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(root, "exports") // Does not exist yet.
	photo := writeFile(t, filepath.Join(root, "a.CR2"), "a")
	// "outside" shares the "exports" prefix and must not be excluded.
	sibling := writeFile(t, filepath.Join(root, "outside", "b.CR2"), "b")
	writeFile(t, filepath.Join(out, "nested", "c.CR2"), "c")

	res, err := Collect(context.Background(), []string{root, out}, out)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := []string{photo, sibling}
	if got := sources(res.Items); !equal(got, want) {
		t.Fatalf("sources = %v, want %v", got, want)
	}
	if len(res.SkippedDirs) != 1 || res.SkippedDirs[0] != out {
		t.Fatalf("SkippedDirs = %v, want just %s", res.SkippedDirs, out)
	}
	if len(res.Unsupported) != 0 {
		t.Fatalf("Unsupported = %v", res.Unsupported)
	}
}

func TestCollectUnreadableDirIsRecorded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions behave differently on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := t.TempDir()
	good := writeFile(t, filepath.Join(root, "good.CR2"), "g")
	locked := filepath.Join(root, "locked")
	writeFile(t, filepath.Join(locked, "hidden.CR2"), "h")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	res, err := Collect(context.Background(), []string{root}, "")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := sources(res.Items); !equal(got, []string{good}) {
		t.Fatalf("sources = %v, want just the readable photo", got)
	}
	if !contains(res.SkippedDirs, locked) {
		t.Fatalf("SkippedDirs = %v, want the unreadable folder", res.SkippedDirs)
	}
}

func TestCollectUnreadableFileStaysQueued(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("file permissions behave differently here")
	}
	root := t.TempDir()
	locked := writeFile(t, filepath.Join(root, "locked.CR2"), "x")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })

	// Scanning never opens photo contents, so an unreadable RAW file is
	// still planned; the exporter reports the read failure later.
	res, err := Collect(context.Background(), []string{root}, "")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := sources(res.Items); !equal(got, []string{locked}) {
		t.Fatalf("sources = %v, want the unreadable file", got)
	}
}

func TestCollectDoesNotFollowDirSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require privileges on Windows")
	}
	root := t.TempDir()
	photo := writeFile(t, filepath.Join(root, "real", "photo.CR2"), "p")
	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Join(root, "real"), link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	res, err := Collect(context.Background(), []string{root}, "")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := sources(res.Items); !equal(got, []string{photo}) {
		t.Fatalf("sources = %v, want the real photo only", got)
	}
	if !contains(res.SkippedDirs, link) {
		t.Fatalf("SkippedDirs = %v, want the directory symlink", res.SkippedDirs)
	}
}

func TestCollectExplicitSymlinkFileIsAccepted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require privileges on Windows")
	}
	root := t.TempDir()
	target := writeFile(t, filepath.Join(root, "target", "photo.CR2"), "p")
	link := filepath.Join(root, "link.CR2")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	res, err := Collect(context.Background(), []string{link}, "")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := sources(res.Items); !equal(got, []string{link}) {
		t.Fatalf("sources = %v, want the dropped symlink path", got)
	}
}

func TestCollectUnusableInputs(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	res, err := Collect(ctx, []string{filepath.Join(tmp, "missing"), ""}, "")
	if err == nil {
		t.Fatal("Collect with only unusable inputs must fail")
	}
	if len(res.Items) != 0 {
		t.Fatalf("items = %v, want none", sources(res.Items))
	}
	if !contains(res.SkippedDirs, filepath.Join(tmp, "missing")) {
		t.Fatalf("SkippedDirs = %v, want the missing input", res.SkippedDirs)
	}

	// One usable input is enough; the other is only recorded.
	photo := writeFile(t, filepath.Join(tmp, "photo.CR2"), "p")
	res, err = Collect(ctx, []string{filepath.Join(tmp, "missing"), photo}, "")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := sources(res.Items); !equal(got, []string{photo}) {
		t.Fatalf("sources = %v", got)
	}
}

func TestCollectRejectsEmptyInputs(t *testing.T) {
	if _, err := Collect(context.Background(), nil, ""); err == nil {
		t.Fatal("Collect(nil) must fail")
	}
}

func TestCollectRespectsCancellation(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.CR2"), "a")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Collect(ctx, []string{root}, "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestCollectIsDeterministic(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "b", "two.CR2"), "2")
	writeFile(t, filepath.Join(root, "a", "one.CR2"), "1")
	writeFile(t, filepath.Join(root, "a", "one.CR2"), "1")
	writeFile(t, filepath.Join(root, "dup.CR2"), "d")
	writeFile(t, filepath.Join(root, "b", "dup.CR2"), "d")

	first, err := Collect(context.Background(), []string{root}, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Collect(context.Background(), []string{root}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !equal(sources(first.Items), sources(second.Items)) {
		t.Fatalf("order changed: %v then %v", sources(first.Items), sources(second.Items))
	}
	for i := range first.Items {
		if first.Items[i].ID != second.Items[i].ID {
			t.Fatalf("ID changed for %s", first.Items[i].Source)
		}
		if first.Items[i].OutputName != second.Items[i].OutputName {
			t.Fatalf("output name changed for %s", first.Items[i].Source)
		}
	}
}
