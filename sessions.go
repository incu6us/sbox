package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// sboxRunFunc runs sbox-run in the VM as root with args and input on stdin.
// Tests pass a fake.
type sboxRunFunc func(ctx context.Context, in io.Reader, out, errOut io.Writer, args ...string) error

// _sessionID matches what sbox-run accepts.
var _sessionID = regexp.MustCompile(`^[a-f0-9]{8,32}$`)

// _sessionDone is what each session action prints on success.
var _sessionDone = map[string]string{"stop": "stopped", "pause": "paused", "resume": "resumed"}

// sessionMain implements `sbox ps`, `sbox clean`, and
// `sbox stop|pause|resume <id>`.
func sessionMain(ctx context.Context, cmd string, args []string, stdout, stderr io.Writer, vm sboxRunFunc) int {
	if cmd == "clean" {
		if len(args) != 0 {
			_, _ = fmt.Fprintln(stderr, "usage: sbox clean")
			return exitUsage
		}
		if err := vm(ctx, nil, io.Discard, stderr, "clean"); err != nil {
			return vmFailed(stderr, err)
		}
		_, _ = fmt.Fprintln(stdout, "removed every project's saved state")
		return exitOK
	}
	if cmd == "ps" {
		if len(args) != 0 {
			_, _ = fmt.Fprintln(stderr, "usage: sbox ps")
			return exitUsage
		}
		var out bytes.Buffer
		if err := vm(ctx, nil, &out, stderr, "ps"); err != nil {
			return vmFailed(stderr, err)
		}
		writeSessions(stdout, parseSessions(out.String()), time.Now())
		return exitOK
	}

	if len(args) != 1 {
		_, _ = fmt.Fprintf(stderr, "usage: sbox %s <session-id>\n", cmd)
		return exitUsage
	}
	id := args[0]
	if !_sessionID.MatchString(id) {
		_, _ = fmt.Fprintf(stderr, "sbox: %q is not a session id (sbox ps lists them)\n", id)
		return exitUsage
	}
	if err := vm(ctx, nil, io.Discard, stderr, cmd, id); err != nil {
		return vmFailed(stderr, err)
	}
	_, _ = fmt.Fprintln(stdout, id, _sessionDone[cmd])
	return exitOK
}

// startMain implements `sbox start <dir> -- <command...>`: the run that
// sbox run would do, as a session that keeps running after this returns.
// It prints the session's id once the guest is up.
func startMain(ctx context.Context, args []string, stdout, stderr io.Writer, vm sboxRunFunc) int {
	if len(args) < 3 || args[1] != "--" {
		_, _ = fmt.Fprintln(stderr, "usage: sbox start <dir> -- <command> [args...]")
		return exitUsage
	}
	id, _, code := startSession(ctx, args[0], "", guestJob{command: args[2:]}, stderr, vm)
	if code == exitOK {
		_, _ = fmt.Fprintln(stdout, id)
	}
	return code
}

// projectID names a project folder's state in the VM: the start of the
// SHA-256 of its resolved absolute path, so the same folder reached by
// another path is the same project.
func projectID(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(real))
	return hex.EncodeToString(sum[:8]), nil
}

// startSession sends dir and job to a new session and returns its id once
// the guest is up, or the exit code to fail with. With a project id, the
// session gets that project's work drive; fresh reports that the drive was
// new, so the guest copied dir in.
func startSession(ctx context.Context, dir, project string, job guestJob, stderr io.Writer, vm sboxRunFunc) (id string, fresh bool, code int) {
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		_, _ = fmt.Fprintf(stderr, "sbox: %s: not a directory\n", dir)
		return "", false, exitUsage
	}
	var ids [3]string // session id, nonce, canary
	for i, n := range []int{8, 16, 6} {
		var err error
		if ids[i], err = randomHex(n); err != nil {
			_, _ = fmt.Fprintln(stderr, "sbox:", err)
			return "", false, exitFail
		}
	}
	id, nonce, canary := ids[0], ids[1], cmp.Or(job.canary, ids[2])

	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeRunTar(pw, dir, job, nonce, canary)) }()
	var out bytes.Buffer
	call := []string{"start", id}
	if project != "" {
		call = append(call, project)
	}
	err := vm(ctx, pr, &out, stderr, call...)
	_ = pr.Close() // unblocks the writer if sbox-run stopped reading early
	if err != nil {
		return "", false, vmFailed(stderr, err)
	}
	if !strings.Contains(out.String(), "READY "+id+"\n") {
		_, _ = fmt.Fprintf(stderr, "sbox: session %s did not report ready\n", id)
		return "", false, exitFail
	}
	return id, strings.Contains(out.String(), "NEW "+id+"\n"), exitOK
}

