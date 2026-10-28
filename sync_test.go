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
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
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

	m, err := readManifest(dir, syncRules(nil))
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
	// newDrive makes start report a new work drive; started is the run
	// tar start was sent.
	newDrive bool
	started  []byte
	rules    ignoreRules // what the pull leaves out; syncRules(nil) when nil
}

func (g *fakeSyncGuest) pullRules() ignoreRules {
	if g.rules == nil {
		return syncRules(nil)
	}
	return g.rules
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
		g.started, _ = io.ReadAll(in)
		if g.newDrive {
			_, _ = fmt.Fprintf(out, "NEW %s\n", args[1])
		}
		_, _ = fmt.Fprintf(out, "READY %s\n", args[1])
	case "attach":
		cmd := args[3:]
		switch {
		case slices.Equal(cmd, []string{"sh", "-c", pullScript(g.pullRules())}):
			tw := tar.NewWriter(out)
			for _, p := range slices.Sorted(maps.Keys(g.files)) {
				if g.pullRules().skip(p) || g.pullRules().skip(path.Dir(p)) {
					continue
				}
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
		case len(cmd) == 3 && strings.HasPrefix(cmd[2], "cd /home/runner && rm -rf -- "): // copyMounts
			rels, _, _ := strings.Cut(strings.TrimPrefix(cmd[2], "cd /home/runner && rm -rf -- "), " && ")
			for _, rel := range strings.Fields(rels) {
				rel = strings.TrimPrefix(strings.Trim(rel, "'"), "work/")
				for p := range g.files {
					if p == rel || strings.HasPrefix(p, rel+"/") {
						delete(g.files, p)
					}
				}
			}
			tr := tar.NewReader(in)
			for {
				hdr, err := tr.Next()
				if err != nil {
					break
				}
				if hdr.Typeflag == tar.TypeReg {
					body, _ := io.ReadAll(tr)
					g.files[strings.TrimPrefix(hdr.Name, "work/")] = string(body)
				}
			}
		case slices.Contains(cmd, _denialScript): // sbox dev's summary: no transcripts here
		case slices.Equal(cmd, []string{"sh", "-c", sumScript(g.pullRules())}):
			for _, p := range slices.Sorted(maps.Keys(g.files)) {
				if !g.pullRules().skip(p) && !g.pullRules().skip(path.Dir(p)) {
					sum := sha256.Sum256([]byte(g.files[p]))
					_, _ = fmt.Fprintf(out, "%x  ./%s\n", sum, p)
				}
			}
		case slices.Equal(cmd, []string{"ss", "-Hltn"}), slices.Equal(cmd, []string{"true"}): // sbox dev's exit summary, its terminal
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
	if err := recordBase(state, project, dir, syncRules(nil)); err != nil {
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
	if code := syncMain(context.Background(), []string{dir}, &stdout, &stderr, guest.run, devHost{stateDir: state}); code != exitOK {
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
	if code := syncMain(context.Background(), []string{"-apply", dir}, &stdout, &stderr, guest.run, devHost{stateDir: state}); code != exitOK {
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
	if code := syncMain(context.Background(), []string{dir}, &stdout, &stderr, guest.run, devHost{stateDir: state}); code != exitOK {
		t.Fatalf("second dry run: exit %d, %s", code, stderr.String())
	}
	if out := stdout.String(); strings.Count(out, "\n") > 3 || !strings.Contains(out, "both-edit") {
		t.Errorf("after apply only the conflict should remain:\n%s", out)
	}

	guest.running = "abababababababab"
	guest.calls = nil
	if code := syncMain(context.Background(), []string{dir}, &stdout, &stderr, guest.run, devHost{stateDir: state}); code != exitOK ||
		slices.ContainsFunc(guest.calls, func(c []string) bool { return c[0] == "start" || c[0] == "stop" }) {
		t.Errorf("with a session running: exit %d, calls %q; want that session used, none started or stopped", code, guest.calls)
	}

	guest.state = "paused"
	guest.calls = nil
	if code := syncMain(context.Background(), []string{dir}, &stdout, &stderr, guest.run, devHost{stateDir: state}); code != exitOK ||
		!slices.Equal(guest.calls[1], []string{"resume", guest.running}) || !slices.Equal(guest.calls[len(guest.calls)-1], []string{"pause", guest.running}) {
		t.Errorf("with the session paused: exit %d, calls %q; want it resumed for the sync and paused again", code, guest.calls)
	}

	if code := syncMain(context.Background(), []string{dir}, &stdout, &stderr, guest.run, devHost{stateDir: t.TempDir()}); code != exitFail ||
		!strings.Contains(stderr.String(), "no sync base") {
		t.Errorf("without a base: exit %d, stderr %q; want a refusal", code, stderr.String())
	}
}

func TestDevMainPushesMacEditsOnBoot(t *testing.T) {
	t.Parallel()
	dir, state := t.TempDir(), t.TempDir()
	write := func(p, body string) {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"mac-edits", "guest-edits", "mac-deletes"} {
		write(p, "v1\n")
	}
	project := mustProjectID(t, dir)
	if err := recordBase(state, project, dir, syncRules(nil)); err != nil {
		t.Fatal(err)
	}
	write("mac-edits", "mac v2\n")
	write("mac-adds", "new\n")
	if err := os.Remove(filepath.Join(dir, "mac-deletes")); err != nil {
		t.Fatal(err)
	}
	guest := &fakeSyncGuest{t: t, project: project, files: map[string]string{"mac-edits": "v1\n", "guest-edits": "guest v2\n", "mac-deletes": "v1\n"}}

	var stdout, stderr bytes.Buffer
	if code := devMain(context.Background(), []string{dir, "--", "true"}, strings.NewReader(""), &stdout, &stderr, guest.run, devHost{stateDir: state}); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if guest.files["mac-edits"] != "mac v2\n" || guest.files["mac-adds"] != "new\n" || guest.files["mac-deletes"] != "" || guest.files["guest-edits"] != "guest v2\n" {
		t.Errorf("guest after boot: %v; want the Mac's changes in, the guest's own kept", guest.files)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "guest-edits")); string(data) != "v1\n" {
		t.Errorf("the Mac's guest-edits = %q: nothing comes back at boot", data)
	}
	for _, want := range []string{"3 changes from the Mac copied in", "the guest has 1 change of its own: sbox sync " + dir, "· the guest has 1 change: sbox sync " + dir} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr.String())
		}
	}
	base, _ := loadBase(state, project)
	host, _ := readManifest(dir, syncRules(nil))
	if base["mac-edits"] != host["mac-edits"] || base["guest-edits"] == (fileState{}) {
		t.Errorf("base after boot %v: want the pushed paths moved, the guest's change still pending", base)
	}
}

// TestDevMainRefreshesGitOnBoot: sync leaves .git out, so a kept drive's
// history would stop at its first boot; each boot copies the Mac's in.
func TestDevMainRefreshesGitOnBoot(t *testing.T) {
	t.Parallel()
	dir, state := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git", "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	for p, body := range map[string]string{"main.go": "v1\n", ".git/HEAD": "ref: refs/heads/main\n", ".git/refs/main": "new\n"} {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	project := mustProjectID(t, dir)
	if err := recordBase(state, project, dir, syncRules(nil)); err != nil {
		t.Fatal(err)
	}
	guest := &fakeSyncGuest{t: t, project: project, files: map[string]string{"main.go": "v1\n", ".git/HEAD": "ref: refs/heads/main\n", ".git/refs/main": "old\n", ".git/refs/guest": "x\n"}}

	var stdout, stderr bytes.Buffer
	if code := devMain(context.Background(), []string{dir, "--", "true"}, strings.NewReader(""), &stdout, &stderr, guest.run, devHost{stateDir: state}); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	want := map[string]string{"main.go": "v1\n", ".git/HEAD": "ref: refs/heads/main\n", ".git/refs/main": "new\n"}
	if !maps.Equal(guest.files, want) {
		t.Errorf("guest after boot: %v; want the Mac's .git in place of its own", guest.files)
	}
}

func TestSyncPatch(t *testing.T) {
	t.Parallel()
	dir, state := t.TempDir(), t.TempDir()
	write := func(root, p, body string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, p), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"edited.txt", "deleted.txt", "src/kept.go"} {
		write(dir, p, "line one\nline two\n")
	}
	project := mustProjectID(t, dir)
	if err := recordBase(state, project, dir, syncRules(nil)); err != nil {
		t.Fatal(err)
	}
	guest := &fakeSyncGuest{t: t, project: project, running: "abababababababab", files: map[string]string{
		"edited.txt": "line one\nline 2\n", "src/kept.go": "line one\nline two\n", "src/new.go": "package main\n",
	}}
	var stdout, stderr bytes.Buffer
	if code := syncMain(context.Background(), []string{"-patch", dir}, &stdout, &stderr, guest.run, devHost{stateDir: state}); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	patch := stdout.String()
	if !strings.Contains(patch, "--- a/edited.txt\n+++ b/edited.txt\n") || !strings.Contains(patch, "+++ b/src/new.go\n") {
		t.Errorf("patch lacks git-style headers:\n%s", patch)
	}

	copyDir := t.TempDir()
	for _, p := range []string{"edited.txt", "deleted.txt", "src/kept.go"} {
		write(copyDir, p, "line one\nline two\n")
	}
	cmd := exec.Command("patch", "-p1", "-s", "-E") // -E: remove files the patch empties, as git apply does
	cmd.Dir, cmd.Stdin = copyDir, strings.NewReader(patch)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("patch -p1 -E: %v\n%s\n%s", err, out, patch)
	}
	got, _ := readManifest(copyDir, syncRules(nil))
	want := manifest{}
	for p, body := range guest.files {
		sum := sha256.Sum256([]byte(body))
		want[p] = fileState{Hash: hex.EncodeToString(sum[:])}
	}
	if !maps.Equal(got, want) {
		t.Errorf("after the patch the copy is %v, want the guest's files %v", got, want)
	}
	if base, _ := loadBase(state, project); base["edited.txt"] == want["edited.txt"] {
		t.Error("-patch moved the base; it only prints")
	}
}

