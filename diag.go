package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// sessionEvents counts what a session's helpers saw, from sbox-run
// events: the egress allowlist, the proxy's verdicts and the guest's DNS
// lookups. Every name in it came from the guest.
type sessionEvents struct {
	rules   []string            // host:port patterns the proxy allows
	ports   []int               // guest ports forwarded to the Mac
	allowed map[string]int      // host:port → connections
	denied  map[string]int      // host:port, or a request line that wasn't CONNECT → refusals
	failed  map[string]int      // allowed host:port whose connection failed
	lookups map[string]int      // name → queries
	qtypes  map[string][]string // name → query types, in order seen
	refused []string            // address:port the guest tried with nothing of sbox's there, in order
}

var (
	_hostPort = regexp.MustCompile(`^[a-z0-9.-]+:[0-9]{1,5}$`)
	_addrPort = regexp.MustCompile(`^[0-9.]+:[0-9]{1,5}$`)
)

// parseEvents reads sbox-run events lines.
func parseEvents(out string) sessionEvents {
	ev := sessionEvents{allowed: map[string]int{}, denied: map[string]int{}, failed: map[string]int{},
		lookups: map[string]int{}, qtypes: map[string][]string{}}
	for _, line := range strings.Split(out, "\n") {
		kind, rest, _ := strings.Cut(line, " ")
		switch kind {
		case "RULE":
			ev.rules = append(ev.rules, rest)
		case "PORT":
			if p, err := strconv.Atoi(rest); err == nil {
				ev.ports = append(ev.ports, p)
			}
		case "EGRESS":
			verdict, target, _ := strings.Cut(rest, " ")
			switch verdict {
			case "ALLOW":
				ev.allowed[target]++
			case "DENY":
				ev.denied[target]++
			case "FAIL":
				ev.failed[target]++
			}
		case "DNS":
			qtype, name, ok := strings.Cut(rest, " ")
			if !ok || ev.ruleCovers(name) {
				continue
			}
			ev.lookups[name]++
			if !slices.Contains(ev.qtypes[name], qtype) {
				ev.qtypes[name] = append(ev.qtypes[name], qtype)
			}
		case "REFUSED":
			if _addrPort.MatchString(rest) && !slices.Contains(ev.refused, rest) {
				ev.refused = append(ev.refused, rest)
			}
		}
	}
	return ev
}

// ruleCovers reports whether an egress rule names host, on any port: its
// lookup failing is not what blocks it, since the proxy resolves it.
func (ev sessionEvents) ruleCovers(host string) bool {
	for _, r := range ev.rules {
		if h, _, _ := strings.Cut(r, ":"); hostMatches(h, host) {
			return true
		}
	}
	return false
}

// hostMatches reports whether an egress rule's host (a name or *.domain)
// covers host.
func hostMatches(pattern, host string) bool {
	return pattern == host || (strings.HasPrefix(pattern, "*.") && strings.HasSuffix(host, pattern[1:]))
}

// byCount sorts a count map's keys, most first, then by name.
func byCount(m map[string]int) []string {
	return slices.SortedFunc(maps.Keys(m), func(a, b string) int {
		return cmp.Or(cmp.Compare(m[b], m[a]), cmp.Compare(a, b))
	})
}

// _hintAt is the count from which a blocked host gets the config line
// that would allow it: telemetry and update checks seen once or twice are
// usually right to stay refused.
const _hintAt = 3

