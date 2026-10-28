package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// ignoreRules are what sync skips: a name (globs allowed) at any depth, or
// a path from the project root when it has a slash (a leading one ties a
// plain name to the root). A rule starting with ! takes what it matches
// back out, whatever its place in the list.
type ignoreRules []string

// copyRules are what sbox dev leaves out of the project it copies in:
// folders rebuilt in the guest (a Mac's node_modules has Mac binaries) and
// extra, from dev.ignore.
func copyRules(extra []string) ignoreRules {
	return append(ignoreRules{".DS_Store", ".venv", "__pycache__", "node_modules"}, extra...)
}

// syncRules are what sync skips: the copy rules, and version control,
// which stays each side's own (the guest gets a copy of .git, never back).
func syncRules(extra []string) ignoreRules {
	return append(ignoreRules{".git"}, copyRules(extra)...)
}

// gitignoreRules turns the project's root .gitignore into rules: git's
// trailing / and leading **/ dropped, a slash before the end anchoring to
// the root. ponytail: root file only, and a ** in the middle drops the
// line; nested .gitignore files and git's ** would need a real matcher.
func gitignoreRules(dir string) []string {
	data, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		return nil
	}
	var rules []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, " \t\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pat, negated := strings.CutPrefix(line, "!")
		pat = strings.TrimSuffix(pat, "/")
		if rest, ok := strings.CutPrefix(pat, "**/"); ok && !strings.Contains(rest, "/") {
			pat = rest
		} else if strings.Contains(pat, "/") && !strings.HasPrefix(pat, "/") {
			pat = "/" + pat
		}
		pat = strings.TrimSuffix(pat, "/**")
		if _, err := path.Match(pat, ""); err != nil || strings.Contains(pat, "**") || strings.Trim(pat, "/") == "" {
			continue
		}
		if negated {
			pat = "!" + pat
		}
		rules = append(rules, pat)
	}
	return rules
}

// gitignoreNote is the preflight line naming what at the project's root
// rules leave out because .gitignore lists it (a .env the app reads, say),
// or "" when nothing is.
func gitignoreNote(dir string, rules ignoreRules) string {
	gitignore := ignoreRules(gitignoreRules(dir))
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		if n := e.Name(); rules.skip(n) && gitignore.skip(n) && !syncRules(nil).skip(n) {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return ""
	}
	list := names
	if len(list) > 5 {
		list = list[:5]
	}
	more := ""
	if n := len(names) - len(list); n > 0 {
		more = fmt.Sprintf(" and %d more", n)
	}
	return fmt.Sprintf("left out by .gitignore: %s%s (dev.ignore: [\"!%s\"] copies one in)", printable(strings.Join(list, ", ")), more, printable(names[0]))
}

// skip reports whether the path rel (slash-separated, from the root) is
// ignored.
func (r ignoreRules) skip(rel string) bool {
	hit := false
	for _, pat := range r {
		pat, negated := strings.CutPrefix(pat, "!")
		if negated && ruleMatches(pat, rel) {
			return false
		}
		hit = hit || !negated && ruleMatches(pat, rel)
	}
	return hit
}

// ruleMatches reports whether one rule, without its !, matches rel.
func ruleMatches(pat, rel string) bool {
	subject := path.Base(rel)
	if strings.Contains(pat, "/") {
		pat, subject = strings.TrimPrefix(pat, "/"), rel
	}
	ok, _ := path.Match(pat, subject) // patterns are checked when the config loads
	return ok
}

// findPrune is find's test for the rules, for a find run from the root:
// -name for names, -path for paths, the ! rules taken out.
func (r ignoreRules) findPrune() string { return r.findPruneUnder(".") }