func TestIgnoreRules(t *testing.T) {
	t.Parallel()
	rules := syncRules([]string{"*.log", "dist", "build/cache"})
	for rel, want := range map[string]bool{
		".git": true, "sub/.git": true, "node_modules": true, "a/b/node_modules": true,
		"app.log": true, "logs/x.log": true, "dist": true, "web/dist": true,
		"build/cache": true, "web/build/cache": false, "build": false, "build/out.js": false,
		"src/main.go": false, "distribution": false,
	} {
		if got := rules.skip(rel); got != want {
			t.Errorf("skip(%q) = %v, want %v", rel, got, want)
		}
	}
	if got := rules.findPrune(); !strings.Contains(got, `-name '*.log'`) || !strings.Contains(got, `-path './build/cache'`) {
		t.Errorf("findPrune = %s", got)
	}
}

func TestDevIgnoreInCopyAndSync(t *testing.T) {
	t.Parallel()
	dir, state := t.TempDir(), t.TempDir()
	for p, body := range map[string]string{
		"sbox.yaml": "dev: {ignore: [dist]}\n", "src/app.js": "app\n", "dist/bundle.js": "built on the Mac\n",
		"node_modules/x/index.js": "darwin build\n", ".git/HEAD": "ref: refs/heads/main\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(dir, p), body)
	}
	project := mustProjectID(t, dir)
	guest := &fakeSyncGuest{t: t, project: project, files: map[string]string{}, rules: syncRules([]string{"dist"})}
	var stdout, stderr bytes.Buffer
	start := func() {
		guest.calls = nil
		if code := devMain(context.Background(), []string{dir, "--", "true"}, strings.NewReader(""), &stdout, &stderr, guest.run, devHost{stateDir: state}); code != exitOK {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
	}
	guest.newDrive = true
	start()
	files := tarFiles(t, guest.started)
	if files["skill/src/app.js"] != "app\n" || files["skill/.git/HEAD"] == "" {
		t.Errorf("copy-in lacks the project or its history: %v", slices.Sorted(maps.Keys(files)))
	}
	for _, p := range []string{"skill/dist/bundle.js", "skill/node_modules/x/index.js"} {
		if _, ok := files[p]; ok {
			t.Errorf("copy-in carries %s", p)
		}
	}
	base, _ := loadBase(state, project)
	if _, ok := base["dist/bundle.js"]; ok || base["src/app.js"] == (fileState{}) {
		t.Errorf("base %v: want the project without ignored files", base)
	}

	// The guest builds its own dist; neither side's dist goes anywhere.
	guest.files = map[string]string{"sbox.yaml": "dev: {ignore: [dist]}\n", "src/app.js": "app\n", "dist/bundle.js": "built in the guest\n"}
	stdout.Reset()
	if code := syncMain(context.Background(), []string{"-apply", dir}, &stdout, &stderr, guest.run, devHost{stateDir: state}); code != exitOK {
		t.Fatalf("sync: exit %d: %s", code, stderr.String())
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "dist", "bundle.js")); string(data) != "built on the Mac\n" || strings.Contains(stdout.String(), "dist") {
		t.Errorf("after sync the Mac's dist is %q, output:\n%s", data, stdout.String())
	}
}