// blocked is the BLOCKED and FAILED lines for the session's egress,
// lookups and refused connections, each with its cause and, when seen
// often (a refused service port at once), the key to add.
func (ev sessionEvents) blocked() []string {
	var lines, plain []string
	for _, t := range byCount(ev.denied) {
		n := ev.denied[t]
		if !_hostPort.MatchString(t) {
			plain = append(plain, fmt.Sprintf("BLOCKED egress  %s ×%d  not HTTPS: the proxy only tunnels HTTPS", printable(t), n))
			continue
		}
		line := fmt.Sprintf("BLOCKED egress  %s ×%d  not in egress", t, n)
		if n >= _hintAt {
			line += fmt.Sprintf(" → egress: [%s]", strings.TrimSuffix(t, ":443"))
		}
		lines = append(lines, line)
	}
	lines = append(lines, plain...)
	for _, t := range byCount(ev.failed) {
		lines = append(lines, fmt.Sprintf("FAILED  egress  %s ×%d  allowed, but the connection failed", printable(t), ev.failed[t]))
	}
	for _, name := range byCount(ev.lookups) {
		n := ev.lookups[name]
		line := fmt.Sprintf("BLOCKED dns     %s (%s) ×%d  not in egress", printable(name), printable(strings.Join(ev.qtypes[name], ", ")), n)
		if n >= _hintAt {
			line += fmt.Sprintf(" → egress: [%s]", printable(name))
		}
		lines = append(lines, line)
	}
	for _, t := range ev.refused {
		if port, ok := strings.CutPrefix(t, "172.16.0.1:"); ok {
			lines = append(lines, fmt.Sprintf("BLOCKED service host.sbox:%s  not in services → services: [%s]", port, port))
		} else {
			lines = append(lines, fmt.Sprintf("BLOCKED connect %s  straight to an address: the guest goes out only by name, through egress", t))
		}
	}
	return lines
}

// allowedSummary lists the hosts the session reached, most used first.
func (ev sessionEvents) allowedSummary() string {
	var parts []string
	for _, t := range byCount(ev.allowed) {
		parts = append(parts, fmt.Sprintf("%s ×%d", printable(t), ev.allowed[t]))
	}
	return strings.Join(parts, ", ")
}

// portLines reads the guest's `ss -Hltn` and reports listening ports the
// Mac can't reach: not forwarded, or forwarded but bound to loopback,
// where the relay (which connects to the guest's tap address) can't get.
func portLines(ss string, forwarded []int) []string {
	loopback, open, seen := map[int]bool{}, map[int]bool{}, map[int]bool{}
	for _, line := range strings.Split(ss, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		i := strings.LastIndexByte(f[3], ':')
		port, err := strconv.Atoi(f[3][i+1:])
		if i < 0 || err != nil || port < 1024 { // below 1024 can't be forwarded; 22 is sshd
			continue
		}
		seen[port] = true
		switch f[3][:i] {
		case "127.0.0.1", "[::1]", "::1":
			loopback[port] = true
		default:
			open[port] = true
		}
	}
	var lines []string
	for _, p := range slices.Sorted(maps.Keys(seen)) {
		fwd := slices.Contains(forwarded, p)
		switch {
		case fwd && loopback[p] && !open[p]:
			lines = append(lines, fmt.Sprintf("BLOCKED port    %d  listening on loopback only, the forward can't reach it → listen on 0.0.0.0 (e.g. --host 0.0.0.0)", p))
		case !fwd && open[p]:
			lines = append(lines, fmt.Sprintf("BLOCKED port    %d  guest is listening, not forwarded → ports: [%d]", p, p))
		}
	}
	return lines
}

// sessionSummary is what sbox dev prints when the terminal closes;
// unsynced is the guest's changes not yet synced, -1 when unknown.
func sessionSummary(ran time.Duration, dir string, unsynced int, ev sessionEvents, ports []string) string {
	var b strings.Builder
	switch {
	case unsynced > 0:
		fmt.Fprintf(&b, "session %s · the guest has %d %s: sbox sync %s\n", age(ran), unsynced, plural(unsynced, "change", "changes"), dir)
	case unsynced == 0:
		fmt.Fprintf(&b, "session %s · nothing to sync\n", age(ran))
	default:
		fmt.Fprintf(&b, "session %s · sbox sync %s shows what changed\n", age(ran), dir)
	}
	if s := ev.allowedSummary(); s != "" {
		fmt.Fprintf(&b, "egress   %s\n", s)
	}
	for _, line := range append(ev.blocked(), ports...) {
		b.WriteString(line + "\n")
	}
	return b.String()
}

// egressAllows reports whether the egress rules let the guest reach
// host:port, matching like the proxy: a name or *.domain, a port or *.
func egressAllows(rules []string, host string, port int) bool {
	for _, r := range rules {
		h, p, _ := strings.Cut(r, ":")
		if hostMatches(h, host) && (p == "*" || p == strconv.Itoa(port)) {
			return true
		}
	}
	return false
}

