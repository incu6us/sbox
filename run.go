package main

import (
	"archive/tar"
	"bufio"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

var (
	//go:embed scripts/guest-init.sh
	_guestInit string
	//go:embed scripts/sbox-claude.py
	_claudeScript string
)

const (
	_maxRunBytes = 256 << 20 // ponytail: whole skill in one tar; raise if real skills need more

	exitTimeout = 124 // like timeout(1)
	exitInfra   = 125 // like docker run: sbox itself failed
)

// runResult is what one microVM run produced.
type runResult struct {
	started  bool
	exitCode int // the command's, valid when started && !timedOut
	timedOut bool
	markers  []string // other nonce-marked lines from the guest and VM side, prefix removed
	nonce    string
	canary   string // marker in the run's fake credentials
	id       string // the run's, for sbox-run out
}

// dnsName is a hostname looked up during a run, with the record types asked for.
type dnsName struct {
	name  string
	types []string
}

func (d dnsName) String() string { return d.name + " (" + strings.Join(d.types, ", ") + ")" }

// addDNS records a lookup, merging repeat lookups of the same name.
func addDNS(list []dnsName, qtype, name string) []dnsName {
	for i := range list {
		if list[i].name == name {
			if !slices.Contains(list[i].types, qtype) {
				list[i].types = append(list[i].types, qtype)
			}
			return list
		}
	}
	return append(list, dnsName{name: name, types: []string{qtype}})
}

// egressLog counts the main microVM's proxy log ("EGRESS ALLOW|DENY target").
func egressLog(markers []string) (allowed, refused []string) {
	counts := make(map[string]int)
	var order []string
	for _, m := range markers {
		rest, ok := strings.CutPrefix(m, "EGRESS ")
		if !ok {
			continue
		}
		if counts[rest] == 0 {
			order = append(order, rest)
		}
		counts[rest]++
	}
	for _, key := range order {
		verdict, target, _ := strings.Cut(key, " ")
		item := fmt.Sprintf("%s ×%d", printable(target), counts[key])
		if verdict == "ALLOW" {
			allowed = append(allowed, item)
		} else {
			refused = append(refused, item)
		}
	}
	return allowed, refused
}

func listOrNone(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	return strings.Join(items, ", ")
}

// dnsLookups collects the DNS logger's lines ("DNS <type> <name>").
func dnsLookups(markers []string) []dnsName {
	var list []dnsName
	for _, m := range markers {
		if rest, ok := strings.CutPrefix(m, "DNS "); ok {
			qtype, name, _ := strings.Cut(rest, " ")
			list = addDNS(list, qtype, name)
		}
	}
	return list
}

// guestJob is what the guest runs: a command as the runner, or a scan
// script as root (it drops to the runner for what it runs), with input
// files for it under sbox/in/.
type guestJob struct {
	command    []string
	scan       string
	files      map[string]string
	prefetch   string            // optional: runs first, in a microVM with registry-only egress
	preLimit   time.Duration     // the prefetch microVM's timeout
	registries []string          // hosts the prefetch proxy allows besides the public registries
	out        string            // optional: a script whose tar of the work folder comes back after the command
	egress     []string          // optional: host:port the main microVM may reach through the proxy
	ports      []int             // guest ports forwarded to the VM's localhost (and so the Mac's)
	services   []int             // Mac ports the guest reaches at host.sbox
	env        map[string]string // set in every ssh login to the guest
	secrets    map[string]string // the same, but root-only on the drive and never on the kept home
	vcpus      int               // the main microVM's size; 0 keeps sbox-run's default
	memoryMiB  int
	claude     []byte      // Claude Code settings for scripts/sbox-claude.py to merge in the guest
	mounts     []mountCopy // Mac files and folders the guest init copies into the home
	ignore     ignoreRules // what the project copy leaves out (sbox dev); everything when nil
	// pauseAfter pauses a session when nothing has been connected to the
	// guest (a terminal, a forwarded port) for this long; 0 never.
	pauseAfter time.Duration
	canary     string // the fake credentials' marker; a random one when empty
}

// mountCopy is one mount: a resolved Mac path and where it goes.
type mountCopy struct{ src, guest string }

// runMain implements `sbox run [-config file] [-v] <dir> -- <command...>`,
// with egress, the timeout and the microVM's size from the config.
// It exits with the command's status, 124 on timeout, 125 when sbox fails.
func runMain(ctx context.Context, args []string, stdout, stderr io.Writer, userConfig string) int {
	flags := flag.NewFlagSet("sbox run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configFile := flags.String("config", "", "a config `file` read after ~/.config/sbox/config.yaml (egress, timeouts.run, vm)")
	verbose := flags.Bool("v", false, "also show kernel and Firecracker output")
	apply := flags.Bool("apply", false, "show what the command changed in the folder, then write it there once you confirm")
	patchFile := flags.String("patch", "", "write what the command changed in the folder to this `file`, as a patch")
	flags.Usage = func() {
		_, _ = fmt.Fprint(stderr, "usage: sbox run [-config <file>] [-v] [-apply | -patch <file>] <dir | GitHub URL> -- <command> [args...]\n\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return exitInfra
	}
	rest := flags.Args()
	if len(rest) < 3 || rest[1] != "--" || *apply && *patchFile != "" {
		flags.Usage()
		return exitInfra
	}
	cfg, err := toolConfig(userConfig, *configFile)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox:", err)
		return exitInfra
	}
	job, timeout := cfg.runJob(rest[2:]), cfg.runTimeout()
	if job.secrets, err = cfg.secretValues(os.LookupEnv); err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox:", err)
		return exitInfra
	}

	dir, name, err := resolveTarget(rest[0], githubFetch(ctx))
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox:", err)
		return exitInfra
	}
	if name != dir {
		_, _ = fmt.Fprintln(stderr, "sbox: running on", name)
	}
	var rules ignoreRules
	var base manifest
	if *apply || *patchFile != "" {
		if *apply && name != dir {
			_, _ = fmt.Fprintln(stderr, "sbox: -apply writes into a folder of yours, not a GitHub download; -patch <file> works")
			return exitInfra
		}
		rules = syncRules(gitignoreRules(dir))
		if base, err = readManifest(dir, rules); err != nil {
			_, _ = fmt.Fprintln(stderr, "sbox:", err)
			return exitInfra
		}
		job.out = pullScript(rules)
	}
	start := time.Now()
	res, err := runVM(ctx, dir, job, timeout, *verbose, stdout, stderr)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox:", err)
		return exitInfra
	}
	elapsed := time.Since(start).Round(100 * time.Millisecond)
	if allowed, refused := egressLog(res.markers); len(allowed)+len(refused) > 0 {
		_, _ = fmt.Fprintf(stderr, "sbox: egress allowed: %s; refused: %s\n", listOrNone(allowed), listOrNone(refused))
	}
	if names := dnsLookups(res.markers); len(names) > 0 {
		list := make([]string, len(names))
		for i, n := range names {
			list[i] = printable(n.String())
		}
		_, _ = fmt.Fprintf(stderr, "sbox: DNS lookups (answered NXDOMAIN): %s\n", strings.Join(list, ", "))
	}
	switch {
	case res.timedOut:
		_, _ = fmt.Fprintf(stderr, "sbox: timed out after %s (timeouts.run)\n", timeout)
		return exitTimeout
	case !res.started:
		_, _ = fmt.Fprintln(stderr, "sbox: the microVM stopped before the command started (-v shows why)")
		return exitInfra
	}
	_, _ = fmt.Fprintf(stderr, "sbox: exit %d in %s\n", res.exitCode, elapsed)
	if job.out != "" {
		var confirm func() bool
		if isTerminal(os.Stdin) {
			confirm = func() bool {
				_, _ = fmt.Fprintf(stderr, "sbox: write these changes into %s? [y/N] ", dir)
				answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				return strings.HasPrefix(strings.ToLower(strings.TrimSpace(answer)), "y")
			}
		}
		if code := runChanges(ctx, sboxRun, res.id, dir, base, rules, *apply, *patchFile, confirm, stdout, stderr); code != exitOK {
			return exitInfra
		}
	}
	return res.exitCode
}