// attachArgs reports whether args after the first are empty or
// "-- <command...>".
func attachArgs(args []string) bool {
	return len(args) == 1 || (len(args) > 2 && args[1] == "--")
}

// attachMain implements `sbox attach <id> [-- <command...>]`: a terminal
// in a running session's guest, by default a login shell. It exits with
// the guest command's status.
func attachMain(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, vm sboxRunFunc) int {
	if len(args) == 0 || !attachArgs(args) {
		_, _ = fmt.Fprintln(stderr, "usage: sbox attach <session-id> [-- <command> [args...]]")
		return exitUsage
	}
	if !_sessionID.MatchString(args[0]) {
		_, _ = fmt.Fprintf(stderr, "sbox: %q is not a session id (sbox ps lists them)\n", args[0])
		return exitUsage
	}
	return attachExit(stderr, vm(ctx, stdin, stdout, stderr, append([]string{"attach"}, args...)...))
}

// devMain implements `sbox dev [-fresh] <dir> [-- <command...>]`: a
// terminal in the project's session. A running or paused session on the
// project is attached to again; otherwise a new one starts. The project's
// work drive (the runner's home, with the project in it) is kept between
// sessions; -fresh stops the live session and discards the drive first.
// When the drive is new, what was copied in becomes the sync base, kept in
// host.stateDir on the Mac. The session's network and environment come
// from the user's config and the project's sbox.yaml. At detach, host.keep
// says whether the session stays up; with none (no terminal to ask on) it
// is stopped.
func devMain(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, vm sboxRunFunc, host devHost) int {
	const usage = "usage: sbox dev [-fresh | -restart] [-config <file>] [-command <cmd>] [-trace] <dir | GitHub URL> [-- <command> [args...]]"
	flags := flag.NewFlagSet("sbox dev", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	fresh := flags.Bool("fresh", false, "")
	restart := flags.Bool("restart", false, "")
	flags.StringVar(&host.projectConfig, "config", "", "")
	command := flags.String("command", "", "")
	trace := flags.Bool("trace", false, "")
	if flags.Parse(args) != nil {
		_, _ = fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	args = flags.Args()
	if len(args) == 0 || !attachArgs(args) || (*fresh && *restart) || (*command != "" && len(args) > 1) {
		_, _ = fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	if t, ok := parseTarget(args[0]); ok {
		dir, code := host.checkout(ctx, t, stderr)
		if code != exitOK {
			return code
		}
		args = append([]string{dir}, args[1:]...)
	}
	project, err := projectID(args[0])
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox:", err)
		return exitUsage
	}
	cfg, job, err := host.devConfig(args[0])
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox:", err)
		return exitUsage
	}
	live, found, code := liveSession(ctx, project, stderr, vm)
	if code != exitOK {
		return code
	}
	if found && (*fresh || *restart) {
		if err := vm(ctx, nil, io.Discard, stderr, "stop", live.ID); err != nil {
			return vmFailed(stderr, err)
		}
		if *restart {
			_, _ = fmt.Fprintf(stderr, "sbox: stopped session %s to boot one with the config as it is now; the home is kept\n", live.ID)
		}
		found = false
	}
	if *fresh {
		if err := vm(ctx, nil, io.Discard, stderr, "forget", project); err != nil {
			return vmFailed(stderr, err)
		}
	}

	id, started := live.ID, live.Started
	if found {
		if live.State == "paused" {
			if err := vm(ctx, nil, io.Discard, stderr, "resume", id); err != nil {
				return vmFailed(stderr, err)
			}
		}
		_, _ = fmt.Fprintf(stderr, "sbox: attached to session %s, running %s\n", id, age(time.Since(live.Started)))
		if changed := configChanged(host.stateDir, project, id, cfg); len(changed) > 0 {
			applied, rest := applyLive(ctx, vm, id, cfg, job, changed, stderr)
			if len(applied) > 0 {
				_, _ = fmt.Fprintf(stderr, "sbox: applied %s to session %s\n", strings.Join(applied, ", "), id)
				if err := markApplied(host.stateDir, project, id, cfg, applied); err != nil {
					_, _ = fmt.Fprintln(stderr, "sbox: recording the session's config:", err)
				}
			}
			warnChanged(stderr, id, args[0], rest)
		}
	} else {
		for _, line := range devPreflight(cfg, job.secrets, readMCPJSON(args[0])) {
			_, _ = fmt.Fprintln(stderr, line)
		}
		started = time.Now()
		var err error
		if job.canary, err = randomHex(6); err != nil {
			_, _ = fmt.Fprintln(stderr, "sbox:", err)
			return exitFail
		}
		var newDrive bool
		if id, newDrive, code = startSession(ctx, args[0], project, job, stderr, vm); code != exitOK {
			return code
		}
		if err := recordBoot(host.stateDir, project, id, job.canary, cfg); err != nil {
			_, _ = fmt.Fprintln(stderr, "sbox: recording the session's config:", err)
		}
		if newDrive {
			if err := recordBase(host.stateDir, project, args[0], syncRules(cfg.Dev.Ignore)); err != nil {
				_, _ = fmt.Fprintln(stderr, "sbox: recording the sync base:", err)
			}
			_, _ = fmt.Fprintln(stderr, "sbox: the root is read-only; install tools into the home (~/.local, rustup, uv python install, npm prefix ~/.local), which is kept between sessions")
		} else if err := pushOnBoot(ctx, vm, id, args[0], project, host.stateDir, syncRules(cfg.Dev.Ignore), stderr); err != nil {
			_, _ = fmt.Fprintln(stderr, "sbox: copying the Mac's changes in:", err)
		}
		if code := runHooks(ctx, id, cfg, false, stdout, stderr, vm); code != exitOK {
			_, _ = fmt.Fprintf(stderr, "sbox: the terminal opens anyway; fix it there, then sbox setup-env %s\n", args[0])
		}
	}

	var inner []string // the terminal's command; a login shell when empty
	switch {
	case len(args) > 1:
		inner = args[2:]
	case cmp.Or(*command, cfg.devCommand()) != "":
		inner = []string{"bash", "-lc", cmp.Or(*command, cfg.devCommand())}
	}
	if *trace {
		inner = traceCommand(inner)
	}
	attach := []string{id}
	if len(inner) > 0 {
		attach = append(attach, append([]string{"--"}, inner...)...)
	}
	stopWatch, watched := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watched)
		if host.title != nil {
			host.watchChanges(ctx, vm, id, project, syncRules(cfg.Dev.Ignore), stopWatch)
		}
	}()
	code = attachMain(ctx, attach, stdin, stdout, stderr, vm)
	close(stopWatch)
	<-watched
	unsynced := guestChanges(ctx, vm, id, host.stateDir, project, syncRules(cfg.Dev.Ignore))
	_, _ = io.WriteString(stderr, collectSummary(ctx, id, args[0], started, unsynced, cfg, vm))
	if *trace {
		var out bytes.Buffer
		_ = vm(ctx, strings.NewReader(""), &out, io.Discard, "attach", id, "--", "sh", "-c", _traceReadBack)
		for _, line := range traceLines(out.String(), bootCanary(host.stateDir, project, id)) {
			_, _ = fmt.Fprintln(stderr, line)
		}
	}
	if host.keep != nil && host.keep(id, unsynced) {
		_, _ = fmt.Fprintf(stderr, "sbox: session %s keeps running: sbox dev attaches again, sbox stop %s ends it\n", id, id)
		return code
	}
	_ = sessionMain(ctx, "stop", []string{id}, io.Discard, stderr, vm)
	return code
}

