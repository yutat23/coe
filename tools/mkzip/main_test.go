package main

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteZipSetsModesAndIsReproducible(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "coe")
	script := filepath.Join(dir, "installer.ps1")
	if err := os.WriteFile(bin, []byte("binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("script"), 0o600); err != nil {
		t.Fatal(err)
	}

	mtime := time.Unix(1700000000, 0).UTC()
	first := filepath.Join(dir, "a.zip")
	second := filepath.Join(dir, "b.zip")
	if err := writeZip(first, []string{script, bin}, mtime, []string{"coe"}); err != nil {
		t.Fatal(err)
	}
	// Touch the inputs: file times on disk must not leak into the archive.
	later := time.Now().Add(time.Hour)
	os.Chtimes(bin, later, later)
	if err := writeZip(second, []string{bin, script}, mtime, []string{"coe"}); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(first)
	b, _ := os.ReadFile(second)
	if !bytes.Equal(a, b) {
		t.Fatalf("same inputs produced different archives")
	}

	zr, err := zip.OpenReader(first)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	modes := map[string]os.FileMode{}
	for _, f := range zr.File {
		modes[f.Name] = f.Mode().Perm()
		if !f.Modified.Equal(mtime) {
			t.Fatalf("%s modified = %s, want %s", f.Name, f.Modified, mtime)
		}
	}
	if modes["coe"] != 0o755 || modes["installer.ps1"] != 0o644 {
		t.Fatalf("modes = %v", modes)
	}
}
