// Package fsutil provides small filesystem helpers used to write export
// results without ever overwriting an unrelated file.
package fsutil

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// CommitNew atomically makes temp visible at dst. It fails rather than
// replacing dst if dst exists. temp must be on the same filesystem as dst.
//
// The hard link is the commit point: once os.Link succeeds, dst is the
// committed output. Removing temp is best-effort cleanup, because a failure to
// unlink temp must not turn a successful commit into a reported failure; the
// caller is expected to remove temp on its own cleanup path.
func CommitNew(temp, dst string) error {
	if _, err := os.Lstat(dst); err == nil {
		return os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Linking is atomic and refuses to replace an existing name. Renaming
	// would silently replace a file that appeared after the check above.
	if err := os.Link(temp, dst); err != nil {
		return err
	}
	_ = os.Remove(temp)
	return nil
}

// UniquePath returns a path in dir based on base that does not exist. It
// appends " (2)", " (3)" and so on before the extension. The check is a
// planning aid; CommitNew remains the authority in case of a race.
func UniquePath(dir, base string) string {
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	candidate := filepath.Join(dir, base)
	for n := 2; ; n++ {
		if _, err := os.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate
		}
		candidate = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, n, ext))
	}
}

// CopyFile copies src to a new file dst. It refuses to overwrite dst. The
// caller is responsible for removing dst if a later step fails.
func CopyFile(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(out, in)
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil {
		os.Remove(dst)
		return n, copyErr
	}
	if syncErr != nil {
		os.Remove(dst)
		return n, syncErr
	}
	if closeErr != nil {
		os.Remove(dst)
		return n, closeErr
	}
	return n, nil
}

// SyncDir flushes a directory entry so a committed file survives a crash on
// filesystems that require it. Failure is advisory on platforms that do not
// permit directories to be opened for syncing.
func SyncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
