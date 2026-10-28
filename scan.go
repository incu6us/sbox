package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	_maxTraceLines = 3000 // per entry point, sent from the guest
	_maxListed     = 15   // per category in the report

	// _traced are the syscalls that show what an entry point did, plus
	// process creation, to rebuild the process tree.
	_traced = "execve,connect,openat,unlink,unlinkat,rename,renameat2,clone,clone3,fork,vfork"
	// _quietPaths are opened by every program; the guest drops them. /proc
	// is filtered file by file, so reads of another process's environ show.
	_quietPaths = `openat\([^"]*"(/usr/|/lib|/sys/|/dev/|/etc/ld\.so|/etc/localtime|/etc/ssl/openssl\.cnf|/home/runner/work/node_modules/|/tmp/node-compile-cache/` +
		`|/proc/(self|thread-self|[0-9]+)/(maps|stat|status|cgroup|mountinfo|limits|auxv|smaps|fd|task|exe|mounts)` +
		`|/proc/(meminfo|cpuinfo|stat|filesystems|sys/|loadavg|version|mounts))`

	scanBlock  = 2 // exit codes, as skills-audit: pass 0, review 1, block 2
	scanReview = 1
)

var (
	_tracePID     = regexp.MustCompile(`^\d+\s+`)
	_traceClone   = regexp.MustCompile(`^(?:clone3?|v?fork)\(.*\) = (\d+)$`)
	_traceExec    = regexp.MustCompile(`^execve\("((?:[^"\\]|\\.)*)", \[(.*?)\]`)
	_traceConnect = regexp.MustCompile(`^connect\(\d+, \{(.*)\}, \d+\) = (.*)$`)
	_traceOpen    = regexp.MustCompile(`^openat\([^,]+, "((?:[^"\\]|\\.)*)", ([A-Z_|]+).*\) = (.*)$`)
	_traceUnlink  = regexp.MustCompile(`^unlink(?:at)?\((?:[^,]+, )?"((?:[^"\\]|\\.)*)".*\) = (.*)$`)
	_traceRename  = regexp.MustCompile(`^rename(?:at2?)?\((?:[^,]+, )?"((?:[^"\\]|\\.)*)", (?:[^,]+, )?"((?:[^"\\]|\\.)*)".*\) = (.*)$`)
	_quoted       = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)
	_sockAddr     = regexp.MustCompile(`(?:sin6?_port=htons\((\d+)\))|(?:inet_addr\("([^"]+)"\))|(?:inet_pton\(AF_INET6, "([^"]+)")|(?:sun_path="([^"]+)")`)
)

// entryRun is what one entry point did, from its trace.
type entryRun struct {
	ep       entryPoint
	ran      bool
	exitCode int
	procs    []traceEvent
	network  []traceEvent
	files    []traceEvent
	dns      []dnsName
	hidden   int // events from the entry point's harness programs, not judged
}

// traceEvent is one thing an entry point did, from one strace line.
type traceEvent struct {
	kind   string   // proc, net, file
	op     string   // net: connect, dns; file: read, write, delete, rename
	path   string   // program, address or file
	target string   // rename destination
	argv   []string // proc
	result string   // "ok", an errno name, or "attempted" for a non-blocking connect
	child  int      // clone: the new process
	pid    int
	prog   string // the program the process was running (for proc: the one it started)
}

func (e traceEvent) String() string {
	switch e.kind {
	case "proc":
		return strings.Join(e.argv, " ")
	case "net":
		if e.op == "dns" { // the queried name travels in the packet, not the syscall
			return "DNS lookup via " + e.path + " (hostname not recorded)"
		}
		return e.path + " → " + e.result
	}
	s := e.op + " " + e.path
	if e.op == "rename" {
		s += " → " + e.target
	}
	if e.result != "ok" {
		s += " (" + e.result + ")"
	}
	return s
}

