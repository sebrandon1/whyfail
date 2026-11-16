package claudecfg

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// InstallAssets copies every regular file under src in fsys to dest, keeping
// the relative layout. It returns the paths it actually wrote; a file whose
// content already matches is left alone, and one that differs is treated as
// ours to update (that is what makes re-running init a no-op or an upgrade).
func InstallAssets(fsys fs.FS, src, dest string) ([]string, error) {
	var written []string
	err := fs.WalkDir(fsys, src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		out := filepath.Join(dest, rel)
		if cur, err := os.ReadFile(out); err == nil && bytes.Equal(cur, data) {
			return nil
		}
		if err := WriteAtomic(out, data); err != nil {
			return err
		}
		written = append(written, out)
		return nil
	})
	if err != nil {
		return written, err
	}
	return written, nil
}

// RemoveAssets deletes the files InstallAssets would write, but only while
// they still match what ships in the binary: a locally edited command is the
// user's, not ours to delete. Directories left empty are removed too.
func RemoveAssets(fsys fs.FS, src, dest string) ([]string, error) {
	var removed []string
	err := fs.WalkDir(fsys, src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		out := filepath.Join(dest, rel)
		cur, err := os.ReadFile(out)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !bytes.Equal(cur, data) {
			return nil
		}
		if err := os.Remove(out); err != nil {
			return err
		}
		removed = append(removed, out)
		return nil
	})
	if err != nil {
		return removed, err
	}
	// Remove the directories the files left behind, stopping at the first one
	// that still has something in it. os.Remove refuses a non-empty directory,
	// so this cannot take anything the user put there.
	for _, p := range removed {
		for dir := filepath.Dir(p); len(dir) > len(dest); dir = filepath.Dir(dir) {
			if err := os.Remove(dir); err != nil {
				break
			}
		}
	}
	return removed, nil
}