// findPruneUnder is findPrune for a find started at root.
func (r ignoreRules) findPruneUnder(root string) string {
	var tests, negated []string
	for _, pat := range r {
		pat, neg := strings.CutPrefix(pat, "!")
		test := "-name " + shellJoin([]string{pat})
		if strings.Contains(pat, "/") {
			test = "-path " + shellJoin([]string{root + "/" + strings.TrimPrefix(pat, "/")})
		}
		if neg {
			negated = append(negated, test)
		} else {
			tests = append(tests, test)
		}
	}
	prune := `\( ` + strings.Join(tests, " -o ") + ` \)`
	if len(negated) > 0 {
		prune = `\( ` + prune + ` ! \( ` + strings.Join(negated, " -o ") + ` \) \)`
	}
	return prune
}

// readManifest hashes the regular files under dir. Symlinks and what the
// rules ignore are skipped.
func readManifest(dir string, rules ignoreRules) (manifest, error) {
	m := make(manifest)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if rel, _ := filepath.Rel(dir, p); p != dir && rules.skip(filepath.ToSlash(rel)) {
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
func recordBase(stateDir, project, dir string, rules ignoreRules) error {
	m, err := readManifest(dir, rules)
	if err != nil {
		return err
	}
	return saveBase(stateDir, project, m)
}

// The guest-side commands of sync, run through attach as the runner.
var (
	// _pushCommand unpacks the Mac's changed files into the guest's project.
	_pushCommand = []string{"tar", "-C", "/home/runner/work", "-xf", "-"}
)

// _deleteScript deletes the NUL-separated paths on its stdin from the
// guest's project.
const _deleteScript = "cd /home/runner/work && xargs -0 rm -f --"

// sumScript prints a SHA-256 per regular file of the guest's project, in
// sha256sum's format, without what the rules ignore: cheaper than a pull
// when only the count of changes is wanted.
func sumScript(rules ignoreRules) string {
	return `cd /home/runner/work && find . ` + rules.findPrune() + ` -prune -o -type f -print0 | xargs -0 -r sha256sum`
}

// countChanges counts the paths where the guest's sums (sumScript's
// output, from the guest, so parsed strictly) differ from the base: edited,
// added or deleted. A name sha256sum had to escape counts as changed.
func countChanges(base manifest, sums string) int {
	seen := make(map[string]bool)
	n := 0
	for _, line := range strings.Split(sums, "\n") {
		hash, file, ok := strings.Cut(line, "  ./")
		switch {
		case strings.HasPrefix(line, "\\"):
			n++
		case !ok || len(hash) != 64:
		default:
			seen[file] = true
			if base[file].Hash != hash {
				n++
			}
		}
	}
	for p := range base {
		if !seen[p] {
			n++
		}
	}
	return n
}

// guestChanges counts the guest's changes not yet synced, or returns -1
// when there is no base to count against or the guest doesn't answer.
func guestChanges(ctx context.Context, vm sboxRunFunc, id, stateDir, project string, rules ignoreRules) int {
	base, err := loadBase(stateDir, project)
	if err != nil {
		return -1
	}
	var sums bytes.Buffer
	if err := vm(ctx, strings.NewReader(""), &sums, io.Discard, "attach", id, "--", "sh", "-c", sumScript(rules)); err != nil {
		return -1
	}
	return countChanges(base, sums.String())
}

// pullScript sends the guest's project as a tar of its regular files,
// without what the rules ignore.
func pullScript(rules ignoreRules) string {
	return `cd /home/runner/work && find . ` + rules.findPrune() + ` -prune -o -type f -print0 | tar --null --no-recursion -T - -cf -`
}

// syncMain implements `sbox sync [-apply] <dir>`: it compares the project
// on the Mac and in its guest with the base both last agreed on, prints
// what each side changed, and with -apply brings each change to the other
// side. Paths changed differently on both sides are left alone.
func syncMain(ctx context.Context, args []string, stdout, stderr io.Writer, vm sboxRunFunc, mac devHost) int {
	stateDir := mac.stateDir
	flags := flag.NewFlagSet("sbox sync", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	applyFlag := flags.Bool("apply", false, "")
	patchFlag := flags.Bool("patch", false, "")
	flags.StringVar(&mac.projectConfig, "config", "", "")
	flags.StringVar(&mac.workspace, "workspace", "", "")
	if flags.Parse(args) != nil || flags.NArg() != 1 || (*applyFlag && *patchFlag) {
		_, _ = fmt.Fprintln(stderr, "usage: sbox sync [-apply | -patch] [-config <file>] [-workspace <name>] <dir>")
		return exitUsage
	}
	apply := *applyFlag
	mode := ""
	if *patchFlag {
		mode = "-patch"
	}
	dir := flags.Arg(0)
	project, err := projectID(dir)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox:", err)
		return exitUsage
	}
	cfg, err := mac.loadConfig(dir)
	if err != nil {
		return configFailed(stderr, err)
	}
	cfg.Dev.Ignore = append(gitignoreRules(dir), cfg.Dev.Ignore...)
	if _, err := loadBase(stateDir, project); err != nil {
		_, _ = fmt.Fprintf(stderr, "sbox: no sync base for %s: sbox dev records one when it first copies the project in (sbox dev -fresh starts over)\n", dir)
		return exitFail
	}
	rules := syncRules(cfg.Dev.Ignore)
	id, stop, code := projectSession(ctx, dir, project, guestJob{command: []string{"sleep", "infinity"}}, rules, stderr, vm, stateDir)
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
	if err := pullGuest(ctx, vm, id, pulled, rules, stderr); err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox: reading the guest's files:", err)
		return exitFail
	}
	guest, err := readManifest(pulled, rules)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox:", err)
		return exitFail
	}
	host, err := readManifest(dir, rules)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox:", err)
		return exitFail
	}
	plan := planSync(base, host, guest)
	if mode == "-patch" {
		patch := guestPatch(plan, host, dir, pulled)
		if f, ok := stdout.(*os.File); ok && isTerminal(f) {
			patch = consoleText(patch) // the guest wrote it: escaped for a terminal, raw into a file
		}
		_, _ = io.WriteString(stdout, patch)
		return exitOK
	}
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

