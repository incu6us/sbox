package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// _neverSynced are names sync skips at any depth: version control, which
// stays each side's own, and folders that are rebuilt rather than copied.
// ponytail: fixed list; a dev.ignore key in the config when someone needs more.
var _neverSynced = map[string]bool{".git": true, "node_modules": true, ".venv": true, "__pycache__": true, ".DS_Store": true}

// readManifest hashes the regular files under dir. Symlinks and the
// never-synced names are skipped.
func readManifest(dir string) (manifest, error) {
	m := make(manifest)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if _neverSynced[d.Name()] && p != dir {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		h, err := hashFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		m[filepath.ToSlash(rel)] = fileState{Hash: h, Exec: info.Mode().Perm()&0o100 != 0}
		return nil
	})
	return m, err
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fileState is what sync compares for one file: its content and whether
// it is executable.
type fileState struct {
	Hash string `json:"sha256"`
	Exec bool   `json:"exec,omitempty"`
}

// manifest maps a project's relative paths to their state.
type manifest map[string]fileState

// Sync operations, per path.
const (
	opPull       = "pull"        // changed in the guest: write it on the Mac
	opPullDelete = "pull-delete" // deleted in the guest: delete it on the Mac
	opPush       = "push"        // changed on the Mac: write it in the guest
	opPushDelete = "push-delete" // deleted on the Mac: delete it in the guest
	opSame       = "same"        // changed the same way on both sides: only the base moves
	opConflict   = "conflict"    // changed differently on both sides: left alone
)

// syncAction is what sync does with one path.
type syncAction struct {
	Path string
	Op   string
}

// planSync compares the Mac's and the guest's files with the base, the
// state both sides last agreed on, and decides per path. A path changed on
// one side only goes to the other; changed on both, it is a conflict
// unless both made the same change. Paths nobody changed are left out.
func planSync(base, host, guest manifest) []syncAction {
	paths := make(map[string]bool)
	for _, m := range []manifest{base, host, guest} {
		for p := range m {
			paths[p] = true
		}
	}
	var plan []syncAction
	for _, p := range slices.Sorted(maps.Keys(paths)) {
		b, inB := base[p]
		h, inH := host[p]
		g, inG := guest[p]
		same := func(x fileState, inX bool, y fileState, inY bool) bool { return inX == inY && (!inX || x == y) }
		var op string
		switch {
		case same(h, inH, g, inG):
			if same(h, inH, b, inB) {
				continue
			}
			op = opSame
		case same(h, inH, b, inB):
			op = opPull
			if !inG {
				op = opPullDelete
			}
		case same(g, inG, b, inB):
			op = opPush
			if !inH {
				op = opPushDelete
			}
		default:
			op = opConflict
		}
		plan = append(plan, syncAction{Path: p, Op: op})
	}
	return plan
}

// applyPulls carries out the guest-to-Mac part of a plan in hostDir, from
// the guest's files unpacked in pulled. Every path is checked before
// anything is written: none may pass through or land on a symlink in
// hostDir, so the guest can't write outside the project through a link.
func applyPulls(hostDir, pulled string, plan []syncAction, guest manifest) error {
	for _, a := range plan {
		if a.Op == opPull || a.Op == opPullDelete {
			if err := noSymlinks(hostDir, a.Path); err != nil {
				return err
			}
		}
	}
	for _, a := range plan {
		target := filepath.Join(hostDir, filepath.FromSlash(a.Path))
		switch a.Op {
		case opPull:
			if err := replaceFile(target, filepath.Join(pulled, filepath.FromSlash(a.Path)), guest[a.Path].Exec); err != nil {
				return err
			}
		case opPullDelete:
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

// noSymlinks fails when rel, or a folder on its way, is a symlink in dir.
func noSymlinks(dir, rel string) error {
	if _, err := insidePath(rel); err != nil {
		return err
	}
	p := dir
	for _, part := range strings.Split(rel, "/") {
		p = filepath.Join(p, part)
		info, err := os.Lstat(p)
		if os.IsNotExist(err) {
			return nil // created on the way
		}
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s: %s is a symlink on the Mac; sync doesn't write through links", rel, p)
		}
	}
	return nil
}

// replaceFile puts src's content at target, through a temporary file and
// a rename, with the executable bit as given.
func replaceFile(target, src string, executable bool) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	tmp, err := os.CreateTemp(filepath.Dir(target), ".sbox-sync-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // gone already once renamed
	mode := os.FileMode(0o644)
	if executable {
		mode = 0o755
	}
	_, err = io.Copy(tmp, in)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), mode)
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), target)
}

// baseFile is where the Mac keeps a project's sync base: the state both
// sides last agreed on. It lives on the Mac, so the guest can't change it.
func baseFile(stateDir, project string) string {
	return filepath.Join(stateDir, "projects", project, "base.json")
}

func loadBase(stateDir, project string) (manifest, error) {
	data, err := os.ReadFile(baseFile(stateDir, project))
	if err != nil {
		return nil, err
	}
	var m manifest
	return m, json.Unmarshal(data, &m)
}