// runVM sends the directory and job into a fresh microVM and streams the
// guest's output to out, the VM side's diagnostics to errOut. They must be
// separate writers: exec copies errOut from its own goroutine.
func runVM(ctx context.Context, dir string, job guestJob, timeout time.Duration, verbose bool, out, errOut io.Writer) (runResult, error) {
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return runResult{}, fmt.Errorf("%s: not a directory", dir)
	}
	id, err := randomHex(8)
	if err != nil {
		return runResult{}, err
	}
	nonce, err := randomHex(16)
	if err != nil {
		return runResult{}, err
	}
	canary, err := randomHex(6)
	if err != nil {
		return runResult{}, err
	}

	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeRunTar(pw, dir, job, nonce, canary)) }()

	// The VM side enforces the timeout; the host one only catches a hung VM.
	ctx, cancel := context.WithTimeout(ctx, timeout+job.preLimit+60*time.Second)
	defer cancel()
	args := []string{"shell", "--workdir", "/", _vmName, "sudo", _runScriptPath, id, strconv.Itoa(int(timeout.Seconds()))}
	if job.prefetch != "" {
		args = append(args, strconv.Itoa(int(job.preLimit.Seconds())))
	}
	cmd := exec.CommandContext(ctx, limactlBin(), args...)
	cmd.Stdin = pr
	cmd.Stderr = errOut
	console, err := cmd.StdoutPipe()
	if err != nil {
		return runResult{}, err
	}
	if err := cmd.Start(); err != nil {
		return runResult{}, fmt.Errorf("limactl: %w", err)
	}

	res := streamConsole(console, out, verbose, nonce, redactor(job.secrets))
	res.nonce, res.canary, res.id = nonce, canary, id
	err = cmd.Wait()
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr) && exitErr.ExitCode() == exitTimeout:
		res.timedOut = true
	case err != nil:
		return res, fmt.Errorf("sbox-run: %w", err)
	}
	return res, nil
}