// projectSession finds the live session on a project, resuming a paused
// one until done, or starts one that done stops (which also flushes the
// guest's writes).
func projectSession(ctx context.Context, dir, project string, job guestJob, rules ignoreRules, stderr io.Writer, vm sboxRunFunc, stateDir string) (id string, done func(), code int) {
	live, found, code := liveSession(ctx, project, stderr, vm)
	switch {
	case code != exitOK:
		return "", nil, code
	case found && live.State == "paused":
		if err := vm(ctx, nil, io.Discard, stderr, "resume", live.ID); err != nil {
			return "", nil, vmFailed(stderr, err)
		}
		return live.ID, func() { _ = vm(ctx, nil, io.Discard, stderr, "pause", live.ID) }, exitOK
	case found:
		return live.ID, func() {}, exitOK
	}
	id, fresh, code := startSession(ctx, dir, project, job, stderr, vm)
	if code != exitOK {
		return "", nil, code
	}
	if fresh { // the drive was gone, so the guest just copied dir in
		if err := recordBase(stateDir, project, dir, rules); err != nil {
			_, _ = fmt.Fprintln(stderr, "sbox: recording the sync base:", err)
		}
	}
	return id, func() { _ = sessionMain(ctx, "stop", []string{id}, io.Discard, stderr, vm) }, exitOK
}

