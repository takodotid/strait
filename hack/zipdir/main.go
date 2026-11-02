// Command zipdir writes a .zip archive that holds one directory, as `tar -C parent -czf` does for a .tar.gz.
// `make dist` uses it for the Windows archive, so the build needs no zip tool.
//
//	go run ./hack/zipdir <archive.zip> <dir>
package main

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "zipdir:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: zipdir <archive.zip> <dir>")
	}
	archive, dir := args[0], filepath.Clean(args[1])
	if err := write(archive, dir); err != nil {
		_ = os.Remove(archive)
		return err
	}
	return nil
}

// write creates archive with dir and everything under it. Entry names start with dir's base name and use forward slashes.
func write(archive, dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	f, err := os.Create(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	parent := filepath.Dir(dir)
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
		h, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		h.Name = filepath.ToSlash(rel)
		switch {
		case d.IsDir():
			h.Name += "/"
			_, err := zw.CreateHeader(h)
			return err
		case !info.Mode().IsRegular():
			return fmt.Errorf("%s is not a regular file", path)
		}
		h.Method = zip.Deflate
		w, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(w, src)
		return err
	})
	if err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return f.Close()
}