// streamConsole prints the guest's output and picks up the init's markers,
// which only count with the run's nonce. Kernel and Firecracker lines are
// noise unless verbose.
// redactor replaces each secret, its hex in either case and its base64
// forms (standard and URL-safe, padded or not) with [redacted:NAME], as
// sbox-redact.py does in the VM; values under 8 characters would match
// noise and stay.
func redactor(secrets map[string]string) func(string) string {
	type form struct {
		re    *regexp.Regexp
		label string
	}
	var forms []form
	for _, name := range slices.Sorted(maps.Keys(secrets)) {
		v := secrets[name]
		if len(v) < 8 {
			continue
		}
		alts := []string{v}
		for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding} {
			e := enc.EncodeToString([]byte(v))
			alts = append(alts, e, strings.TrimRight(e, "="))
		}
		slices.SortFunc(alts, func(a, b string) int { return len(b) - len(a) })
		for i := range alts {
			alts[i] = regexp.QuoteMeta(alts[i])
		}
		label := "[redacted:" + name + "]"
		forms = append(forms, form{regexp.MustCompile(strings.Join(alts, "|")), label},
			form{regexp.MustCompile("(?i)" + hex.EncodeToString([]byte(v))), label})
	}
	return func(s string) string {
		for _, f := range forms {
			s = f.re.ReplaceAllLiteralString(s, f.label)
		}
		return s
	}
}

func streamConsole(r io.Reader, out io.Writer, verbose bool, nonce string, redact func(string) string) runResult {
	var (
		res     runResult
		blank   bool // held back: the guest prints one before each marker
		mark    = "SBOX-" + nonce + " "
		start   = mark + "START"
		exitPfx = mark + "EXIT "
	)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if redact != nil {
			line = redact(line)
		}
		if blank && !strings.HasPrefix(line, mark) {
			_, _ = fmt.Fprintln(out, "│")
		}
		blank = false
		switch {
		case line == start:
			res.started = true
		case strings.HasPrefix(line, exitPfx):
			if code, err := strconv.Atoi(strings.TrimPrefix(line, exitPfx)); err == nil {
				res.exitCode = code
			}
		case strings.HasPrefix(line, mark):
			res.markers = append(res.markers, strings.TrimPrefix(line, mark))
		case res.started && line == "":
			blank = true
		case res.started && !isNoise(line):
			_, _ = fmt.Fprintln(out, "│ "+printable(line))
		case verbose:
			_, _ = fmt.Fprintln(out, "· "+printable(line))
		}
	}
	return res
}

// isNoise spots Firecracker's own log lines and kernel messages.
func isNoise(line string) bool {
	return _kernelLine.MatchString(line) || (len(line) > 4 && line[4] == '-' && strings.Contains(line, "[") &&
		strings.HasPrefix(line, "20"))
}

// _kernelLine is a kernel message's "[    0.512345] " timestamp.
var _kernelLine = regexp.MustCompile(`^\[\s*\d+\.\d+\]`)

// printable escapes control characters, so guest output can't drive the
// user's terminal.
func printable(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '\t' || unicode.IsPrint(r) {
			b.WriteRune(r)
		} else {
			fmt.Fprintf(&b, `\x%02x`, r)
		}
	}
	return b.String()
}