// scanMain implements `sbox scan [-config file] [-v] [-json] <dir>`: runs every
// automatic entry point of the skill in one microVM, each under strace,
// and reports what it did.
func scanMain(ctx context.Context, args []string, stdout, stderr io.Writer, userConfig string) int {
	flags := flag.NewFlagSet("sbox scan", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configFile := flags.String("config", "", "a config `file` read after ~/.config/sbox/config.yaml (timeouts.entry, prefetch, vm)")
	verbose := flags.Bool("v", false, "also show the entry points' own output")
	asJSON := flags.Bool("json", false, "print the report as JSON")
	refetch := flags.Bool("refetch", false, "download a GitHub target again, even a commit already cached")
	flags.Usage = func() {
		_, _ = fmt.Fprint(stderr, "usage: sbox scan [-config <file>] [-v] [-json] [-refetch] <dir | GitHub URL>\n\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return exitInfra
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return exitInfra
	}
	cfg, err := toolConfig(userConfig, *configFile)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox:", err)
		return exitInfra
	}
	perEntry, rules := cfg.entryTimeout(), cfg.scanRules()
	secrets, err := cfg.secretValues(os.LookupEnv)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox:", err)
		return exitInfra
	}
	secretNames := slices.Sorted(maps.Keys(secrets))
	jsonOut := *asJSON || cfg.Report.Format == "json"
	dir, name, where, err := resolveTarget(flags.Arg(0), githubFetch(ctx, *refetch))
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox:", err)
		return exitInfra
	}

	eps, err := findEntryPoints(dir)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox:", err)
		return exitInfra
	}
	if len(eps) == 0 {
		_, _ = fmt.Fprintln(stdout, "no automatic entry points (hooks, MCP servers, install scripts) found")
		return exitOK
	}

	var steps []prefetchStep
	if cfg.prefetchOn() {
		steps = planPrefetch(dir, eps)
	}
	job := guestJob{vcpus: cfg.VM.VCPUs, memoryMiB: cfg.VM.MemoryMiB, registries: cfg.Prefetch.Registries, secrets: secrets}
	if len(steps) > 0 {
		job.prefetch, job.preLimit = prefetchScript(steps), cfg.prefetchTimeout()
		if hasNPMDependencies(dir) {
			// The dependencies' install scripts run here, under strace. npm
			// itself is sbox's harness, not the skill's code: without a user
			// config it doesn't read ~/.npmrc (fake token and all), so any
			// read of it comes from a dependency.
			eps = append(eps, entryPoint{
				Kind: "npm", Name: "rebuild", Source: "package.json", Command: "npm rebuild --foreground-scripts",
				Env:     []string{"npm_config_userconfig=/dev/null"},
				Harness: []string{"npm"},
			})
		}
	}
	if !jsonOut {
		_, _ = fmt.Fprintf(stdout, "%d entry points in %s\n", len(eps), name)
		for _, o := range rules.overrides() {
			_, _ = fmt.Fprintf(stdout, "config: %s\n", o)
		}
		if len(secretNames) > 0 {
			_, _ = fmt.Fprintf(stdout, "config: real secrets %s (redacted in this report)\n", strings.Join(secretNames, ", "))
		}
	}
	script, files := scanScript(eps, perEntry)
	job.scan, job.files = script, files
	live := io.Discard
	if *verbose {
		live = stdout
	}
	vmTimeout := time.Duration(len(eps))*(perEntry+5*time.Second) + 30*time.Second
	res, err := runVM(ctx, dir, job, vmTimeout, false, live, stderr)
	switch {
	case err != nil:
		_, _ = fmt.Fprintln(stderr, "sbox:", err)
		return exitInfra
	case !res.started:
		_, _ = fmt.Fprintln(stderr, "sbox: the microVM stopped before the scan started (sbox run -v shows why)")
		return exitInfra
	case res.timedOut:
		_, _ = fmt.Fprintln(stderr, "sbox: the scan timed out; the report covers what finished")
	}

	runs := collectRuns(eps, res.markers, res.nonce)
	findings := make([][]finding, len(runs))
	var all []finding
	var pre *prefetchResult
	if len(steps) > 0 {
		pre = collectPrefetch(steps, res.markers)
		pre.Findings = rules.apply(pre.Findings)
		all = append(all, pre.Findings...)
	}
	for i, r := range runs {
		findings[i] = rules.apply(analyze(r, res.canary, rules.credentialPaths))
		all = append(all, findings[i]...)
	}
	verdict := rules.verdict(all)
	if jsonOut {
		err = writeScanJSON(stdout, where, pre, runs, findings, verdict, all, rules.overrides(), secretNames)
	} else {
		writeScanReport(stdout, pre, runs, findings, verdict, all, cmp.Or(cfg.Report.MaxListed, _maxListed))
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox:", err)
		return exitInfra
	}
	return scanExit(verdict, cfg.Report.FailOn)
}

