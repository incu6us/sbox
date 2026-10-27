package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// tarEntry is one member of a test archive.
type tarEntry struct {
	name, body, link string
	mode             int64
	typ              byte
}

func tarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: e.mode, Typeflag: e.typ, Linkname: e.link, Size: int64(len(e.body))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// _limaLike mirrors the shape of Lima's release archive.
var _limaLike = []tarEntry{
	{name: "./", typ: tar.TypeDir, mode: 0o755},
	{name: "./bin/", typ: tar.TypeDir, mode: 0o755},
	{name: "./bin/limactl", typ: tar.TypeReg, mode: 0o755, body: "#!/bin/sh\necho limactl\n"},
	{name: "./share/lima/templates/", typ: tar.TypeDir, mode: 0o755},
	{name: "./share/lima/templates/ubuntu-24.04.yaml", typ: tar.TypeReg, mode: 0o644, body: "images: []\n"},
	{name: "./share/doc/lima/", typ: tar.TypeDir, mode: 0o755},
	{name: "./share/doc/lima/templates", typ: tar.TypeSymlink, link: "../../lima/templates"},
}

func TestExtractTarGz(t *testing.T) {
	t.Parallel()

	t.Run("lima layout", func(t *testing.T) {
		t.Parallel()
		dest := t.TempDir()
		if err := extractTarGz(bytes.NewReader(tarGz(t, _limaLike)), dest); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(dest, "bin", "limactl"))
		if err != nil || info.Mode().Perm()&0o100 == 0 {
			t.Errorf("bin/limactl: %v, mode %v; want an executable file", err, info)
		}
		yaml, err := os.ReadFile(filepath.Join(dest, "share", "doc", "lima", "templates", "ubuntu-24.04.yaml"))
		if err != nil || string(yaml) != "images: []\n" {
			t.Errorf("template through the internal symlink: %q, %v", yaml, err)
		}
	})

	escapes := []struct {
		name  string
		entry tarEntry
	}{
		{"parent path", tarEntry{name: "../evil", typ: tar.TypeReg, mode: 0o644, body: "x"}},
		{"nested parent path", tarEntry{name: "./bin/../../evil", typ: tar.TypeReg, mode: 0o644, body: "x"}},
		{"absolute path", tarEntry{name: "/tmp/evil", typ: tar.TypeReg, mode: 0o644, body: "x"}},
		{"symlink out", tarEntry{name: "./bin/out", typ: tar.TypeSymlink, link: "../../.."}},
		{"absolute symlink", tarEntry{name: "./bin/out", typ: tar.TypeSymlink, link: "/etc"}},
	}
	for _, tt := range escapes {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			dest := filepath.Join(root, "dest")
			err := extractTarGz(bytes.NewReader(tarGz(t, []tarEntry{tt.entry})), dest)
			if err == nil || !strings.Contains(err.Error(), "outside") {
				t.Errorf("err = %v, want a refusal naming the escape", err)
			}
			if _, err := os.Lstat(filepath.Join(root, "evil")); !os.IsNotExist(err) {
				t.Errorf("something was written outside the folder")
			}
		})
	}
}

func TestExtractTarGzRefusesWritingThroughALink(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dest := filepath.Join(root, "dest")
	// a/b/c points back at a; a/b/c/l would land at a/l, pointing two
	// levels up from a: outside dest, though each link looks inside.
	entries := []tarEntry{
		{name: "a/b/", typ: tar.TypeDir, mode: 0o755},
		{name: "a/b/c", typ: tar.TypeSymlink, link: "../.."},
		{name: "a/b/c/l", typ: tar.TypeSymlink, link: "../.."},
	}
	err := extractTarGz(bytes.NewReader(tarGz(t, entries)), dest)
	if err == nil || !strings.Contains(err.Error(), "through a link") {
		t.Errorf("err = %v, want a refusal to write through a link", err)
	}
}

func TestInstallLima(t *testing.T) {
	t.Parallel()
	archive := tarGz(t, _limaLike)
	sum := sha256.Sum256(archive)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/lima.tar.gz" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(archive)
	}))
	t.Cleanup(srv.Close)
	notOnPath := func(string) (string, error) { return "", exec.ErrNotFound }

	tests := []struct {
		name    string
		a       artifact
		wantErr string // "" for success
	}{
		{name: "installs", a: artifact{url: srv.URL + "/lima.tar.gz", sha256: hex.EncodeToString(sum[:])}},
		{name: "checksum mismatch", a: artifact{url: srv.URL + "/lima.tar.gz", sha256: strings.Repeat("0", 64)}, wantErr: "checksum"},
		{name: "not found", a: artifact{url: srv.URL + "/missing.tar.gz", sha256: hex.EncodeToString(sum[:])}, wantErr: "404"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			err := installLima(context.Background(), srv.Client(), tt.a, home)
			got := findLimactl(home, notOnPath)
			if tt.wantErr == "" {
				if err != nil || got != filepath.Join(home, _limaDir, "bin", "limactl") {
					t.Errorf("err %v, limactl %q; want installed under %s", err, got, _limaDir)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) || got != "" {
				t.Errorf("err %v, limactl %q; want an error with %q and nothing installed", err, got, tt.wantErr)
			}
			left, _ := os.ReadDir(filepath.Dir(filepath.Join(home, _limaDir)))
			if len(left) != 0 {
				t.Errorf("left behind: %v", left)
			}
		})
	}
}
