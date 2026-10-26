package main

import (
	"cmp"
	"encoding/hex"
	"encoding/json"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"
)

// severity ranks how much attention a finding needs, as in skills-audit.
type severity int

const (
	sevInfo severity = iota
	sevLow
	sevMedium
	sevHigh
	sevCritical
)

var _severityNames = [...]string{"INFO", "LOW", "MEDIUM", "HIGH", "CRITICAL"}

func (s severity) String() string { return _severityNames[s] }

// MarshalText renders the severity by name in JSON output.
func (s severity) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

const (
	verdictPass   = "PASS"
	verdictReview = "REVIEW"
	verdictBlock  = "BLOCK"

	_home = "/home/runner/"
	_work = "/home/runner/work/" // the skill, and the project dir hooks are given
)

// finding is one thing an entry point did that deserves a look.
type finding struct {
	Severity severity `json:"severity"`
	Rule     string   `json:"rule"`
	Detail   string   `json:"detail"`
}

// Path lists: a trailing slash means "anything under".
var (
	_credentialPaths = prefixed(_home, ".ssh/", ".aws/", ".config/gcloud/", ".azure/", ".kube/config",
		".docker/config.json", ".netrc", ".npmrc", ".pypirc", ".git-credentials", ".gnupg/",
		".config/gh/", ".bash_history", ".zsh_history", ".env")
	_agentConfigPaths = append(
		prefixed(_home, ".claude/settings.json", ".claude/settings.local.json", ".claude/CLAUDE.md",
			".claude/skills/", ".claude/commands/", ".claude/agents/", ".claude/hooks/", ".claude.json",
			".cursor/mcp.json", ".cursor/hooks.json", ".cursor/rules/", ".cursor/skills/", ".cursor/commands/",
			".codex/"),
		prefixed(_work, "CLAUDE.md", "AGENTS.md", ".mcp.json", ".cursorrules", ".claude/settings.json",
			".claude/settings.local.json", ".cursor/rules/", ".cursor/mcp.json")...)
	_persistencePaths = append(
		prefixed(_home, ".bashrc", ".bash_profile", ".profile", ".zshrc", ".zprofile", ".zshenv",
			".config/autostart/", ".config/systemd/", ".ssh/authorized_keys"),
		"/var/spool/cron/", "/etc/cron", "/etc/profile.d/", "/etc/systemd/")
	_agentDirs = prefixed(_home, ".claude/", ".cursor/", ".codex/")
	_tempDirs  = []string{"/tmp/", "/var/tmp/", "/dev/shm/", _home + ".cache/"}

	_networkTools = map[string]bool{
		"curl": true, "wget": true, "nc": true, "ncat": true, "netcat": true, "socat": true,
		"ssh": true, "scp": true, "sftp": true, "rsync": true, "ftp": true, "telnet": true,
		"dig": true, "nslookup": true, "host": true, "drill": true,
	}
	_escalation = map[string]bool{"sudo": true, "su": true, "doas": true, "pkexec": true}
	_environ    = regexp.MustCompile(`^/proc/(self|thread-self|\d+)/environ$`)
)

// analyze turns what one entry point did into findings, worst first.
// canary is the marker in the run's fake credentials.
func analyze(r entryRun, canary string) []finding {
	var out []finding
	seen := make(map[string]bool)
	add := func(sev severity, rule, detail string) {
		if !seen[rule+detail] {
			seen[rule+detail] = true
			out = append(out, finding{Severity: sev, Rule: rule, Detail: detail})
		}
	}

	readsCredentials, usesNetwork := false, false
	for _, f := range r.files {
		target := f.path
		if f.op == "rename" {
			target = f.target
		}
		switch {
		case f.op == "read" && matches(f.path, _credentialPaths):
			readsCredentials = true
			add(sevHigh, "credential-read", "reads "+f.path+missing(f))
		case f.op == "read" && _environ.MatchString(f.path):
			add(sevMedium, "environ-read", "reads a process environment: "+f.path)
		case f.op == "read":
		case matches(target, _agentConfigPaths):
			add(sevHigh, "agent-config-write", f.op+"s agent configuration: "+target)
		case matches(target, _persistencePaths):
			add(sevHigh, "persistence", f.op+"s a startup or access file: "+target)
		case matches(target, _agentDirs):
			add(sevLow, "agent-dir-write", f.op+"s under an agent's folder: "+target)
		}
	}

	for _, n := range r.network {
		switch {
		case n.op == "connect" && !isLocal(n.path):
			usesNetwork = true
			add(sevHigh, "connect", "tries to connect to "+n.path)
		case n.op == "dns" && len(r.dns) == 0:
			usesNetwork = true
			add(sevMedium, "dns-lookup", "looks up a hostname (name not recorded)")
		}
	}
	for _, d := range r.dns {
		usesNetwork = true
		add(sevMedium, "dns-lookup", "looks up "+d.name)
		if carries(d.name, canary) {
			add(sevCritical, "canary-exfiltration", "sends a planted credential out in a DNS lookup: "+d.name)
		}
	}
	for _, p := range r.procs {
		if !carries(strings.Join(p.argv, " "), canary) {
			continue
		}
		name := path.Base(p.path)
		if _networkTools[name] {
			add(sevCritical, "canary-exfiltration", "passes a planted credential to "+name)
		} else {
			add(sevHigh, "canary-argv", "puts a planted credential on "+name+"'s command line")
		}
	}

	for _, p := range r.procs {
		name := path.Base(p.path)
		switch {
		case matches(p.path, _tempDirs):
			add(sevHigh, "temp-exec", "runs a program from a temp folder: "+p.path)
		case _networkTools[name]:
			add(sevMedium, "network-tool", "runs "+name)
		case _escalation[name]:
			add(sevMedium, "escalation", "tries to gain privileges with "+name)
		}
	}

	if readsCredentials && usesNetwork {
		add(sevCritical, "possible-exfiltration", "reads credentials and tries the network in the same entry point")
	}
	if r.exitCode == 124 || r.exitCode == 137 {
		add(sevLow, "timeout", "still running at the timeout")
	}

	slices.SortStableFunc(out, func(a, b finding) int { return cmp.Compare(b.Severity, a.Severity) })
	return out
}