// scanScript is the guest's scan: each entry point runs as the runner under
// strace, from the skill's root, with a clean environment and its input on
// stdin. The traces go to a root-only directory, so an entry point can't
// edit the record of what it did, and every runner process is killed before
// the next entry point starts.
func scanScript(eps []entryPoint, perEntry time.Duration) (string, map[string]string) {
	secs := int(perEntry.Seconds())
	files := make(map[string]string)
	var b strings.Builder
	b.WriteString("#!/bin/sh\n# Generated by sbox scan; runs as root. $nonce comes from the init.\nmkdir -m 0700 -p /run/sbox\n")
	// The config's secrets, NAME=value lines, become env -i's last
	// assignments, so a real value replaces the fake one of the same name.
	b.WriteString("set --\n[ -f /drive/sbox/secrets ] && while IFS= read -r kv; do set -- \"$@\" \"$kv\"; done </drive/sbox/secrets\n")
	for i, ep := range eps {
		n := i + 1
		stdin := "/dev/null"
		if ep.Stdin != "" {
			name := fmt.Sprintf("stdin.%d", n)
			files[name] = ep.Stdin
			stdin = "/drive/sbox/in/" + name
		}
		cmd := ep.Command
		if ep.Kind == "mcp" { // keep stdin open after the handshake, so the server has time to answer
			cmd = fmt.Sprintf("(cat %s; sleep %d) | %s", stdin, max(secs-2, 1), ep.Command)
			stdin = "/dev/null"
		}
		fmt.Fprintf(&b, `echo "SBOX-$nonce ENTRY %d"
getent hosts "entry-%d-$nonce.sbox.invalid" >/dev/null 2>&1 # DNS log boundary
env -i HOME=/home/runner USER=runner LOGNAME=runner PATH=/usr/local/bin:/usr/bin:/bin \
	CLAUDE_PLUGIN_ROOT=/home/runner/work CLAUDE_PROJECT_DIR=/home/runner/work CURSOR_PROJECT_DIR=/home/runner/work \
	ANTHROPIC_API_KEY="sk-ant-api03-sbox${canary}Rt5Yu6" GITHUB_TOKEN="ghp_sbox${canary}Mn3Bv4Cx5" \
	AWS_ACCESS_KEY_ID="AKIASBOX$(echo "$canary" | tr a-f A-F)" AWS_SECRET_ACCESS_KEY="sbox${canary}Kf9wQ2xLr7Tz4Vb1Nm8Pc3Hd6Js5" \
	npm_config_offline=true UV_OFFLINE=1 UV_PYTHON_DOWNLOADS=never \
	npm_config_update_notifier=false npm_config_audit=false npm_config_fund=false NODE_DISABLE_COMPILE_CACHE=1 %s"$@" \
	timeout -k 2 %d strace -ff -qq -s 200 -u runner -e trace=%s -o /run/sbox/trace.%d sh -c %s <%s 2>&1
status=$?
pkill -KILL -u runner 2>/dev/null
echo
echo "SBOX-$nonce RESULT %d $status"
for f in /run/sbox/trace.%d.*; do
	[ -f "$f" ] || continue
	grep -avE '%s' "$f" | sed "s/^/SBOX-$nonce TRACE %d ${f##*.} /"
done | head -n %d
`, n, n, envAssignments(ep.Env), secs, _traced, n, shellJoin([]string{cmd}), stdin, n, n, _quietPaths, n, _maxTraceLines)
	}
	return b.String(), files
}