// packTree adds src (a folder, or a single file) to tw under prefix:
// regular files, folders and symlinks as links; sockets, devices, pipes and
// what skip ignores are left out. total counts file bytes across calls,
// capped at _maxRunBytes.
func packTree(tw *tar.Writer, src, prefix string, skip ignoreRules, total *int64) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		} else if !info.Mode().IsRegular() && !info.IsDir() {
			return nil // sockets, devices, pipes
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel != "." && skip.skip(filepath.ToSlash(rel)) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(filepath.Join(prefix, rel))
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if *total += info.Size(); *total > _maxRunBytes {
			return fmt.Errorf("%s: larger than %d MiB", src, _maxRunBytes>>20)
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = io.Copy(tw, f)
		return err
	})
}

// writeRunTar packs what the guest gets: skill/ (the directory, symlinks
// kept as links), sbox/init.sh, sbox/nonce, sbox/canary and sbox/scan.sh
// for root only, sbox/cmd.sh and sbox/in/* for the runner.
func writeRunTar(w io.Writer, dir string, job guestJob, nonce, canary string) error {
	tw := tar.NewWriter(w)
	total := int64(0)
	err := packTree(tw, dir, "skill", job.ignore, &total)
	var mountList strings.Builder
	for i, m := range job.mounts {
		if err == nil {
			err = packTree(tw, m.src, fmt.Sprintf("mounts/%d", i), nil, &total)
		}
		fmt.Fprintf(&mountList, "%d %s\n", i, m.guest)
	}
	if err != nil {
		return err
	}

	type file struct {
		name, body string
		mode       int64
	}
	files := []file{
		{"sbox/init.sh", _guestInit, 0o700},
		{"sbox/nonce", nonce, 0o400},
		{"sbox/canary", canary, 0o400},
	}
	if job.prefetch != "" {
		files = append(files, file{"sbox/prefetch.sh", job.prefetch, 0o700})
	}
	for name, vars := range map[string]map[string]string{"sbox/env": job.env, "sbox/secrets": job.secrets} {
		if len(vars) > 0 {
			var b strings.Builder
			for _, k := range slices.Sorted(maps.Keys(vars)) {
				fmt.Fprintf(&b, "%s=%s\n", k, vars[k])
			}
			mode := int64(0o644)
			if name == "sbox/secrets" {
				mode = 0o400
			}
			files = append(files, file{name, b.String(), mode})
		}
	}
	if len(job.mounts) > 0 {
		files = append(files, file{"sbox/mounts", mountList.String(), 0o644})
	}
	if job.claude != nil {
		files = append(files, file{"sbox/claude.json", string(job.claude), 0o644}, file{"sbox/claude.py", _claudeScript, 0o644})
	}
	if job.pauseAfter > 0 {
		files = append(files, file{"sbox/pause_after", fmt.Sprintf("%d\n", int(job.pauseAfter.Seconds())), 0o644})
	}
	if job.vcpus > 0 || job.memoryMiB > 0 {
		files = append(files, file{"sbox/vm", fmt.Sprintf("%d %d\n", job.vcpus, job.memoryMiB), 0o644})
	}
	if len(job.egress) > 0 {
		files = append(files, file{"sbox/egress", strings.Join(job.egress, "\n") + "\n", 0o644})
	}
	if job.out != "" {
		files = append(files, file{"sbox/out.sh", job.out, 0o700})
	}
	if len(job.registries) > 0 {
		files = append(files, file{"sbox/registries", strings.Join(job.registries, "\n") + "\n", 0o644})
	}
	for name, list := range map[string][]int{"sbox/ports": job.ports, "sbox/services": job.services} {
		if len(list) > 0 {
			var b strings.Builder
			for _, n := range list {
				fmt.Fprintf(&b, "%d\n", n)
			}
			files = append(files, file{name, b.String(), 0o644})
		}
	}
	if job.scan != "" {
		files = append(files, file{"sbox/scan.sh", job.scan, 0o700})
	} else {
		files = append(files, file{"sbox/cmd.sh", "#!/bin/sh\nexec " + shellJoin(job.command) + "\n", 0o755})
	}
	for _, name := range slices.Sorted(maps.Keys(job.files)) {
		files = append(files, file{"sbox/in/" + name, job.files[name], 0o644})
	}
	for _, f := range files {
		hdr := &tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := io.WriteString(tw, f.body); err != nil {
			return err
		}
	}
	return tw.Close()
}

// shellJoin quotes each argument for sh, so the command runs as given.
func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", errors.New("no randomness available")
	}
	return hex.EncodeToString(b), nil
}
