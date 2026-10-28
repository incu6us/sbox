package main

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPlanSync(t *testing.T) {
	t.Parallel()

	v1, v2, v3 := fileState{Hash: "1"}, fileState{Hash: "2"}, fileState{Hash: "3"}
	exe := fileState{Hash: "1", Exec: true}
	base := manifest{"same": v1, "guest-edit": v1, "host-edit": v1, "both-edit": v1, "both-same": v1,
		"guest-del": v1, "host-del": v1, "both-del": v1, "chmod": v1, "edit-vs-del": v1}
	host := manifest{"same": v1, "guest-edit": v1, "host-edit": v2, "both-edit": v2, "both-same": v2,
		"guest-del": v1, "chmod": v1, "edit-vs-del": v2, "host-new": v1, "both-new": v1}
	guest := manifest{"same": v1, "guest-edit": v2, "host-edit": v1, "both-edit": v3, "both-same": v2,
		"host-del": v1, "chmod": exe, "guest-new": v1, "both-new": v2}

	want := []syncAction{
		{Path: "both-del", Op: opSame},
		{Path: "both-edit", Op: opConflict},
		{Path: "both-new", Op: opConflict},
		{Path: "both-same", Op: opSame},
		{Path: "chmod", Op: opPull},
		{Path: "edit-vs-del", Op: opConflict},
		{Path: "guest-del", Op: opPullDelete},
		{Path: "guest-edit", Op: opPull},
		{Path: "guest-new", Op: opPull},
		{Path: "host-del", Op: opPushDelete},
		{Path: "host-edit", Op: opPush},
		{Path: "host-new", Op: opPush},
	}
	if got := planSync(base, host, guest); !slices.Equal(got, want) {
		t.Errorf("planSync =\n%v\nwant\n%v", got, want)
	}
}

func TestReadManifest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(rel, body string, mode os.FileMode) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("README.md", "hi\n", 0o644)
	write("bin/run.sh", "#!/bin/sh\n", 0o755)
	write(".git/HEAD", "ref\n", 0o644)
	write("node_modules/x/index.js", "x\n", 0o644)
	write("src/node_modules/y.js", "y\n", 0o644)
	write("app/__pycache__/m.pyc", "c\n", 0o644)
	write(".venv/bin/python", "p\n", 0o755)
	write(".DS_Store", "d\n", 0o644)
	if err := os.Symlink("README.md", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}

	m, err := readManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("hi\n"))
	want := manifest{
		"README.md":  {Hash: hex.EncodeToString(sum[:])},
		"bin/run.sh": {Hash: m["bin/run.sh"].Hash, Exec: true},
	}
	if !maps.Equal(m, want) || len(m["bin/run.sh"].Hash) != 64 {
		t.Errorf("readManifest = %v, want %v", m, want)
	}
}