// pullGuest unpacks the guest's project files into dest, as a tar from an
// untrusted source: regular files only, nothing outside dest.
func pullGuest(ctx context.Context, vm sboxRunFunc, id, dest string, rules ignoreRules, stderr io.Writer) error {
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := vm(ctx, nil, pw, stderr, "attach", id, "--", "sh", "-c", pullScript(rules))
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

// changeRoot is a folder sbox run brings back: its path on the Mac, its
// path in the guest relative to the runner's home ("work" for the folder
// run was given, an rw mount's otherwise), the manifest it was copied in
// with, and the rules that leave things out of both.
type changeRoot struct {
	host, guest string
	base        manifest
	rules       ignoreRules
}

// outScript tars the roots from the runner's home, each with its rules,
// for sbox run -apply, -patch and -push.
func outScript(roots []changeRoot) string {
	var finds []string
	for _, r := range roots {
		finds = append(finds, "find "+shellJoin([]string{r.guest})+" "+r.rules.findPruneUnder(r.guest)+" -prune -o -type f -print0")
	}
	return "cd /home/runner && { " + strings.Join(finds, "; ") + "; } | tar --null --no-recursion -T - -cf -"
}

// runChanges brings back what sbox run's command changed in the roots:
// the guest's tar (sbox-run out), each root planned against its base, so
// files the Mac changed meanwhile are left alone (or listed as
// conflicts). It shows them with diffs; writes the first root's (the
// folder run was given) to patchFile as a patch; or with apply writes
// them all once confirm says yes, returning the paths it wrote in the
// first root. With no confirm (no terminal), -apply is refused.
func runChanges(ctx context.Context, vm sboxRunFunc, id string, roots []changeRoot, apply bool, patchFile string, confirm func() bool, stdout, stderr io.Writer) (applied []string, code int) {
	fail := func(err error) ([]string, int) {
		_, _ = fmt.Fprintln(stderr, "sbox:", err)
		return nil, exitFail
	}
	tmp, err := os.MkdirTemp("", "sbox-run-")
	if err != nil {
		return fail(err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	pulled := filepath.Join(tmp, "guest")
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := vm(ctx, nil, pw, stderr, "out", id)
		pw.CloseWithError(err)
		done <- err
	}()
	err = extractTar(pr, pulled, true)
	_, _ = io.Copy(io.Discard, pr)
	if verr := <-done; err == nil {
		err = verr
	}
	if err != nil {
		return fail(fmt.Errorf("reading the guest's files: %w", err))
	}

	type rootPlan struct {
		root        changeRoot
		pulled      string
		host, guest manifest
		plan        []syncAction
	}
	var plans []rootPlan
	for _, r := range roots {
		p := rootPlan{root: r, pulled: filepath.Join(pulled, filepath.FromSlash(r.guest))}
		if p.guest, err = readManifest(p.pulled, r.rules); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fail(err)
		}
		if p.host, err = readManifest(r.host, r.rules); err != nil {
			return fail(err)
		}
		p.plan = slices.DeleteFunc(planSync(r.base, p.host, p.guest), func(a syncAction) bool {
			return a.Op == opPush || a.Op == opPushDelete || a.Op == opSame // the Mac's own, or already alike
		})
		plans = append(plans, p)
	}
	if patchFile != "" {
		if err := os.WriteFile(patchFile, []byte(guestPatch(plans[0].plan, plans[0].host, plans[0].root.host, plans[0].pulled)), 0o644); err != nil {
			return fail(err)
		}
		_, _ = fmt.Fprintf(stderr, "sbox: the command's changes are in %s\n", patchFile)
		for _, p := range plans[1:] {
			if len(p.plan) > 0 {
				_, _ = fmt.Fprintf(stderr, "sbox: the rw mount %s changed too; it isn't in the patch (-apply brings it)\n", p.root.host)
			}
		}
		return nil, exitOK
	}
	changes := false
	for _, p := range plans {
		if len(plans) > 1 && len(p.plan) > 0 {
			_, _ = fmt.Fprintf(stdout, "in %s:\n", p.root.host)
		}
		if writePlan(stdout, p.plan, p.host, p.guest, p.root.host, p.pulled, true) {
			changes = true
		}
	}
	if !changes || !apply {
		return nil, exitOK
	}
	if confirm == nil {
		return fail(errors.New("-apply asks before writing: run it in a terminal, or use -patch <file>"))
	}
	if !confirm() {
		_, _ = fmt.Fprintln(stderr, "sbox: left as it was")
		return nil, exitOK
	}
	for i, p := range plans {
		if err := applyRoot(p.root.host, p.pulled, p.plan, p.guest); err != nil {
			return fail(err)
		}
		for _, a := range p.plan {
			if i == 0 && (a.Op == opPull || a.Op == opPullDelete) {
				applied = append(applied, a.Path)
			}
		}
	}
	return applied, exitOK
}

// applyRoot writes one root's pulls: a folder through applyPulls, a single
// mounted file in place.
func applyRoot(host, pulled string, plan []syncAction, guest manifest) error {
	if info, err := os.Stat(host); err == nil && !info.IsDir() {
		for _, a := range plan {
			if a.Op == opPull && a.Path == "." {
				return replaceFile(host, pulled, guest["."].Exec)
			}
		}
		return nil
	}
	return applyPulls(host, pulled, plan, guest)
}

// pushOnBoot brings the Mac's changes into a kept home when sbox dev boots
// it, so the agent starts from your latest edits: only the Mac-to-guest
// half of the sync plan is carried out, and the base moves for those paths
// alone. The guest's own changes and conflicts wait for sbox sync, with a
// line saying so. The Mac's .git replaces the guest's. A project with no
// base (never copied in) is left alone.
func pushOnBoot(ctx context.Context, vm sboxRunFunc, id, dir, project, stateDir string, rules ignoreRules, stderr io.Writer) error {
	base, err := loadBase(stateDir, project)
	if err != nil {
		return nil
	}
	tmp, err := os.MkdirTemp("", "sbox-sync-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	pulled := filepath.Join(tmp, "guest")
	if err := pullGuest(ctx, vm, id, pulled, rules, stderr); err != nil {
		return err
	}
	guest, err := readManifest(pulled, rules)
	if err != nil {
		return err
	}
	host, err := readManifest(dir, rules)
	if err != nil {
		return err
	}
	var pushes []syncAction
	waiting := 0
	for _, a := range planSync(base, host, guest) {
		switch a.Op {
		case opPush, opPushDelete, opSame:
			pushes = append(pushes, a)
		default:
			waiting++
		}
	}
	if err := applyPushes(ctx, vm, id, dir, pushes, host, stderr); err != nil {
		return err
	}
	// Sync leaves .git out, so the guest's history would stop at the
	// drive's first boot: the Mac's replaces it, guest commits included.
	// ponytail: copied whole every boot; compare a fingerprint if a big
	// .git makes boots slow.
	if info, err := os.Lstat(filepath.Join(dir, ".git")); err == nil && info.IsDir() {
		if err := copyMounts(ctx, vm, id, []mountCopy{{filepath.Join(dir, ".git"), "/home/runner/work/.git"}}, stderr); err != nil {
			return err
		}
	}
	if err := saveBase(stateDir, project, nextBase(base, pushes, host, guest)); err != nil {
		return err
	}
	if n := len(pushes); n > 0 {
		_, _ = fmt.Fprintf(stderr, "sbox: %d %s from the Mac copied in\n", n, plural(n, "change", "changes"))
	}
	if waiting > 0 {
		_, _ = fmt.Fprintf(stderr, "sbox: the guest has %d %s of its own: sbox sync %s\n", waiting, plural(waiting, "change", "changes"), dir)
	}
	return nil
}

// plural picks the word for n.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
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

// guestPatch is the guest's side of a plan as one unified diff with git's
// a/ and b/ prefixes, for git apply or patch -p1 on the Mac. The base
// doesn't move: once applied, the next sync finds both sides agree.
func guestPatch(plan []syncAction, host manifest, dir, pulled string) string {
	var b strings.Builder
	for _, a := range plan {
		mac, guest := filepath.Join(dir, filepath.FromSlash(a.Path)), filepath.Join(pulled, filepath.FromSlash(a.Path))
		left, right := "a/"+a.Path, "b/"+a.Path
		switch a.Op {
		case opPull:
			if _, onHost := host[a.Path]; !onHost {
				mac, left = os.DevNull, os.DevNull
			}
		case opPullDelete:
			guest, right = os.DevNull, os.DevNull
		default:
			continue
		}
		out, _ := exec.Command("diff", "-u", "--label", left, "--label", right, mac, guest).Output() // exit 1 means "differs"
		b.Write(out)
	}
	return b.String()
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