// devPreflight is what a session will be allowed, printed before it
// boots: egress, ports, services, secrets (names only), MCP servers and
// where each runs, then a BLOCKED line for each remote server the egress
// rules keep out. project is the folder's .mcp.json servers.
func devPreflight(cfg config, secrets map[string]string, project map[string]mcpServer) []string {
	var lines, blocked []string
	if len(cfg.Egress) == 0 {
		lines = append(lines, "egress   none: the guest reaches nothing outside")
	} else {
		lines = append(lines, "egress   "+strings.Join(cfg.Egress, ", "))
	}
	if len(cfg.Ports) > 0 {
		var ps []string
		for _, p := range cfg.Ports {
			ps = append(ps, fmt.Sprintf("%d → the Mac's localhost:%d", p, p))
		}
		lines = append(lines, "ports    "+strings.Join(ps, ", "))
	}
	if len(cfg.Services) > 0 {
		var ps []string
		for _, p := range cfg.Services {
			ps = append(ps, fmt.Sprintf("host.sbox:%d → the Mac's localhost:%d", p, p))
		}
		lines = append(lines, "services "+strings.Join(ps, ", "))
	}
	if len(cfg.Mounts) > 0 {
		var ms []string
		for _, m := range cfg.Mounts {
			ms = append(ms, m.Host+" → "+m.Guest)
		}
		lines = append(lines, "mounts   "+strings.Join(ms, ", "))
	}
	if len(secrets) > 0 {
		lines = append(lines, "secrets  "+strings.Join(slices.Sorted(maps.Keys(secrets)), ", "))
	}
	var servers []string
	for _, set := range []struct {
		servers map[string]mcpServer
		from    string
	}{{cfg.MCP.Servers, ""}, {project, ", .mcp.json"}} {
		for _, name := range slices.Sorted(maps.Keys(set.servers)) {
			s, shown := set.servers[name], printable(name)
			if s.URL == "" {
				servers = append(servers, fmt.Sprintf("%s (in the guest%s)", shown, set.from))
				if line := fetcherBlocked(cfg.Egress, shown, s.Command); line != "" {
					blocked = append(blocked, line)
				}
				continue
			}
			servers = append(servers, fmt.Sprintf("%s (remote%s)", shown, set.from))
			if line := remoteBlocked(cfg.Egress, shown, s.URL); line != "" {
				blocked = append(blocked, line)
			}
		}
	}
	if len(servers) > 0 {
		lines = append(lines, "mcp      "+strings.Join(servers, ", "))
	}
	return append(lines, blocked...)
}

// _fetchers are the commands that download an MCP server when it starts,
// with the registry hosts they need.
var _fetchers = map[string][]string{"npx": {"registry.npmjs.org"}, "uvx": {"pypi.org", "files.pythonhosted.org"}}

// fetcherBlocked is the BLOCKED line for a guest MCP server run through
// npx or uvx when egress lacks the registry, or "".
func fetcherBlocked(rules []string, name, command string) string {
	hosts := _fetchers[path.Base(command)]
	if len(hosts) == 0 || egressAllows(rules, hosts[0], 443) {
		return ""
	}
	return fmt.Sprintf("BLOCKED mcp     %s  %s fetches the server: %s isn't in egress → egress: [%s]", name, path.Base(command), hosts[0], strings.Join(hosts, ", "))
}

// remoteBlocked is the BLOCKED line for a remote MCP server the guest
// can't reach, or "".
func remoteBlocked(rules []string, name, rawURL string) string {
	u, err := url.Parse(rawURL)
	switch {
	case err != nil || u.Hostname() == "":
		return fmt.Sprintf("BLOCKED mcp     %s  url %q doesn't parse", name, printable(rawURL))
	case u.Scheme != "https":
		return fmt.Sprintf("BLOCKED mcp     %s  plain http: the proxy only tunnels HTTPS", name)
	}
	host, port := strings.ToLower(u.Hostname()), 443
	if p := u.Port(); p != "" {
		port, _ = strconv.Atoi(p)
	}
	if egressAllows(rules, host, port) {
		return ""
	}
	entry := host
	if port != 443 {
		entry = fmt.Sprintf("%s:%d", host, port)
	}
	return fmt.Sprintf("BLOCKED mcp     %s  host %s:%d not in egress → egress: [%s]", name, printable(host), port, printable(entry))
}

