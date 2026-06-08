// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestLicenseHeaders checks that every Go and Python source file in the tree
// carries the Tailscale copyright + SPDX header (mirrors tailscale/tsidp's
// license_test.go, extended to cover Python). Adapted from that test.
func TestLicenseHeaders(t *testing.T) {
	headers := map[string][]byte{
		".go": []byte("// Copyright (c) Tailscale Inc & AUTHORS\n// SPDX-License-Identifier: BSD-3-Clause\n"),
		".py": []byte("# Copyright (c) Tailscale Inc & AUTHORS\n# SPDX-License-Identifier: BSD-3-Clause\n"),
	}

	err := filepath.Walk(".", func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.IsDir() {
			switch fi.Name() {
			case ".git", "build", ".venv-test", ".venv", "__pycache__":
				return filepath.SkipDir
			}
			return nil
		}
		want, ok := headers[filepath.Ext(path)]
		if !ok {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()

		buf := make([]byte, 512)
		n, err := io.ReadAtLeast(f, buf, 512)
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			return err
		}
		// Normalize CRLF and search the head of the file so a shebang line
		// (worker.py) ahead of the header is fine.
		head := bytes.ReplaceAll(buf[:n], []byte("\r\n"), []byte("\n"))
		if !bytes.Contains(head, want) {
			t.Errorf("file %s is missing the license header:\n%s", path, want)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}