func TestApplyPulls(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T) (host, pulled, outside string) {
		root := t.TempDir()
		host, pulled, outside = filepath.Join(root, "host"), filepath.Join(root, "pulled"), filepath.Join(root, "outside")
		for _, d := range []string{host, pulled, outside} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		return host, pulled, outside
	}
	put := func(t *testing.T, p, body string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("writes and deletes", func(t *testing.T) {
		t.Parallel()
		host, pulled, _ := setup(t)
		put(t, filepath.Join(pulled, "src/new/run.sh"), "#!/bin/sh\n")
		put(t, filepath.Join(host, "old.txt"), "old")
		plan := []syncAction{{Path: "src/new/run.sh", Op: opPull}, {Path: "old.txt", Op: opPullDelete}, {Path: "x", Op: opPush}}
		guest := manifest{"src/new/run.sh": {Hash: "h", Exec: true}}
		if err := applyPulls(host, pulled, plan, guest); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(host, "src/new/run.sh"))
		if err != nil || info.Mode().Perm() != 0o755 {
			t.Errorf("run.sh: %v, %v; want written with its executable bit", err, info)
		}
		if _, err := os.Stat(filepath.Join(host, "old.txt")); !os.IsNotExist(err) {
			t.Errorf("old.txt still there (stat: %v)", err)
		}
	})

	for _, tt := range []struct {
		name string
		link string // a symlink in the host folder, to outside
		path string // what the guest sends
	}{
		{name: "through a linked folder", link: "linked", path: "linked/evil"},
		{name: "onto a linked file", link: "cfg", path: "cfg"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			host, pulled, outside := setup(t)
			if err := os.Symlink(outside, filepath.Join(host, tt.link)); err != nil {
				t.Fatal(err)
			}
			put(t, filepath.Join(pulled, tt.path), "pwned")
			put(t, filepath.Join(host, "fine.txt"), "old")
			put(t, filepath.Join(pulled, "fine.txt"), "new")
			plan := []syncAction{{Path: "fine.txt", Op: opPull}, {Path: tt.path, Op: opPull}}
			err := applyPulls(host, pulled, plan, manifest{tt.path: {Hash: "h"}, "fine.txt": {Hash: "h"}})
			if err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Errorf("err = %v, want a refusal naming the symlink", err)
			}
			if left, _ := os.ReadDir(outside); len(left) != 0 {
				t.Errorf("wrote outside the project: %v", left)
			}
			if got, _ := os.ReadFile(filepath.Join(host, "fine.txt")); string(got) != "old" {
				t.Errorf("fine.txt = %q: something was written before the check", got)
			}
		})
	}
}

// fakeSyncGuest plays sbox-run for sync: a guest whose project tree is a
// map, reached through attach.
type fakeSyncGuest struct {
	t       *testing.T
	project string
	files   map[string]string // the guest's work tree
	running string            // a session already running for the project
	state   string            // its state, running when empty
	calls   [][]string
}

func (g *fakeSyncGuest) run(_ context.Context, in io.Reader, out, _ io.Writer, args ...string) error {
	g.calls = append(g.calls, args)
	switch args[0] {
	case "ps":
		if g.running != "" {
			state := cmp.Or(g.state, "running")
			_, _ = fmt.Fprintf(out, "%s %s 1700000000 %s\n", g.running, state, g.project)
		}
	case "start":
		_, _ = io.Copy(io.Discard, in)
		_, _ = fmt.Fprintf(out, "READY %s\n", args[1])
	case "attach":
		cmd := args[3:]
		switch {
		case slices.Equal(cmd, []string{"sh", "-c", _pullScript}):
			tw := tar.NewWriter(out)
			for _, p := range slices.Sorted(maps.Keys(g.files)) {
				_ = tw.WriteHeader(&tar.Header{Name: "./" + p, Mode: 0o644, Size: int64(len(g.files[p])), Typeflag: tar.TypeReg})
				_, _ = io.WriteString(tw, g.files[p])
			}
			_ = tw.Close()
		case slices.Equal(cmd, _pushCommand):
			tr := tar.NewReader(in)
			for {
				hdr, err := tr.Next()
				if err != nil {
					break
				}
				body, _ := io.ReadAll(tr)
				g.files[strings.TrimPrefix(hdr.Name, "./")] = string(body)
			}
		case slices.Equal(cmd, []string{"sh", "-c", _deleteScript}):
			list, _ := io.ReadAll(in)
			for _, p := range strings.Split(strings.TrimSuffix(string(list), "\x00"), "\x00") {
				delete(g.files, p)
			}
		default:
			g.t.Errorf("unexpected attach command %q", cmd)
		}
	}
	return nil
}

