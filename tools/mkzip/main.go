// Command mkzip writes a release archive for build.ps1.
//
// Compress-Archive records no Unix permissions, so the coe binary in the
// Linux and macOS zips extracted without its execute bit. This tool stores
// each entry with an explicit mode, and with a fixed timestamp and order so
// that the same inputs always produce the same archive (and SHA-256).
//
//	go run ./tools/mkzip -o out.zip -mtime 1700000000 -exec coe build/coe_linux_amd64/coe
package main

import (
	"archive/zip"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

func main() {
	out := flag.String("o", "", "archive to write")
	mtime := flag.Int64("mtime", 315532800, "modification time for every entry, in Unix seconds")
	execNames := flag.String("exec", "", "comma-separated entry names stored as executable (0755)")
	flag.Parse()
	if *out == "" || flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: mkzip -o out.zip [-mtime unix] [-exec name,...] file...")
		os.Exit(2)
	}
	if err := writeZip(*out, flag.Args(), time.Unix(*mtime, 0).UTC(), strings.Split(*execNames, ",")); err != nil {
		fmt.Fprintln(os.Stderr, "mkzip:", err)
		os.Exit(1)
	}
}

func writeZip(path string, files []string, mtime time.Time, execNames []string) error {
	files = slices.Clone(files)
	slices.SortFunc(files, func(a, b string) int { return strings.Compare(filepath.Base(a), filepath.Base(b)) })

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(f)
	for _, name := range files {
		if err := addFile(zw, name, mtime, slices.Contains(execNames, filepath.Base(name))); err != nil {
			zw.Close()
			f.Close()
			return err
		}
	}
	if err := zw.Close(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func addFile(zw *zip.Writer, name string, mtime time.Time, executable bool) error {
	src, err := os.Open(name)
	if err != nil {
		return err
	}
	defer src.Close()

	hdr := &zip.FileHeader{
		Name:     filepath.Base(name),
		Method:   zip.Deflate,
		Modified: mtime,
	}
	mode := os.FileMode(0o644)
	if executable {
		mode = 0o755
	}
	hdr.SetMode(mode) // also marks the entry as made on Unix so unzip applies the mode
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, src)
	return err
}