// setupEnvMain implements `sbox setup-env <dir>`: the project's dev.setup
// steps again, in its live session or in one started for them.
func setupEnvMain(ctx context.Context, args []string, stdout, stderr io.Writer, vm sboxRunFunc, host devHost) int {
	flags := flag.NewFlagSet("sbox setup-env", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&host.projectConfig, "config", "", "")
	if flags.Parse(args) != nil || flags.NArg() != 1 {
		_, _ = fmt.Fprintln(stderr, "usage: sbox setup-env [-config <file>] <dir>")
		return exitUsage
	}
	args = flags.Args()
	project, err := projectID(args[0])
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox:", err)
		return exitUsage
	}
	cfg, job, err := host.devConfig(args[0])
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox:", err)
		return exitUsage
	}
	if len(cfg.Dev.Setup) == 0 {
		_, _ = fmt.Fprintln(stdout, "no dev.setup steps in the config")
		return exitOK
	}
	id, done, code := projectSession(ctx, args[0], project, job, syncRules(cfg.Dev.Ignore), stderr, vm, host.stateDir)
	if code != exitOK {
		return code
	}
	defer done()
	warnConfigChanged(stderr, host.stateDir, project, id, args[0], cfg)
	return runHooks(ctx, id, cfg, true, stdout, stderr, vm)
}