func TestSyncMain(t *testing.T) {
	t.Parallel()
	dir, state := t.TempDir(), t.TempDir()
	write := func(p, body string) {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(p string) string {
		data, err := os.ReadFile(filepath.Join(dir, p))
		if err != nil {
			return "<missing>"
		}
		return string(data)
	}
	// What was copied in, and recorded as the base.
	for _, p := range []string{"guest-edits", "guest-deletes", "host-edits", "both-edit"} {
		write(p, "v1\n")
	}
	project := mustProjectID(t, dir)
	if err := recordBase(state, project, dir); err != nil {
		t.Fatal(err)
	}
	// Then each side worked.
	write("host-edits", "host v2\n")
	write("host-adds", "new on the Mac\n")
	write("both-edit", "host v2\n")
	guest := &fakeSyncGuest{t: t, project: project, files: map[string]string{
		"guest-edits": "guest v2\n", "host-edits": "v1\n", "both-edit": "guest v2\n", "guest-adds": "new in the guest\n",
	}}

	var stdout, stderr bytes.Buffer
	if code := syncMain(context.Background(), []string{dir}, &stdout, &stderr, guest.run, state); code != exitOK {
		t.Fatalf("dry run: exit %d, %s", code, stderr.String())
	}
	for _, want := range []string{"guest-edits", "guest-deletes", "guest-adds", "host-edits", "host-adds", "conflict", "both-edit", "-apply"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("dry run output lacks %q:\n%s", want, stdout.String())
		}
	}
	if read("guest-edits") != "v1\n" || read("guest-adds") != "<missing>" || guest.files["host-adds"] != "" {
		t.Error("the dry run changed something")
	}
	if last := guest.calls[len(guest.calls)-1]; last[0] != "stop" {
		t.Errorf("the session sync started was not stopped: last call %q", last)
	}

	stdout.Reset()
	if code := syncMain(context.Background(), []string{"-apply", dir}, &stdout, &stderr, guest.run, state); code != exitOK {
		t.Fatalf("apply: exit %d, %s", code, stderr.String())
	}
	if read("guest-edits") != "guest v2\n" || read("guest-adds") != "new in the guest\n" || read("guest-deletes") != "<missing>" {
		t.Errorf("Mac side after apply: guest-edits %q, guest-adds %q, guest-deletes %q",
			read("guest-edits"), read("guest-adds"), read("guest-deletes"))
	}
	if guest.files["host-edits"] != "host v2\n" || guest.files["host-adds"] != "new on the Mac\n" {
		t.Errorf("guest side after apply: %v", guest.files)
	}
	if read("both-edit") != "host v2\n" || guest.files["both-edit"] != "guest v2\n" {
		t.Error("the conflict was touched")
	}

	stdout.Reset()
	if code := syncMain(context.Background(), []string{dir}, &stdout, &stderr, guest.run, state); code != exitOK {
		t.Fatalf("second dry run: exit %d, %s", code, stderr.String())
	}
	if out := stdout.String(); strings.Count(out, "\n") > 3 || !strings.Contains(out, "both-edit") {
		t.Errorf("after apply only the conflict should remain:\n%s", out)
	}

	guest.running = "abababababababab"
	guest.calls = nil
	if code := syncMain(context.Background(), []string{dir}, &stdout, &stderr, guest.run, state); code != exitOK ||
		slices.ContainsFunc(guest.calls, func(c []string) bool { return c[0] == "start" || c[0] == "stop" }) {
		t.Errorf("with a session running: exit %d, calls %q; want that session used, none started or stopped", code, guest.calls)
	}

	guest.state = "paused"
	guest.calls = nil
	if code := syncMain(context.Background(), []string{dir}, &stdout, &stderr, guest.run, state); code != exitOK ||
		!slices.Equal(guest.calls[1], []string{"resume", guest.running}) || !slices.Equal(guest.calls[len(guest.calls)-1], []string{"pause", guest.running}) {
		t.Errorf("with the session paused: exit %d, calls %q; want it resumed for the sync and paused again", code, guest.calls)
	}

	if code := syncMain(context.Background(), []string{dir}, &stdout, &stderr, guest.run, t.TempDir()); code != exitFail ||
		!strings.Contains(stderr.String(), "no sync base") {
		t.Errorf("without a base: exit %d, stderr %q; want a refusal", code, stderr.String())
	}
}