func TestGuestChangeCount(t *testing.T) {
	t.Parallel()
	h := func(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
	base := manifest{"same": {Hash: h("1")}, "edited": {Hash: h("1")}, "deleted": {Hash: h("1")}, "run.sh": {Hash: h("1"), Exec: true}}
	sums := h("1") + "  ./same\n" + h("2") + "  ./edited\n" + h("1") + "  ./run.sh\n" + h("3") + "  ./added\n" +
		"\\" + h("4") + "  ./odd\\nname\n" + "garbage\n"
	if got := countChanges(base, sums); got != 4 {
		t.Errorf("countChanges = %d, want 4 (edited, deleted, added, the escaped name)", got)
	}
	if got := countChanges(base, h("1")+"  ./same\n"+h("1")+"  ./edited\n"+h("1")+"  ./deleted\n"+h("1")+"  ./run.sh\n"); got != 0 {
		t.Errorf("no changes: %d", got)
	}
	if s := sumScript(syncRules([]string{"dist"})); !strings.Contains(s, "-name 'dist'") || !strings.Contains(s, "sha256sum") {
		t.Errorf("sumScript = %s", s)
	}
}

// slowGuest is a fakeSyncGuest whose terminal stays open for a while,
// safe to call from the title poller at the same time.
type slowGuest struct {
	mu sync.Mutex
	g  *fakeSyncGuest
}

func (s *slowGuest) run(ctx context.Context, in io.Reader, out, errOut io.Writer, args ...string) error {
	if args[0] == "attach" && slices.Equal(args[2:], []string{"--", "claude"}) {
		time.Sleep(80 * time.Millisecond)
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.g.run(ctx, in, out, errOut, args...)
}

func TestDevMainTerminalTitle(t *testing.T) {
	t.Parallel()
	dir, state := t.TempDir(), t.TempDir()
	writeTestFile(t, filepath.Join(dir, "app.js"), "v1\n")
	project := mustProjectID(t, dir)
	if err := recordBase(state, project, dir, syncRules(nil)); err != nil {
		t.Fatal(err)
	}
	guest := &slowGuest{g: &fakeSyncGuest{t: t, project: project, files: map[string]string{"app.js": "v1\n", "new.js": "x\n"}}}
	var mu sync.Mutex
	var titles []string
	title := func(s string) { mu.Lock(); titles = append(titles, s); mu.Unlock() }
	var stdout, stderr bytes.Buffer
	if code := devMain(context.Background(), []string{dir, "--", "claude"}, strings.NewReader(""), &stdout, &stderr, guest.run,
		devHost{stateDir: state, title: title, pollEvery: 5 * time.Millisecond}); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(titles) != 2 || titles[0] != "sbox: 1 change to sync" || titles[1] != "" {
		t.Errorf("titles %q, want the count once, then cleared", titles)
	}
}