// devHost is what dev commands take from the Mac: where the sync base and
// the user's config live, and how to ask about keeping a session at
// detach (nil: no terminal to ask on).
type devHost struct {
	stateDir, userConfig string
	keep                 func(id string, unsynced int) bool
	lookupEnv            func(string) (string, bool) // the Mac's environment, for secrets; os.LookupEnv when nil
	home                 string                      // for ~ in mounts; the user's home when empty
	cwd                  string                      // where sbox dev <GitHub URL> makes its folder; "." when empty
	fetch                fetchFunc                   // GitHub downloads; githubFetch when nil
	projectConfig        string                      // -config: the file used instead of the project's sbox.yaml
	title                func(string)                // sets the terminal's title; no change notice when nil
	pollEvery            time.Duration               // how often the notice counts the guest's changes; 10 s when 0
}

// watchChanges puts the count of the guest's unsynced changes in the
// terminal's title while the terminal is attached, until stop is closed,
// then clears it. The title, not the screen: printing would garble a
// full-screen program like Claude Code.
func (h devHost) watchChanges(ctx context.Context, vm sboxRunFunc, id, project string, rules ignoreRules, stop <-chan struct{}) {
	every := cmp.Or(h.pollEvery, 10*time.Second)
	shown := ""
	defer func() {
		if shown != "" {
			h.title("")
		}
	}()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
		text := ""
		switch n := guestChanges(ctx, vm, id, h.stateDir, project, rules); {
		case n > 0:
			text = fmt.Sprintf("sbox: %d %s to sync", n, plural(n, "change", "changes"))
		case n == 0:
			text = "sbox: nothing to sync"
		}
		if text != "" && text != shown {
			h.title(text)
			shown = text
		}
	}
}

// checkout copies a GitHub target into a new folder named after it in
// h.cwd, for sbox dev to work on (sbox sync writes there), and returns
// the folder.
func (h devHost) checkout(ctx context.Context, t target, stderr io.Writer) (string, int) {
	name := t.repo
	if t.path != "" {
		name = path.Base(t.path)
	}
	local := filepath.Join(cmp.Or(h.cwd, "."), name)
	if _, err := os.Lstat(local); err == nil {
		_, _ = fmt.Fprintf(stderr, "sbox: %s exists; sbox dev %s works on it\n", local, local)
		return "", exitUsage
	}
	fetch := h.fetch
	if fetch == nil {
		fetch = githubFetch(ctx)
	}
	src, commit, err := fetch(t)
	if err == nil {
		err = os.CopyFS(local, os.DirFS(src))
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox:", err)
		return "", exitFail
	}
	_, _ = fmt.Fprintf(stderr, "sbox: %s copied to %s; sbox sync brings the agent's work there\n", t.display(commit), local)
	return local, exitOK
}

