// Command archive writes a CLI archive that holds one directory, as `tar -C parent -czf` does: a .zip when the archive's
// name ends in .zip, a .tar.gz otherwise. `make dist` uses it for every archive, so the build needs no tar or zip tool.
//
// The same files give the same bytes, whatever the build's time, time zone, umask and user (RFC0006 requirement 17):
//
//   - the entries are sorted by name;
//   - every entry has the time SOURCE_DATE_EPOCH;
//   - a directory or an executable file has the mode 0755, any other file 0644;
//   - a .tar.gz entry has the owner and group 0 with no names, and the gzip header holds no name and no time;
//   - a .zip entry stores its time in UTC.
//
// Usage:
//
//	SOURCE_DATE_EPOCH=<seconds> go run ./hack/archive <archive.tar.gz|archive.zip> <dir>
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

func main() {
	if err := run(os.Args[1:], os.Getenv("SOURCE_DATE_EPOCH")); err != nil {
		fmt.Fprintln(os.Stderr, "archive:", err)
		os.Exit(1)
	}
}

func run(args []string, epoch string) error {
	if len(args) != 2 {
		return errors.New("usage: SOURCE_DATE_EPOCH=<seconds> archive <archive.tar.gz|archive.zip> <dir>")
	}
	archive, dir := args[0], filepath.Clean(args[1])
	secs, err := strconv.ParseInt(epoch, 10, 64)
	if err != nil || secs < 0 {
		return fmt.Errorf("SOURCE_DATE_EPOCH %q is not a number of seconds", epoch)
	}
	if err := write(archive, dir, time.Unix(secs, 0).UTC()); err != nil {
		_ = os.Remove(archive)
		return err
	}
	return nil
}

// entry is a directory or a regular file of the archive.
type entry struct {
	// path is the file's path on disk.
	path string
	// name is the entry's name in the archive: forward slashes, and a trailing slash for a directory.
	name string
	// mode is 0755 or 0644.
	mode int64
	size int64
}

func (e entry) dir() bool { return strings.HasSuffix(e.name, "/") }

// write creates archive with dir and everything under it, each entry with the time mtime. Entry names start with dir's
// base name.
func write(archive, dir string, mtime time.Time) error {
	entries, err := list(dir)
	if err != nil {
		return err
	}
	f, err := os.Create(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	if strings.HasSuffix(archive, ".zip") {
		err = writeZip(f, entries, mtime)
	} else {
		err = writeTarGz(f, entries, mtime)
	}
	if err != nil {
		return err
	}
	return f.Close()
}

// list returns dir and everything under it, sorted by name. A directory's name ends with a slash, so it comes before the
// entries under it. Anything but a directory or a regular file fails.
func list(dir string) ([]entry, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	parent := filepath.Dir(dir)
	var entries []entry
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(parent, path)
		if err != nil {
			return err
		}
		e := entry{path: path, name: filepath.ToSlash(rel), mode: 0o644}
		switch {
		case info.IsDir():
			e.name += "/"
			e.mode = 0o755
		case !info.Mode().IsRegular():
			return fmt.Errorf("%s is not a regular file", path)
		default:
			e.size = info.Size()
			// The umask decides the other bits. Only whether the file runs is kept.
			if info.Mode().Perm()&0o111 != 0 {
				e.mode = 0o755
			}
		}
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(a.name, b.name) })
	return entries, nil
}

// writeTarGz writes the entries as a tar, compressed with gzip. The gzip header is left empty: no name, no time.
func writeTarGz(w io.Writer, entries []entry, mtime time.Time) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		// Uid, Gid, Uname and Gname stay empty: owner 0, with no names.
		h := &tar.Header{Name: e.name, Mode: e.mode, ModTime: mtime, Typeflag: tar.TypeReg, Size: e.size, Format: tar.FormatUSTAR}
		if e.dir() {
			h.Typeflag = tar.TypeDir
		}
		if err := tw.WriteHeader(h); err != nil {
			return fmt.Errorf("%s: %w", e.name, err)
		}
		if !e.dir() {
			if err := copyFile(tw, e); err != nil {
				return err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// writeZip writes the entries as a zip, files deflated. Each entry's time is in UTC, in its MS-DOS fields too.
func writeZip(w io.Writer, entries []entry, mtime time.Time) error {
	zw := zip.NewWriter(w)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Modified: mtime, Method: zip.Deflate}
		mode := fs.FileMode(e.mode)
		if e.dir() {
			h.Method = zip.Store
			mode |= fs.ModeDir
		}
		h.SetMode(mode)
		fw, err := zw.CreateHeader(h)
		if err != nil {
			return fmt.Errorf("%s: %w", e.name, err)
		}
		if !e.dir() {
			if err := copyFile(fw, e); err != nil {
				return err
			}
		}
	}
	return zw.Close()
}

// copyFile copies e's file into w. It fails when the file's size changed since list read it.
func copyFile(w io.Writer, e entry) error {
	src, err := os.Open(e.path)
	if err != nil {
		return err
	}
	defer src.Close()
	n, err := io.Copy(w, src)
	if err != nil {
		return fmt.Errorf("%s: %w", e.path, err)
	}
	if n != e.size {
		return fmt.Errorf("%s: %d bytes, want %d: it changed while it was archived", e.path, n, e.size)
	}
	return nil
}