// readMCPJSON reads the project's .mcp.json servers; a missing or broken
// file has none (Claude Code reports a broken one itself).
func readMCPJSON(dir string) map[string]mcpServer {
	var f struct {
		MCPServers map[string]mcpServer `json:"mcpServers"`
	}
	data, err := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if err != nil || json.Unmarshal(data, &f) != nil {
		return nil
	}
	return f.MCPServers
}

// watchMain implements `sbox watch [id]`: a running session's BLOCKED
// lines as they appear or their counts change, until it ends. With no id,
// the one live session.
func watchMain(ctx context.Context, args []string, stdout, stderr io.Writer, vm sboxRunFunc, every time.Duration) int {
	if len(args) > 1 {
		_, _ = fmt.Fprintln(stderr, "usage: sbox watch [session-id]")
		return exitUsage
	}
	var id string
	if len(args) == 1 {
		id = args[0]
	} else {
		var ps bytes.Buffer
		if err := vm(ctx, nil, &ps, stderr, "ps"); err != nil {
			return vmFailed(stderr, err)
		}
		var live []string
		for _, s := range parseSessions(ps.String()) {
			if s.State != "exited" {
				live = append(live, s.ID)
			}
		}
		if len(live) != 1 {
			_, _ = fmt.Fprintf(stderr, "sbox: %d live sessions; name one: sbox watch <id> (sbox ps lists them)\n", len(live))
			return exitUsage
		}
		id = live[0]
	}
	if !_sessionID.MatchString(id) {
		_, _ = fmt.Fprintf(stderr, "sbox: %q is not a session id (sbox ps lists them)\n", id)
		return exitUsage
	}
	_, _ = fmt.Fprintf(stdout, "watching session %s (Ctrl-C to stop)\n", id)
	since := time.Now()
	shown := map[string]string{} // what a line is about → the line last printed for it
	for {
		var events, ss bytes.Buffer
		if err := vm(ctx, nil, &events, io.Discard, "events", id); err != nil {
			if ctx.Err() == nil {
				_, _ = fmt.Fprintf(stdout, "session %s ended\n", id)
			}
			return exitOK
		}
		_ = vm(ctx, strings.NewReader(""), &ss, io.Discard, "attach", id, "--", "ss", "-Hltn")
		ev := parseEvents(events.String())
		lines := append(ev.blocked(), portLines(ss.String(), ev.ports)...)
		for _, line := range append(lines, toolDenials(ctx, vm, id, since)...) {
			f := strings.Fields(line)
			key := strings.Join(f[:min(3, len(f))], " ")
			if shown[key] != line {
				shown[key] = line
				_, _ = fmt.Fprintln(stdout, line)
			}
		}
		select {
		case <-ctx.Done():
			return exitOK
		case <-time.After(every):
		}
	}
}

// logMain implements `sbox log [-f] [-blocked | -start] <id>`: the
// session's console, or with -start the output of dev.start's background
// steps, followed until it ends with -f; or just its BLOCKED lines.
func logMain(ctx context.Context, args []string, stdout, stderr io.Writer, vm sboxRunFunc, every time.Duration) int {
	flags := flag.NewFlagSet("sbox log", flag.ContinueOnError)
	flags.SetOutput(stderr)
	follow := flags.Bool("f", false, "follow the console until the session ends")
	onlyBlocked := flags.Bool("blocked", false, "only what was blocked")
	start := flags.Bool("start", false, "dev.start's background output (~/.sbox/start.log)")
	if flags.Parse(args) != nil || flags.NArg() != 1 || *onlyBlocked && *start {
		_, _ = fmt.Fprintln(stderr, "usage: sbox log [-f] [-blocked | -start] <session-id>")
		return exitUsage
	}
	id := flags.Arg(0)
	if !_sessionID.MatchString(id) {
		_, _ = fmt.Fprintf(stderr, "sbox: %q is not a session id (sbox ps lists them)\n", id)
		return exitUsage
	}
	if *onlyBlocked {
		var events bytes.Buffer
		if err := vm(ctx, nil, &events, stderr, "events", id); err != nil {
			return vmFailed(stderr, err)
		}
		for _, line := range parseEvents(events.String()).blocked() {
			_, _ = fmt.Fprintln(stdout, line)
		}
		return exitOK
	}
	printed, read, polled := 0, 0, false // console: text printed; start.log: bytes read
	for {
		var out bytes.Buffer
		errOut := stderr
		if polled {
			errOut = io.Discard // once following, the session going away is the end
		}
		var err error
		if *start {
			err = vm(ctx, strings.NewReader(""), &out, errOut, "attach", id, "--", "sh", "-c", fmt.Sprintf("tail -c +%d ~/.sbox/start.log 2>/dev/null; true", read+1))
		} else {
			err = vm(ctx, nil, &out, errOut, "log", id)
		}
		if err != nil {
			if polled {
				return exitOK
			}
			return vmFailed(stderr, err)
		}
		polled = true
		if *start {
			read += out.Len()
			_, _ = io.WriteString(stdout, consoleText(out.String()))
		} else if text := consoleText(out.String()); len(text) > printed {
			_, _ = io.WriteString(stdout, text[printed:])
			printed = len(text)
		}
		if !*follow {
			return exitOK
		}
		select {
		case <-ctx.Done():
			return exitOK
		case <-time.After(every):
		}
	}
}