// loadConfig reads the user's config and the project's: dir's sbox.yaml,
// or the -config file, which must exist.
func (h devHost) loadConfig(dir string) (config, error) {
	project := filepath.Join(dir, "sbox.yaml")
	if h.projectConfig != "" {
		if _, err := os.Stat(h.projectConfig); err != nil {
			return config{}, fmt.Errorf("-config: %w", err)
		}
		project = h.projectConfig
	}
	return loadConfig(h.userConfig, project)
}

// devConfig loads the project's config and builds the session's job: the
// secrets read from the Mac's environment, the mounts' host paths found.
// Either missing is an error here, before anything starts.
func (h devHost) devConfig(dir string) (config, guestJob, error) {
	cfg, err := h.loadConfig(dir)
	if err != nil {
		return config{}, guestJob{}, err
	}
	lookup := h.lookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	secrets, err := cfg.secretValues(lookup)
	if err != nil {
		return config{}, guestJob{}, err
	}
	home := h.home
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	srcs, err := cfg.mountSources(home)
	if err != nil {
		return config{}, guestJob{}, err
	}
	job := devJob(cfg, secrets)
	for i, src := range srcs {
		job.mounts = append(job.mounts, mountCopy{src: src, guest: cfg.Mounts[i].Guest})
	}
	return cfg, job, nil
}

// bootRecord is the config a dev session booted with, kept on the Mac
// next to the sync base.
type bootRecord struct {
	ID     string            `json:"id"`
	Canary string            `json:"canary,omitempty"` // the fake credentials' marker, for -trace's rules
	Keys   map[string]string `json:"keys"`
}

// bootCanary is session id's canary, or "" when sbox dev didn't record it
// (the canary rules then stay quiet).
func bootCanary(stateDir, project, id string) string {
	var rec bootRecord
	data, err := os.ReadFile(bootFile(stateDir, project))
	if err != nil || json.Unmarshal(data, &rec) != nil || rec.ID != id {
		return ""
	}
	return rec.Canary
}

func bootFile(stateDir, project string) string {
	return filepath.Join(stateDir, "projects", project, "session.json")
}

// recordBoot remembers what session id booted with.
func recordBoot(stateDir, project, id, canary string, cfg config) error {
	data, err := json.Marshal(bootRecord{ID: id, Canary: canary, Keys: cfg.bootKeys()})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(bootFile(stateDir, project)), 0o755); err != nil {
		return err
	}
	return os.WriteFile(bootFile(stateDir, project), data, 0o644)
}

// configChanged lists the boot keys that differ between what session id
// booted with and cfg; none when sbox dev didn't record that session.
func configChanged(stateDir, project, id string, cfg config) []string {
	var rec bootRecord
	data, err := os.ReadFile(bootFile(stateDir, project))
	if err != nil || json.Unmarshal(data, &rec) != nil || rec.ID != id {
		return nil
	}
	return changedKeys(rec.Keys, cfg.bootKeys())
}

// warnConfigChanged says so when session id booted with another config
// than dir's now, and how to apply it.
func warnConfigChanged(stderr io.Writer, stateDir, project, id, dir string, cfg config) {
	warnChanged(stderr, id, dir, configChanged(stateDir, project, id, cfg))
}

// warnChanged names the changed keys only a restart applies.
func warnChanged(stderr io.Writer, id, dir string, changed []string) {
	if len(changed) > 0 {
		_, _ = fmt.Fprintf(stderr, "sbox: the config changed since session %s started (%s): sbox dev -restart %s applies it; the home is kept, running processes aren't\n",
			id, strings.Join(changed, ", "), dir)
	}
}

// _claudeKeys are the boot keys that end up in Claude Code's files, which
// it reads when it starts: rewriting them applies to the next claude run.
var _claudeKeys = []string{"dev.permissions", "mcp", "permissions"}

