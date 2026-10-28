package main

import (
	"cmp"
	"encoding/hex"
	"encoding/json"
	"io"
	"maps"
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

	_redactedMark = "[redacted:" // where the host took a config secret out of guest output

	_home = "/home/runner/"
	_work = "/home/runner/work/" // the skill, and the project dir hooks are given
)

// _ruleNames are every rule a finding can carry, for rules.severity.
var _ruleNames = []string{"agent-config-write", "agent-dir-write", "canary-argv", "canary-exfiltration", "connect",
	"credential-read", "dns-lookup", "environ-read", "escalation", "network-tool", "persistence",
	"possible-exfiltration", "prefetch-failed", "prefetch-refused", "prefetch-timeout", "temp-exec", "timeout"}

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
// canary is the marker in the run's fake credentials; creds are paths read
// as credentials besides the built-in ones.
func analyze(r entryRun, canary string, creds []string) []finding {
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
		case f.op == "read" && (matches(f.path, _credentialPaths) || matches(f.path, creds)):
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
		} else if strings.Contains(d.name, _redactedMark) {
			add(sevCritical, "canary-exfiltration", "sends a secret from the config out in a DNS lookup: "+d.name)
		}
	}
	for _, p := range r.procs {
		argv := strings.Join(p.argv, " ")
		what := "a planted credential"
		switch {
		case carries(argv, canary):
		case strings.Contains(argv, _redactedMark): // the host redacted a config secret on the way in
			what = "a secret from the config"
		default:
			continue
		}
		name := path.Base(p.path)
		if _networkTools[name] {
			add(sevCritical, "canary-exfiltration", "passes "+what+" to "+name)
		} else {
			add(sevHigh, "canary-argv", "puts "+what+" on "+name+"'s command line")
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
func verdictFor(findings []finding) string { return config{}.scanRules().verdict(findings) }

// scanRules is the config's calibration of sbox scan, applied on top of
// analyze.
type scanRules struct {
	credentialPaths []string
	expected        []string
	severity        map[string]severity
	review, block   severity
	set             config // what the config said, for overrides
}

// scanRules reads the rules keys; review is capped at block.
func (c config) scanRules() scanRules {
	level := func(name string, def severity) severity {
		if i := slices.Index(_severityNames[:], name); i >= 0 {
			return severity(i)
		}
		return def
	}
	s := scanRules{credentialPaths: c.Rules.CredentialPaths, expected: c.Rules.ExpectedHosts, severity: map[string]severity{}, set: c}
	for rule, name := range c.Rules.Severity {
		s.severity[rule] = level(name, sevInfo)
	}
	s.block = level(c.Rules.BlockOn, sevCritical)
	s.review = min(level(c.Rules.ReviewOn, sevMedium), s.block)
	return s
}

// apply drops lookups of expected hosts to INFO, marked so, and sets the
// overridden rules' severities; worst first again.
func (s scanRules) apply(findings []finding) []finding {
	out := slices.Clone(findings)
	for i, f := range out {
		if name, ok := strings.CutPrefix(f.Detail, "looks up "); ok && f.Rule == "dns-lookup" &&
			slices.ContainsFunc(s.expected, func(h string) bool { return hostMatches(h, name) }) {
			out[i].Severity, out[i].Detail = sevInfo, f.Detail+" (expected)"
		}
		if sev, ok := s.severity[f.Rule]; ok {
			out[i].Severity = sev
		}
	}
	slices.SortStableFunc(out, func(a, b finding) int { return cmp.Compare(b.Severity, a.Severity) })
	return out
}

// verdict is BLOCK from the block threshold, REVIEW from the review one.
func (s scanRules) verdict(findings []finding) string {
	top := sevInfo
	for _, f := range findings {
		top = max(top, f.Severity)
	}
	switch {
	case top >= s.block:
		return verdictBlock
	case top >= s.review && top > sevInfo:
		return verdictReview
	}
	return verdictPass
}

// overrides names what the config changed in how findings are judged,
// for the report header and JSON; nil when nothing.
func (s scanRules) overrides() []string {
	var out []string
	r := s.set.Rules
	if len(r.ExpectedHosts) > 0 {
		out = append(out, "expected hosts: "+strings.Join(r.ExpectedHosts, ", "))
	}
	for _, rule := range slices.Sorted(maps.Keys(r.Severity)) {
		out = append(out, "severity: "+rule+" "+r.Severity[rule])
	}
	if r.ReviewOn != "" {
		out = append(out, "review_on: "+r.ReviewOn)
	}
	if r.BlockOn != "" {
		out = append(out, "block_on: "+r.BlockOn)
	}
	return out
}

// scanExit is sbox scan's exit status: non-zero at REVIEW or BLOCK, or with
// report.fail_on: block, at BLOCK only.
func scanExit(verdict, failOn string) int {
	switch {
	case verdict == verdictBlock:
		return scanBlock
	case verdict == verdictReview && failOn != "block":
		return scanReview
	}
	return exitOK
}

func countSeverities(findings []finding) map[severity]int {
	counts := make(map[severity]int)
	for _, f := range findings {
		counts[f.Severity]++
	}
	return counts
}

// writeScanJSON prints the scan as one JSON object.
// writeScanJSON writes the report as one object, with the config's
// overrides and the names of its real secrets under config when there are
// any.
func writeScanJSON(w io.Writer, target targetJSON, pre *prefetchResult, runs []entryRun, findings [][]finding, verdict string, all []finding, overrides, secrets []string) error {
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
		Hidden    int       `json:"harness_events_hidden"`
		Processes []string  `json:"processes"`
		Network   []string  `json:"network"`
		Files     []string  `json:"files"`
		DNS       []dnsJSON `json:"dns"`
		Findings  []finding `json:"findings"`
	}
	type configJSON struct {
		Overrides []string `json:"overrides,omitempty"`
		Secrets   []string `json:"secrets,omitempty"`
	}
	report := struct {
		Target   targetJSON      `json:"target"`
		Verdict  string          `json:"verdict"`
		Counts   map[string]int  `json:"counts"`
		Config   *configJSON     `json:"config,omitempty"`
		Prefetch *prefetchResult `json:"prefetch"`
		Entries  []entryJSON     `json:"entries"`
	}{Target: target, Verdict: verdict, Counts: make(map[string]int), Prefetch: pre, Entries: []entryJSON{}}
	if len(overrides)+len(secrets) > 0 {
		report.Config = &configJSON{Overrides: overrides, Secrets: secrets}
	}
	for sev, n := range countSeverities(all) {
		report.Counts[sev.String()] = n
	}
	for i, r := range runs {
		e := entryJSON{
			Kind: r.ep.Kind, Name: r.ep.Name, Source: r.ep.Source, Command: r.ep.Command,
			Ran: r.ran, ExitCode: r.exitCode, Hidden: r.hidden,
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