// envAssignments quotes extra NAME=value settings for the env -i line.
func envAssignments(env []string) string {
	var b strings.Builder
	for _, e := range env {
		b.WriteString(shellJoin([]string{e}) + " ")
	}
	return b.String()
}

// collectRuns matches the guest's RESULT and TRACE markers to entry points,
// and the DNS log's lines to the entry point that ran after the last
// sentinel lookup. Sentinels carry the nonce, so entry points can't fake them.
func collectRuns(eps []entryPoint, markers []string, nonce string) []entryRun {
	runs := make([]entryRun, len(eps))
	for i, ep := range eps {
		runs[i].ep = ep
	}
	sentinel := regexp.MustCompile(`^entry-(\d+)-` + regexp.QuoteMeta(nonce) + `\.sbox\.invalid$`)
	current := 0
	traces := make([]processTraces, len(runs))
	for _, m := range markers {
		if rest, ok := strings.CutPrefix(m, "DNS "); ok {
			qtype, name, _ := strings.Cut(rest, " ")
			if s := sentinel.FindStringSubmatch(name); s != nil {
				current, _ = strconv.Atoi(s[1])
			} else if current >= 1 && current <= len(runs) {
				runs[current-1].dns = addDNS(runs[current-1].dns, qtype, name)
			}
			continue
		}
		kind, rest, _ := strings.Cut(m, " ")
		num, rest, _ := strings.Cut(rest, " ")
		n, err := strconv.Atoi(num)
		if err != nil || n < 1 || n > len(runs) {
			continue
		}
		r := &runs[n-1]
		switch kind {
		case "RESULT":
			r.ran = true
			r.exitCode, _ = strconv.Atoi(rest)
		case "TRACE":
			pidText, line, _ := strings.Cut(rest, " ")
			if pid, err := strconv.Atoi(pidText); err == nil {
				traces[n-1].add(pid, line)
			}
		}
	}
	for i := range runs {
		attribute(&runs[i], traces[i])
	}
	return runs
}

// processTraces holds one entry point's trace lines per process, in order.
type processTraces struct {
	lines map[int][]string
	order []int // processes in the order their lines arrived
}

func (t *processTraces) add(pid int, line string) {
	if t.lines == nil {
		t.lines = make(map[int][]string)
	}
	if _, ok := t.lines[pid]; !ok {
		t.order = append(t.order, pid)
	}
	t.lines[pid] = append(t.lines[pid], line)
}