// applyLive applies the changed keys a running session can take without a
// reboot (Claude Code's settings, the mounts) and returns them, sorted,
// and the rest.
func applyLive(ctx context.Context, vm sboxRunFunc, id string, cfg config, job guestJob, changed []string, stderr io.Writer) (applied, rest []string) {
	claude := slices.ContainsFunc(changed, func(k string) bool { return slices.Contains(_claudeKeys, k) })
	if claude {
		err := vm(ctx, bytes.NewReader(cfg.claudeSettings()), io.Discard, stderr, "attach", id, "--", "python3", "-I", "/drive/sbox/claude.py", "-", "/home/runner")
		claude = err == nil
	}
	mounts := slices.Contains(changed, "mounts")
	if mounts {
		mounts = copyMounts(ctx, vm, id, job.mounts, stderr) == nil
	}
	for _, k := range changed {
		if (claude && slices.Contains(_claudeKeys, k)) || (mounts && k == "mounts") {
			applied = append(applied, k)
		} else {
			rest = append(rest, k)
		}
	}
	return applied, rest
}

// copyMounts copies the mounts into a running guest's home as the runner,
// replacing what's there, as the guest init does at boot.
func copyMounts(ctx context.Context, vm sboxRunFunc, id string, mounts []mountCopy, stderr io.Writer) error {
	if len(mounts) == 0 {
		return nil
	}
	var tarball bytes.Buffer
	tw := tar.NewWriter(&tarball)
	var rels []string
	total := int64(0)
	for _, m := range mounts {
		rel := strings.TrimPrefix(m.guest, "/home/runner/")
		rels = append(rels, rel)
		if err := packTree(tw, m.src, rel, nil, &total); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	script := "cd /home/runner && rm -rf -- " + shellJoin(rels) + " && tar -xf -"
	return vm(ctx, &tarball, io.Discard, stderr, "attach", id, "--", "sh", "-c", script)
}

// markApplied records that session id now has cfg's values for keys.
func markApplied(stateDir, project, id string, cfg config, keys []string) error {
	var rec bootRecord
	data, err := os.ReadFile(bootFile(stateDir, project))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &rec); err != nil || rec.ID != id {
		return fmt.Errorf("no record of session %s", id)
	}
	now := cfg.bootKeys()
	for _, k := range keys {
		rec.Keys[k] = now[k]
	}
	if data, err = json.Marshal(rec); err != nil {
		return err
	}
	return os.WriteFile(bootFile(stateDir, project), data, 0o644)
}

// devJob is what a dev session runs: nothing but the guest's sshd, with
// the config's network and environment, and the secrets' values.
func devJob(cfg config, secrets map[string]string) guestJob {
	return guestJob{command: []string{"sleep", "infinity"}, ports: cfg.Ports, services: cfg.Services, egress: cfg.Egress, env: cfg.Dev.Env, secrets: secrets,
		vcpus: cfg.Dev.VCPUs, memoryMiB: cfg.Dev.MemoryMiB, claude: cfg.claudeSettings(), ignore: copyRules(cfg.Dev.Ignore), pauseAfter: cfg.Dev.PauseAfter}
}

// runHooks runs the config's setup and start steps in session id (setup
// only, and regardless of its stamp, when force is set), with their output
// on stdout and stderr, and returns their exit status.
func runHooks(ctx context.Context, id string, cfg config, force bool, stdout, stderr io.Writer, vm sboxRunFunc) int {
	start := cfg.Dev.Start
	if force {
		start = nil
	}
	if len(cfg.Dev.Setup)+len(start) == 0 {
		return exitOK
	}
	script := hookScript(cfg.Dev.Setup, start, cfg.setupTimeout(), force)
	return attachMain(ctx, []string{id, "--", "sh", "-c", script}, strings.NewReader(""), stdout, stderr, vm)
}

// collectSummary asks the session what it saw (the helpers' events and the
// guest's listening ports) and returns sessionSummary's lines; a session
// that can't answer gets just the first line.
func collectSummary(ctx context.Context, id, dir string, started time.Time, unsynced int, cfg config, vm sboxRunFunc) string {
	var events, ss bytes.Buffer
	_ = vm(ctx, nil, &events, io.Discard, "events", id)
	_ = vm(ctx, strings.NewReader(""), &ss, io.Discard, "attach", id, "--", "ss", "-Hltn")
	blocked := append(portLines(ss.String(), cfg.Ports), toolDenials(ctx, vm, id, started)...)
	return sessionSummary(time.Since(started), dir, unsynced, parseEvents(events.String()), blocked)
}

