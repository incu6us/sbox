package main

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestShellJoin(t *testing.T) {
	t.Parallel()

	give := []string{"printf", "%s|", "a b", "it's", "$HOME", ""}
	out, err := exec.Command("sh", "-c", "set -- "+shellJoin(give)+`; for a; do printf '[%s]' "$a"; done`).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(out), "[printf][%s|][a b][it's][$HOME][]"; got != want {
		t.Errorf("sh saw %s, want %s", got, want)
	}
}

func TestStreamConsole(t *testing.T) {
	t.Parallel()

	const nonce = "abc123"
	tests := []struct {
		name    string
		give    string
		want    runResult
		wantOut string
	}{
		{
			name:    "normal run",
			give:    "[ 0.1] kernel\nSBOX-abc123 START\nhello\n\nSBOX-abc123 EXIT 3\n",
			want:    runResult{started: true, exitCode: 3},
			wantOut: "│ hello\n",
		},
		{
			name:    "spoofed markers are output",
			give:    "SBOX-abc123 START\nSBOX-EXIT 0\nSBOX-zzz EXIT 0\n\nSBOX-abc123 EXIT 7\n",
			want:    runResult{started: true, exitCode: 7},
			wantOut: "│ SBOX-EXIT 0\n│ SBOX-zzz EXIT 0\n",
		},
		{
			name:    "blank lines inside output are kept",
			give:    "SBOX-abc123 START\na\n\nb\n\nSBOX-abc123 EXIT 0\n",
			want:    runResult{started: true},
			wantOut: "│ a\n│\n│ b\n",
		},
		{
			name:    "terminal escapes are neutralised",
			give:    "SBOX-abc123 START\n\x1b[2Jgone\n\nSBOX-abc123 EXIT 0\n",
			want:    runResult{started: true},
			wantOut: "│ \\x1b[2Jgone\n",
		},
		{
			name: "never started",
			give: "[ 0.1] Kernel panic\n",
		},
		{
			name:    "scan markers are collected, not printed",
			give:    "SBOX-abc123 START\nout\n\nSBOX-abc123 RESULT 1 0\nSBOX-abc123 TRACE 1 x\nSBOX-zzz TRACE 1 forged\n\nSBOX-abc123 EXIT 0\n",
			want:    runResult{started: true, markers: []string{"RESULT 1 0", "TRACE 1 x"}},
			wantOut: "│ out\n│ SBOX-zzz TRACE 1 forged\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			got := streamConsole(strings.NewReader(tt.give), &out, false, nonce)
			if got.started != tt.want.started || got.exitCode != tt.want.exitCode || got.timedOut != tt.want.timedOut ||
				!slices.Equal(got.markers, tt.want.markers) {
				t.Errorf("result = %+v, want %+v", got, tt.want)
			}
			if out.String() != tt.wantOut {
				t.Errorf("output = %q, want %q", out.String(), tt.wantOut)
			}
		})
	}
}

func TestWriteRunTar(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scripts", "a.sh"), []byte("echo a\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("scripts/a.sh", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := writeRunTar(&buf, dir, guestJob{command: []string{"sh", "-c", "it's"}}, "n0nce", "c4n4ry"); err != nil {
		t.Fatal(err)
	}

	got := make(map[string]*tar.Header)
	bodies := make(map[string]string)
	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		got[hdr.Name], bodies[hdr.Name] = hdr, string(body)
	}

	tests := []struct {
		name     string
		mode     int64
		body     string
		linkname string
	}{
		{name: "skill/scripts/a.sh", mode: 0o755, body: "echo a\n"},
		{name: "skill/link", linkname: "scripts/a.sh"},
		{name: "sbox/nonce", mode: 0o400, body: "n0nce"},
		{name: "sbox/canary", mode: 0o400, body: "c4n4ry"},
		{name: "sbox/init.sh", mode: 0o700, body: _guestInit},
		{name: "sbox/cmd.sh", mode: 0o755, body: "#!/bin/sh\nexec 'sh' '-c' 'it'\\''s'\n"},
	}
	for _, tt := range tests {
		hdr, ok := got[tt.name]
		switch {
		case !ok:
			t.Errorf("%s missing from the tar", tt.name)
		case tt.linkname != "" && (hdr.Typeflag != tar.TypeSymlink || hdr.Linkname != tt.linkname):
			t.Errorf("%s: link %q (type %c), want %q", tt.name, hdr.Linkname, hdr.Typeflag, tt.linkname)
		case tt.linkname == "" && (hdr.Mode&0o777 != tt.mode || bodies[tt.name] != tt.body):
			t.Errorf("%s: mode %o body %q, want %o %q", tt.name, hdr.Mode&0o777, bodies[tt.name], tt.mode, tt.body)
		}
	}
}