// attribute rebuilds the process tree from clone and execve, gives every
// event the program its process was running (inherited from the parent at
// clone time until the process execs), and keeps the events that aren't
// from the entry point's harness programs. A shell the harness spawns, and
// everything under it, is judged.
func attribute(r *entryRun, t processTraces) {
	events := make(map[int][]traceEvent, len(t.lines))
	parent := make(map[int]int)
	for _, pid := range t.order {
		for _, line := range t.lines[pid] {
			ev, ok := parseTrace(line)
			if !ok {
				continue
			}
			ev.pid = pid
			events[pid] = append(events[pid], ev)
			if ev.kind == "clone" {
				parent[ev.child] = pid
			}
		}
	}

	start := make(map[int]string)
	resolving := make(map[int]bool)
	var programAtStart func(pid int) string
	programAtStart = func(pid int) string {
		if prog, ok := start[pid]; ok {
			return prog
		}
		p, ok := parent[pid]
		if !ok || resolving[pid] {
			return ""
		}
		resolving[pid] = true
		prog := programAtStart(p)
		for _, ev := range events[p] {
			if ev.kind == "proc" {
				prog = programName(ev)
			}
			if ev.kind == "clone" && ev.child == pid {
				break
			}
		}
		start[pid] = prog
		return prog
	}

	seen := make(map[string]bool)
	for _, pid := range t.order {
		prog := programAtStart(pid)
		for _, ev := range events[pid] {
			if ev.kind == "proc" {
				prog = programName(ev)
			}
			ev.prog = prog
			if ev.kind == "clone" {
				continue
			}
			if slices.Contains(r.ep.Harness, prog) {
				r.hidden++
				continue
			}
			key := ev.kind + " " + ev.String()
			if seen[key] {
				continue
			}
			seen[key] = true
			switch ev.kind {
			case "proc":
				r.procs = append(r.procs, ev)
			case "net":
				r.network = append(r.network, ev)
			case "file":
				r.files = append(r.files, ev)
			}
		}
	}
}

// _interpreters run a script named by their first argument; the script is
// the program that matters ("node /usr/local/bin/npm" is npm).
var _interpreters = map[string]bool{"node": true, "nodejs": true, "python": true, "python3": true, "python3.12": true}

func programName(ev traceEvent) string {
	name := path.Base(ev.path)
	if _interpreters[name] && len(ev.argv) > 1 && !strings.HasPrefix(ev.argv[1], "-") {
		return path.Base(ev.argv[1])
	}
	return name
}

// parseTrace turns one strace line into an event. Failed execve calls are
// PATH lookups, not processes.
func parseTrace(line string) (traceEvent, bool) {
	line = _tracePID.ReplaceAllString(line, "")
	if m := _traceClone.FindStringSubmatch(line); m != nil {
		child, _ := strconv.Atoi(m[1])
		return traceEvent{kind: "clone", child: child}, true
	}
	if m := _traceExec.FindStringSubmatch(line); m != nil {
		if !strings.HasSuffix(line, "= 0") {
			return traceEvent{}, false
		}
		var argv []string
		for _, a := range _quoted.FindAllStringSubmatch(m[2], -1) {
			argv = append(argv, unescape(a[1]))
		}
		return traceEvent{kind: "proc", path: unescape(m[1]), argv: argv}, true
	}
	if m := _traceConnect.FindStringSubmatch(line); m != nil {
		addr := sockAddr(m[1])
		switch {
		case addr == "unix:/var/run/nscd/socket": // glibc probes for nscd on every lookup
			return traceEvent{}, false
		case strings.HasSuffix(addr, ":53"):
			return traceEvent{kind: "net", op: "dns", path: addr, result: outcome(m[2])}, true
		}
		return traceEvent{kind: "net", op: "connect", path: addr, result: outcome(m[2])}, true
	}
	if m := _traceOpen.FindStringSubmatch(line); m != nil {
		op := "read"
		if strings.Contains(m[2], "O_WRONLY") || strings.Contains(m[2], "O_RDWR") || strings.Contains(m[2], "O_CREAT") {
			op = "write"
		}
		return traceEvent{kind: "file", op: op, path: unescape(m[1]), result: outcome(m[3])}, true
	}
	if m := _traceUnlink.FindStringSubmatch(line); m != nil {
		return traceEvent{kind: "file", op: "delete", path: unescape(m[1]), result: outcome(m[2])}, true
	}
	if m := _traceRename.FindStringSubmatch(line); m != nil {
		return traceEvent{kind: "file", op: "rename", path: unescape(m[1]), target: unescape(m[2]), result: outcome(m[3])}, true
	}
	return traceEvent{}, false
}

// unescape reverses strace's C-style string escaping.
func unescape(s string) string {
	if u, err := strconv.Unquote(`"` + s + `"`); err == nil {
		return u
	}
	return s
}

