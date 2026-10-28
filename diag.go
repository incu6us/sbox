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
}

var _hostPort = regexp.MustCompile(`^[a-z0-9.-]+:[0-9]{1,5}$`)

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

// blocked is the BLOCKED and FAILED lines for the session's egress and
// lookups, each with its cause and, when seen often, the key to add.
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

// sessionSummary is what sbox dev prints when the terminal closes.
func sessionSummary(ran time.Duration, dir string, ev sessionEvents, ports []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "session %s · sbox sync %s shows what changed\n", age(ran), dir)
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
		for _, line := range append(ev.blocked(), portLines(ss.String(), ev.ports)...) {
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

// logMain implements `sbox log [-f] [-blocked] <id>`: the session's
// console, followed until it ends with -f, or just its BLOCKED lines.
func logMain(ctx context.Context, args []string, stdout, stderr io.Writer, vm sboxRunFunc, every time.Duration) int {
	flags := flag.NewFlagSet("sbox log", flag.ContinueOnError)
	flags.SetOutput(stderr)
	follow := flags.Bool("f", false, "follow the console until the session ends")
	onlyBlocked := flags.Bool("blocked", false, "only what was blocked")
	if flags.Parse(args) != nil || flags.NArg() != 1 {
		_, _ = fmt.Fprintln(stderr, "usage: sbox log [-f] [-blocked] <session-id>")
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
	printed := 0
	for {
		var out bytes.Buffer
		errOut := stderr
		if printed > 0 {
			errOut = io.Discard // once following, the session going away is the end
		}
		if err := vm(ctx, nil, &out, errOut, "log", id); err != nil {
			if printed > 0 {
				return exitOK
			}
			return vmFailed(stderr, err)
		}
		text := consoleText(out.String())
		if len(text) > printed {
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