func saveBase(stateDir, project string, m manifest) error {
	p := baseFile(stateDir, project)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// recordBase makes dir's current files the project's sync base.
func recordBase(stateDir, project, dir string) error {
	m, err := readManifest(dir)
	if err != nil {
		return err
	}
	return saveBase(stateDir, project, m)
}

// The guest-side commands of sync, run through attach as the runner.
var (
	// _pullScript sends the guest's project as a tar of its regular files,
	// without the never-synced names.
	_pullScript = pullScript()
	// _pushCommand unpacks the Mac's changed files into the guest's project.
	_pushCommand = []string{"tar", "-C", "/home/runner/work", "-xf", "-"}
)

// _deleteScript deletes the NUL-separated paths on its stdin from the
// guest's project.
const _deleteScript = "cd /home/runner/work && xargs -0 rm -f --"

func pullScript() string {
	var names []string
	for _, n := range slices.Sorted(maps.Keys(_neverSynced)) {
		names = append(names, "-name "+n)
	}
	return `cd /home/runner/work && find . \( ` + strings.Join(names, " -o ") +
		` \) -prune -o -type f -print0 | tar --null --no-recursion -T - -cf -`
}

// syncMain implements `sbox sync [-apply] <dir>`: it compares the project
// on the Mac and in its guest with the base both last agreed on, prints
// what each side changed, and with -apply brings each change to the other
// side. Paths changed differently on both sides are left alone.
func syncMain(ctx context.Context, args []string, stdout, stderr io.Writer, vm sboxRunFunc, stateDir string) int {
	apply := len(args) > 0 && args[0] == "-apply"
	if apply {
		args = args[1:]
	}
	if len(args) != 1 {
		_, _ = fmt.Fprintln(stderr, "usage: sbox sync [-apply] <dir>")
		return exitUsage
	}
	dir := args[0]
	project, err := projectID(dir)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox:", err)
		return exitUsage
	}
	if _, err := loadBase(stateDir, project); err != nil {
		_, _ = fmt.Fprintf(stderr, "sbox: no sync base for %s: sbox dev records one when it first copies the project in (sbox dev -fresh starts over)\n", dir)
		return exitFail
	}
	id, stop, code := projectSession(ctx, dir, project, stderr, vm, stateDir)
	if code != exitOK {
		return code
	}
	defer stop()
	base, err := loadBase(stateDir, project) // again: a new drive resets it
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox:", err)
		return exitFail
	}

	tmp, err := os.MkdirTemp("", "sbox-sync-")
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox:", err)
		return exitFail
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	pulled := filepath.Join(tmp, "guest")
	if err := pullGuest(ctx, vm, id, pulled, stderr); err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox: reading the guest's files:", err)
		return exitFail
	}
	guest, err := readManifest(pulled)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox:", err)
		return exitFail
	}
	host, err := readManifest(dir)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox:", err)
		return exitFail
	}
	plan := planSync(base, host, guest)
	changes := writePlan(stdout, plan, host, guest, dir, pulled, !apply)
	if !apply {
		if changes {
			_, _ = fmt.Fprintf(stdout, "dry run: sbox sync -apply %s does it\n", dir)
		}
		return exitOK
	}

	if err := applyPulls(dir, pulled, plan, guest); err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox:", err)
		return exitFail
	}
	if err := applyPushes(ctx, vm, id, dir, plan, host, stderr); err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox: writing to the guest:", err)
		return exitFail
	}
	if err := saveBase(stateDir, project, nextBase(base, plan, host, guest)); err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox: saving the sync base:", err)
		return exitFail
	}
	return exitOK
}

// projectSession finds the running session on a project, or starts one
// that stop ends (which also flushes the guest's writes).
func projectSession(ctx context.Context, dir, project string, stderr io.Writer, vm sboxRunFunc, stateDir string) (string, func(), int) {
	var ps bytes.Buffer
	if err := vm(ctx, nil, &ps, stderr, "ps"); err != nil {
		return "", nil, vmFailed(stderr, err)
	}
	for _, s := range parseSessions(ps.String()) {
		if s.Project == project && s.State == "running" {
			return s.ID, func() {}, exitOK
		}
	}
	id, fresh, code := startSession(ctx, dir, project, guestJob{command: []string{"sleep", "infinity"}}, stderr, vm)
	if code != exitOK {
		return "", nil, code
	}
	if fresh { // the drive was gone, so the guest just copied dir in
		if err := recordBase(stateDir, project, dir); err != nil {
			_, _ = fmt.Fprintln(stderr, "sbox: recording the sync base:", err)
		}
	}
	return id, func() { _ = sessionMain(ctx, "stop", []string{id}, io.Discard, stderr, vm) }, exitOK
}