// liveSession finds the project's running or paused session.
func liveSession(ctx context.Context, project string, stderr io.Writer, vm sboxRunFunc) (session, bool, int) {
	var ps bytes.Buffer
	if err := vm(ctx, nil, &ps, stderr, "ps"); err != nil {
		return session{}, false, vmFailed(stderr, err)
	}
	for _, s := range parseSessions(ps.String()) {
		if s.Project == project && (s.State == "running" || s.State == "paused") {
			return s, true, exitOK
		}
	}
	return session{}, false, exitOK
}

// attachExit turns an sbox-run call's error into an exit code: the
// remote command's own status when it has one.
func attachExit(stderr io.Writer, err error) int {
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return exitOK
	case errors.As(err, &exitErr) && exitErr.ExitCode() > 0:
		return exitErr.ExitCode()
	}
	return vmFailed(stderr, err)
}

// vmFailed reports a failed sbox-run call. When sbox-run itself exited
// non-zero it has already said why; anything else (no limactl, no VM) is
// printed here.
func vmFailed(stderr io.Writer, err error) int {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		_, _ = fmt.Fprintln(stderr, "sbox:", err)
	}
	return exitFail
}

// session is a microVM started with sbox start, as sbox-run ps reports it.
type session struct {
	ID      string
	State   string // running, paused or exited
	Started time.Time
	Project string // set for sbox dev sessions
}

// parseSessions reads sbox-run ps lines ("<id> <state> <unix time>
// [project]"). Malformed lines are skipped.
func parseSessions(out string) []session {
	var list []session
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 3 && len(f) != 4 {
			continue
		}
		secs, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil {
			continue
		}
		s := session{ID: f[0], State: f[1], Started: time.Unix(secs, 0)}
		if len(f) == 4 {
			s.Project = f[3]
		}
		list = append(list, s)
	}
	return list
}

// writeSessions prints the sessions as a table, with their age at now.
func writeSessions(w io.Writer, list []session, now time.Time) {
	if len(list) == 0 {
		_, _ = fmt.Fprintln(w, "no sessions")
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-16s  %-7s  %s\n", "ID", "STATE", "AGE")
	for _, s := range list {
		fmt.Fprintf(&b, "%-16s  %-7s  %s\n", printable(s.ID), printable(s.State), age(now.Sub(s.Started)))
	}
	_, _ = io.WriteString(w, b.String())
}

// age shortens a duration to its two largest units: 42s, 2h5m, 2d2h.
func age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
}

// askKeep asks on out whether to keep a session running at detach, and
// reads the answer from in. Keeping is the default when the guest has
// changes not yet synced, or when that's unknown (unsynced < 0); with
// nothing to sync, stopping is.
func askKeep(in io.Reader, out io.Writer) func(id string, unsynced int) bool {
	return func(id string, unsynced int) bool {
		choices := "[Y/n]"
		switch {
		case unsynced > 0:
			_, _ = fmt.Fprintf(out, "sbox: the guest has %d %s not synced; ", unsynced, plural(unsynced, "change", "changes"))
		case unsynced == 0:
			choices = "[y/N]"
			_, _ = fmt.Fprint(out, "sbox: ")
		default:
			_, _ = fmt.Fprint(out, "sbox: ")
		}
		_, _ = fmt.Fprintf(out, "keep session %s running? %s ", id, choices)
		answer, _ := bufio.NewReader(in).ReadString('\n')
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer == "" {
			return unsynced != 0
		}
		return strings.HasPrefix(answer, "y")
	}
}

// isTerminal reports whether f is a terminal: it answers the get-terminal-
// attributes ioctl. (A character device isn't enough: /dev/null is one.)
func isTerminal(f *os.File) bool {
	var attrs syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), _ioctlGetTermios, uintptr(unsafe.Pointer(&attrs)))
	return errno == 0
}