// sockAddr renders strace's sockaddr as host:port or unix:path.
func sockAddr(s string) string {
	var port, host, path string
	for _, m := range _sockAddr.FindAllStringSubmatch(s, -1) {
		switch {
		case m[1] != "":
			port = m[1]
		case m[2] != "":
			host = m[2]
		case m[3] != "":
			host = "[" + m[3] + "]"
		case m[4] != "":
			path = m[4]
		}
	}
	switch {
	case path != "":
		return "unix:" + path
	case host != "":
		return host + ":" + port
	}
	return s
}

// outcome shortens a syscall result: "ok" or the errno name. A
// non-blocking connect reports EINPROGRESS before it goes nowhere.
func outcome(r string) string {
	if rest, ok := strings.CutPrefix(r, "-1 "); ok {
		name, _, _ := strings.Cut(rest, " ")
		if name == "EINPROGRESS" {
			return "attempted"
		}
		return name
	}
	return "ok"
}

func writeScanReport(w io.Writer, pre *prefetchResult, runs []entryRun, findings [][]finding, verdict string, all []finding, maxListed int) {
	var b strings.Builder
	if pre != nil {
		writePrefetchReport(&b, pre, maxListed)
	}
	for i, r := range runs {
		fmt.Fprintf(&b, "\n%d/%d  %s\n     $ %s\n", i+1, len(runs), r.ep, printable(r.ep.Command))
		if !r.ran {
			b.WriteString("     did not run (the scan stopped first)\n")
			continue
		}
		switch r.exitCode {
		case 124, 137:
			fmt.Fprintf(&b, "     timed out (exit %d)\n", r.exitCode)
		default:
			fmt.Fprintf(&b, "     exit %d\n", r.exitCode)
		}
		writeList(&b, "processes", eventStrings(r.procs), maxListed)
		writeList(&b, "network", networkItems(r), maxListed)
		writeList(&b, "files", eventStrings(r.files), maxListed)
		if r.hidden > 0 {
			fmt.Fprintf(&b, "     %-10s %d events from %s itself, not judged\n", "", r.hidden, strings.Join(r.ep.Harness, ", "))
		}
		writeFindings(&b, findings[i])
	}
	counts := countSeverities(all)
	fmt.Fprintf(&b, "\nverdict: %s  (%d critical, %d high, %d medium, %d low)\n", verdict,
		counts[sevCritical], counts[sevHigh], counts[sevMedium], counts[sevLow])
	_, _ = io.WriteString(w, b.String())
}

func writeFindings(b *strings.Builder, findings []finding) {
	for j, f := range findings {
		label := ""
		if j == 0 {
			label = "findings"
		}
		fmt.Fprintf(b, "     %-10s %-8s %s\n", label, f.Severity, printable(f.Detail))
	}
}

func eventStrings(events []traceEvent) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.String()
	}
	return out
}

// networkItems lists connection attempts and, when the DNS logger saw
// lookups, the names instead of strace's anonymous "DNS lookup" lines.
func networkItems(r entryRun) []string {
	if len(r.dns) == 0 {
		return eventStrings(r.network)
	}
	items := eventStrings(slices.DeleteFunc(slices.Clone(r.network), func(e traceEvent) bool { return e.op == "dns" }))
	for _, d := range r.dns {
		items = append(items, "DNS "+d.String()+" → NXDOMAIN")
	}
	return items
}

func writeList(b *strings.Builder, label string, items []string, maxListed int) {
	if len(items) == 0 {
		fmt.Fprintf(b, "     %-10s none\n", label)
		return
	}
	for i, item := range items {
		if i == maxListed {
			fmt.Fprintf(b, "     %-10s … %d more\n", "", len(items)-i)
			break
		}
		name := ""
		if i == 0 {
			name = label
		}
		fmt.Fprintf(b, "     %-10s %s\n", name, printable(item))
	}
}