// verdictFor blocks on any critical finding, asks for review on medium or
// high ones, and passes the rest, as skills-audit does.
func verdictFor(findings []finding) string {
	top := sevInfo
	for _, f := range findings {
		top = max(top, f.Severity)
	}
	switch {
	case top == sevCritical:
		return verdictBlock
	case top >= sevMedium:
		return verdictReview
	}
	return verdictPass
}

func countSeverities(findings []finding) map[severity]int {
	counts := make(map[severity]int)
	for _, f := range findings {
		counts[f.Severity]++
	}
	return counts
}

// writeScanJSON prints the scan as one JSON object.
func writeScanJSON(w io.Writer, dir string, runs []entryRun, findings [][]finding, verdict string, all []finding) error {
	type dnsJSON struct {
		Name  string   `json:"name"`
		Types []string `json:"types"`
	}
	type entryJSON struct {
		Kind      string    `json:"kind"`
		Name      string    `json:"name"`
		Source    string    `json:"source"`
		Command   string    `json:"command"`
		Ran       bool      `json:"ran"`
		ExitCode  int       `json:"exit_code"`
		Processes []string  `json:"processes"`
		Network   []string  `json:"network"`
		Files     []string  `json:"files"`
		DNS       []dnsJSON `json:"dns"`
		Findings  []finding `json:"findings"`
	}
	report := struct {
		Target  string         `json:"target"`
		Verdict string         `json:"verdict"`
		Counts  map[string]int `json:"counts"`
		Entries []entryJSON    `json:"entries"`
	}{Target: dir, Verdict: verdict, Counts: make(map[string]int), Entries: []entryJSON{}}
	for sev, n := range countSeverities(all) {
		report.Counts[sev.String()] = n
	}
	for i, r := range runs {
		e := entryJSON{
			Kind: r.ep.Kind, Name: r.ep.Name, Source: r.ep.Source, Command: r.ep.Command,
			Ran: r.ran, ExitCode: r.exitCode,
			Processes: eventStrings(r.procs), Network: networkItems(r), Files: eventStrings(r.files),
			DNS: []dnsJSON{}, Findings: append([]finding{}, findings[i]...),
		}
		for _, d := range r.dns {
			e.DNS = append(e.DNS, dnsJSON{Name: d.name, Types: d.types})
		}
		report.Entries = append(report.Entries, e)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(report)
}

// carries reports whether s contains the canary, as is or hex-encoded,
// in any case: the usual ways a stolen value gets packed into a hostname or
// an argument.
func carries(s, canary string) bool {
	if canary == "" {
		return false
	}
	s = strings.ToLower(s)
	return strings.Contains(s, canary) || strings.Contains(s, hex.EncodeToString([]byte(canary)))
}

func prefixed(base string, rels ...string) []string {
	out := make([]string, len(rels))
	for i, r := range rels {
		out[i] = base + r
	}
	return out
}

// matches reports whether p is one of the paths, or under one ending in "/".
func matches(p string, paths []string) bool {
	for _, x := range paths {
		if p == x || (strings.HasSuffix(x, "/") && strings.HasPrefix(p, x)) {
			return true
		}
	}
	return false
}

// isLocal is loopback, a unix socket or the VM side of the tap.
func isLocal(addr string) bool {
	return strings.HasPrefix(addr, "unix:") || strings.HasPrefix(addr, "127.") ||
		strings.HasPrefix(addr, "[::1]") || strings.HasPrefix(addr, "172.16.0.1:")
}

func missing(f traceEvent) string {
	if f.result == "ENOENT" {
		return " (missing)"
	}
	return ""
}