// pullGuest unpacks the guest's project files into dest, as a tar from an
// untrusted source: regular files only, nothing outside dest.
func pullGuest(ctx context.Context, vm sboxRunFunc, id, dest string, stderr io.Writer) error {
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := vm(ctx, nil, pw, stderr, "attach", id, "--", "sh", "-c", _pullScript)
		pw.CloseWithError(err)
		done <- err
	}()
	err := extractTar(pr, dest, true)
	if _, derr := io.Copy(io.Discard, pr); err == nil { // tar's trailing padding, or the rest after an error
		err = derr
	}
	_ = pr.Close()
	if verr := <-done; err == nil {
		err = verr
	}
	return err
}

// applyPushes carries out the Mac-to-guest part of a plan: changed files
// as a tar, deletions as a NUL-separated list.
func applyPushes(ctx context.Context, vm sboxRunFunc, id, dir string, plan []syncAction, host manifest, stderr io.Writer) error {
	var files bytes.Buffer
	tw := tar.NewWriter(&files)
	var deletes []string
	for _, a := range plan {
		switch a.Op {
		case opPush:
			data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(a.Path)))
			if err != nil {
				return err
			}
			mode := int64(0o644)
			if host[a.Path].Exec {
				mode = 0o755
			}
			if err := tw.WriteHeader(&tar.Header{Name: "./" + a.Path, Mode: mode, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
				return err
			}
			if _, err := tw.Write(data); err != nil {
				return err
			}
		case opPushDelete:
			deletes = append(deletes, a.Path)
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if files.Len() > 1024 { // more than the end-of-archive blocks
		if err := vm(ctx, &files, io.Discard, stderr, append([]string{"attach", id, "--"}, _pushCommand...)...); err != nil {
			return err
		}
	}
	if len(deletes) > 0 {
		list := strings.Join(deletes, "\x00") + "\x00"
		if err := vm(ctx, strings.NewReader(list), io.Discard, stderr, "attach", id, "--", "sh", "-c", _deleteScript); err != nil {
			return err
		}
	}
	return nil
}

// nextBase is the base after a plan is carried out: each side now agrees
// on every path except the conflicts.
func nextBase(base manifest, plan []syncAction, host, guest manifest) manifest {
	next := maps.Clone(base)
	for _, a := range plan {
		switch a.Op {
		case opPull, opSame:
			if st, ok := guest[a.Path]; ok {
				next[a.Path] = st
			} else {
				delete(next, a.Path)
			}
		case opPush:
			next[a.Path] = host[a.Path]
		case opPullDelete, opPushDelete:
			delete(next, a.Path)
		}
	}
	return next
}

// writePlan prints what each side changed, with diffs of changed files
// when diffs is set, and reports whether anything is to be done.
func writePlan(w io.Writer, plan []syncAction, host, guest manifest, dir, pulled string, diffs bool) bool {
	var fromGuest, fromHost, conflicts []string
	for _, a := range plan {
		_, onHost := host[a.Path]
		_, inGuest := guest[a.Path]
		switch a.Op {
		case opPull:
			fromGuest = append(fromGuest, changeLabel(onHost)+printable(a.Path))
		case opPullDelete:
			fromGuest = append(fromGuest, "deleted  "+printable(a.Path))
		case opPush:
			fromHost = append(fromHost, changeLabel(inGuest)+printable(a.Path))
		case opPushDelete:
			fromHost = append(fromHost, "deleted  "+printable(a.Path))
		case opConflict:
			conflicts = append(conflicts, printable(a.Path))
		}
	}
	var b strings.Builder
	section := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		b.WriteString(title + "\n")
		for _, it := range items {
			b.WriteString("  " + it + "\n")
		}
	}
	section("from the guest:", fromGuest)
	section("from the Mac:", fromHost)
	section("conflicts (changed on both sides, left alone):", conflicts)
	if len(fromGuest)+len(fromHost)+len(conflicts) == 0 {
		b.WriteString("nothing to sync\n")
	}
	if diffs {
		for _, a := range plan {
			if _, onHost := host[a.Path]; a.Op == opPull && onHost {
				b.WriteString(fileDiff(filepath.Join(dir, filepath.FromSlash(a.Path)), filepath.Join(pulled, filepath.FromSlash(a.Path)), a.Path))
			}
		}
	}
	_, _ = io.WriteString(w, b.String())
	return len(fromGuest)+len(fromHost) > 0
}

func changeLabel(existed bool) string {
	if existed {
		return "changed  "
	}
	return "added    "
}

// fileDiff is a unified diff of a file on the Mac and its guest version,
// escaped for the terminal; empty when diff isn't there.
func fileDiff(macFile, guestFile, rel string) string {
	out, _ := exec.Command("diff", "-u", "--label", "mac/"+rel, "--label", "guest/"+rel, macFile, guestFile).Output() // exit 1 means "differs"
	var b strings.Builder
	for _, line := range strings.SplitAfter(string(out), "\n") {
		text, nl := strings.CutSuffix(line, "\n")
		b.WriteString(printable(text))
		if nl {
			b.WriteString("\n")
		}
	}
	return b.String()
}