// consoleText makes a serial console readable: CR LF to LF, and anything
// unprintable from the guest escaped.
func consoleText(raw string) string {
	var b strings.Builder
	for _, line := range strings.SplitAfter(raw, "\n") {
		text, nl := strings.CutSuffix(line, "\n")
		b.WriteString(printable(strings.TrimSuffix(text, "\r")))
		if nl {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// _denialScript runs in the guest as the runner and prints a "kind\ttool"
// line for each tool call Claude Code denied since argv[1] (an RFC 3339
// time, compared as text with the transcripts' timestamps). The format,
// confirmed on Claude Code 2.1's transcripts: a user entry carrying
// toolDenialKind answers a tool_use, named in an earlier assistant entry.
const _denialScript = `import glob, json, os, sys
since, uses = sys.argv[1], {}
for path in sorted(glob.glob(os.path.expanduser("~/.claude/projects/*/*.jsonl"))):
    for line in open(path, errors="replace"):
        if '"tool_use"' not in line and "toolDenialKind" not in line:
            continue
        try:
            d = json.loads(line)
        except ValueError:
            continue
        content = (d.get("message") or {}).get("content")
        if not isinstance(content, list):
            continue
        for c in content:
            if not isinstance(c, dict):
                continue
            if c.get("type") == "tool_use":
                uses[c.get("id")] = c.get("name")
            elif "toolDenialKind" in d and c.get("type") == "tool_result" and str(d.get("timestamp", "")) >= since:
                print("%s\t%s" % (d["toolDenialKind"], uses.get(c.get("tool_use_id"), "?")))
`

var (
	_denialKind = regexp.MustCompile(`^[a-z-]{1,40}$`)
	_toolName   = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,100}$`)
)

// denialLines groups the denial script's output (from the guest, so
// parsed strictly) into BLOCKED tool lines, most denied first.
func denialLines(out string) []string {
	counts := map[string]int{}
	for _, line := range strings.Split(out, "\n") {
		kind, tool, ok := strings.Cut(line, "\t")
		if ok && _denialKind.MatchString(kind) && _toolName.MatchString(tool) {
			counts[tool+"\t"+kind]++
		}
	}
	var lines []string
	for _, key := range byCount(counts) {
		tool, kind, _ := strings.Cut(key, "\t")
		cause := "denied (" + kind + ")"
		switch kind {
		case "permission-rule":
			cause = "denied by a permission rule (permissions.deny or mcp.deny)"
		case "user-rejected":
			cause = "rejected at the prompt"
		}
		lines = append(lines, fmt.Sprintf("BLOCKED tool    %s ×%d  %s", tool, counts[key], cause))
	}
	return lines
}

// toolDenials asks session id's guest for Claude Code's denied tool calls
// since since; none when it can't answer.
func toolDenials(ctx context.Context, vm sboxRunFunc, id string, since time.Time) []string {
	var out bytes.Buffer
	if err := vm(ctx, strings.NewReader(""), &out, io.Discard, "attach", id, "--", "python3", "-I", "-c", _denialScript, since.UTC().Format(time.RFC3339)); err != nil {
		return nil
	}
	return denialLines(out.String())
}

// _mcpWhich prints each of its arguments the guest has no command for, in
// a login shell so ~/.local/bin counts.
const _mcpWhich = `for c; do command -v "$c" >/dev/null 2>&1 || echo "$c"; done`

// mcpMissing asks session id for the commands of the guest's MCP servers
// and returns a BLOCKED line per server whose command isn't there. Running
// the servers instead (claude mcp list) takes 30 s a server and slows the
// first claude to start beside it.
func mcpMissing(ctx context.Context, vm sboxRunFunc, id string, servers map[string]mcpServer) []string {
	var cmds []string
	for _, s := range servers {
		if s.URL == "" && s.Command != "" && !slices.Contains(cmds, s.Command) {
			cmds = append(cmds, s.Command)
		}
	}
	if len(cmds) == 0 {
		return nil
	}
	slices.Sort(cmds)
	var out bytes.Buffer
	_ = vm(ctx, strings.NewReader(""), &out, io.Discard, append([]string{"attach", id, "--", "bash", "-lc", _mcpWhich, "sh"}, cmds...)...)
	missing := strings.Split(out.String(), "\n")
	var lines []string
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		if c := servers[name].Command; servers[name].URL == "" && slices.Contains(missing, c) {
			lines = append(lines, fmt.Sprintf("BLOCKED mcp     %s  could not start: %s isn't in the guest → image.packages, or install it into ~/.local", printable(name), printable(c)))
		}
	}
	return lines
}

// traceCommand wraps a terminal's command (a login shell when empty) in
// strace for sbox dev -trace: the runner traces what it starts, with
// scan's syscalls, one file per process in the guest's tmpfs.
func traceCommand(cmd []string) []string {
	run := "bash -l"
	if len(cmd) > 0 {
		run = shellJoin(cmd)
	}
	return []string{"bash", "-lc", "rm -rf /tmp/sbox-trace/terminal && mkdir -p /tmp/sbox-trace/terminal && exec strace -ff -qq -s 200 -e trace=" +
		_traced + " -o /tmp/sbox-trace/terminal/t -- " + run}
}

// traceReadBack sends one phase's traces (terminal or setup) of a -trace
// session: "T <pid> <line>" for what scan's rules read (the quiet paths
// dropped), "D <pid> <line>" for every refused write, quiet or not.
func traceReadBack(phase string) string {
	return `for f in /tmp/sbox-trace/` + phase + `/t.*; do
	[ -f "$f" ] || continue
	pid=${f##*.}
	grep -avE '` + _quietPaths + `' "$f" | sed "s/^/T $pid /"
	grep -aE ' = -1 E(ROFS|ACCES) ' "$f" | sed "s/^/D $pid /"
done | head -n ` + strconv.Itoa(_maxTraceLines)
}

// traceLines turns traceReadBack's output (written by the runner, so it
// informs and never judges) into scan's findings at INFO and a BLOCKED
// line per refused write outside the home.
func traceLines(out, canary string) []string {
	var traces processTraces
	writes := map[string]string{} // path → errno
	for _, line := range strings.Split(out, "\n") {
		tag, rest, _ := strings.Cut(line, " ")
		pidText, call, _ := strings.Cut(rest, " ")
		pid, err := strconv.Atoi(pidText)
		if err != nil {
			continue
		}
		switch tag {
		case "T":
			traces.add(pid, call)
		case "D":
			if ev, ok := parseTrace(call); ok && ev.kind == "file" && ev.op != "read" && !strings.HasPrefix(ev.path, _home) {
				writes[ev.path] = ev.result
			}
		}
	}
	r := entryRun{ep: entryPoint{Kind: "dev", Name: "terminal"}, ran: true}
	attribute(&r, traces)
	var lines []string
	for _, f := range analyze(r, canary) {
		lines = append(lines, fmt.Sprintf("INFO    %s  %s", f.Rule, printable(f.Detail)))
	}
	for _, p := range slices.Sorted(maps.Keys(writes)) {
		cause := writes[p] + ": not the runner's"
		if writes[p] == "EROFS" {
			cause = "EROFS: the root is read-only → install into ~/.local or the project"
		}
		lines = append(lines, fmt.Sprintf("BLOCKED write   %s  %s", printable(p), cause))
	}
	return lines
}
