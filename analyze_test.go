package main

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestAnalyze(t *testing.T) {
	t.Parallel()

	file := func(op, p, result string) traceEvent {
		return traceEvent{kind: "file", op: op, path: p, result: result}
	}
	conn := func(addr string) traceEvent {
		return traceEvent{kind: "net", op: "connect", path: addr, result: "attempted"}
	}
	proc := func(p string) traceEvent { return traceEvent{kind: "proc", path: p, argv: []string{p}} }

	tests := []struct {
		name        string
		give        entryRun
		wantRules   []string
		wantVerdict string
	}{
		{
			name: "plugin minding its own business",
			give: entryRun{
				files: []traceEvent{
					file("read", "/home/runner/work/hooks/a.js", "ok"),
					file("read", "/home/runner/.config/tool/config.json", "ENOENT"),
					file("write", "/home/runner/.claude/.tool-active", "ok"),
				},
				procs: []traceEvent{proc("/usr/local/bin/node")},
			},
			wantRules:   []string{"agent-dir-write"},
			wantVerdict: verdictPass,
		},
		{
			name: "network attempt",
			give: entryRun{
				network: []traceEvent{conn("93.184.215.14:80"), conn("127.0.0.1:9000"), conn("unix:/run/x.sock")},
				dns:     []dnsName{{name: "example.com", types: []string{"A"}}},
				procs:   []traceEvent{proc("/usr/bin/curl")},
			},
			wantRules:   []string{"connect", "dns-lookup", "network-tool"},
			wantVerdict: verdictReview,
		},
		{
			name:        "credential read alone",
			give:        entryRun{files: []traceEvent{file("read", "/home/runner/.aws/credentials", "ENOENT")}},
			wantRules:   []string{"credential-read"},
			wantVerdict: verdictReview,
		},
		{
			name: "credential read plus network",
			give: entryRun{
				files: []traceEvent{file("read", "/home/runner/.ssh/id_ed25519", "ENOENT")},
				dns:   []dnsName{{name: "collector.example", types: []string{"A"}}},
			},
			wantRules:   []string{"possible-exfiltration", "credential-read", "dns-lookup"},
			wantVerdict: verdictBlock,
		},
		{
			name: "canary sent in a DNS lookup",
			give: entryRun{
				files: []traceEvent{file("read", "/home/runner/.aws/credentials", "ok")},
				dns:   []dnsName{{name: "sboxa1b2c3d4e5f6kf9w.collector.example", types: []string{"A"}}},
			},
			wantRules:   []string{"canary-exfiltration", "possible-exfiltration", "credential-read", "dns-lookup"},
			wantVerdict: verdictBlock,
		},
		{
			name: "hex-encoded canary in a lookup",
			give: entryRun{
				dns: []dnsName{{name: "613162326333643465356636.x.example", types: []string{"A"}}}, // hex of a1b2c3d4e5f6
			},
			wantRules:   []string{"canary-exfiltration", "dns-lookup"},
			wantVerdict: verdictBlock,
		},
		{
			name: "canary on command lines",
			give: entryRun{
				procs: []traceEvent{
					{kind: "proc", path: "/usr/bin/curl", argv: []string{"curl", "-H", "Authorization: Bearer ghp_sboxA1B2C3D4E5F6"}},
					{kind: "proc", path: "/usr/local/bin/node", argv: []string{"node", "x.js", "sboxa1b2c3d4e5f6"}},
				},
			},
			wantRules:   []string{"canary-exfiltration", "canary-argv", "network-tool"},
			wantVerdict: verdictBlock,
		},
		{
			name: "config and persistence writes",
			give: entryRun{
				files: []traceEvent{
					file("write", "/home/runner/.claude/settings.json", "ok"),
					file("write", "/home/runner/work/CLAUDE.md", "ok"),
					file("write", "/home/runner/.bashrc", "ok"),
					{kind: "file", op: "rename", path: "/tmp/x", target: "/home/runner/.ssh/authorized_keys", result: "ok"},
				},
			},
			wantRules:   []string{"agent-config-write", "persistence"},
			wantVerdict: verdictReview,
		},
		{
			name: "temp exec, escalation, environ, timeout",
			give: entryRun{
				procs:    []traceEvent{proc("/tmp/payload"), proc("/usr/bin/sudo")},
				files:    []traceEvent{file("read", "/proc/1/environ", "EACCES")},
				exitCode: 124,
			},
			wantRules:   []string{"temp-exec", "environ-read", "escalation", "timeout"}, // same severity: files before processes
			wantVerdict: verdictReview,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := analyze(tt.give, "a1b2c3d4e5f6", nil)
			var rules []string
			for _, f := range got {
				if !slices.Contains(rules, f.Rule) {
					rules = append(rules, f.Rule)
				}
			}
			if !slices.Equal(rules, tt.wantRules) {
				t.Errorf("rules = %v, want %v (findings %+v)", rules, tt.wantRules, got)
			}
			if v := verdictFor(got); v != tt.wantVerdict {
				t.Errorf("verdict = %s, want %s", v, tt.wantVerdict)
			}
		})
	}
}

// TestScanRules: the config's calibration on top of analyze: an extra
// credential path, expected hosts, severity overrides and thresholds, and
// the overrides the report names.
func TestScanRules(t *testing.T) {
	t.Parallel()
	var cfg config
	cfg.Rules.CredentialPaths = []string{"/home/runner/.config/myapp/"}
	cfg.Rules.ExpectedHosts = []string{"telemetry.vercel.com", "*.sentry.io"}
	cfg.Rules.Severity = map[string]string{"agent-dir-write": "INFO"}
	rules := cfg.scanRules()
	file := func(op, p, result string) traceEvent {
		return traceEvent{kind: "file", op: op, path: p, result: result}
	}
	run := entryRun{
		files: []traceEvent{file("read", "/home/runner/.config/myapp/token", "ok"), file("write", "/home/runner/.claude/cache", "ok")},
		dns:   []dnsName{{name: "telemetry.vercel.com"}, {name: "o1.ingest.sentry.io"}, {name: "evil.example"}, {name: "x.sboxa1b2c3d4e5f6.example"}},
	}
	got := rules.apply(analyze(run, "a1b2c3d4e5f6", rules.credentialPaths))
	has := func(sev severity, rule, detail string) bool {
		return slices.Contains(got, finding{Severity: sev, Rule: rule, Detail: detail})
	}
	for _, want := range []finding{
		{sevHigh, "credential-read", "reads /home/runner/.config/myapp/token"},
		{sevInfo, "dns-lookup", "looks up telemetry.vercel.com (expected)"},
		{sevInfo, "dns-lookup", "looks up o1.ingest.sentry.io (expected)"},
		{sevMedium, "dns-lookup", "looks up evil.example"},
		{sevCritical, "canary-exfiltration", "sends a planted credential out in a DNS lookup: x.sboxa1b2c3d4e5f6.example"},
		{sevInfo, "agent-dir-write", "writes under an agent's folder: /home/runner/.claude/cache"},
	} {
		if !has(want.Severity, want.Rule, want.Detail) {
			t.Errorf("findings lack %+v:\n%+v", want, got)
		}
	}
	if got[0].Severity != sevCritical {
		t.Errorf("findings not worst first after the overrides: %+v", got)
	}

	medium := []finding{{Severity: sevMedium, Rule: "dns-lookup"}}
	high := []finding{{Severity: sevHigh, Rule: "connect"}}
	cfg.Rules.ReviewOn, cfg.Rules.BlockOn = "HIGH", "HIGH"
	if r := cfg.scanRules(); r.verdict(medium) != verdictPass || r.verdict(high) != verdictBlock {
		t.Errorf("review_on and block_on HIGH: medium %s, high %s; want PASS, BLOCK", r.verdict(medium), r.verdict(high))
	}
	cfg.Rules.ReviewOn, cfg.Rules.BlockOn = "", "LOW"
	if r := cfg.scanRules(); r.verdict([]finding{{Severity: sevLow}}) != verdictBlock {
		t.Errorf("block_on LOW alone: want a low finding to block (review capped at block)")
	}
	if got := cfg.scanRules().overrides(); !slices.Equal(got, []string{"expected hosts: telemetry.vercel.com, *.sentry.io", "severity: agent-dir-write INFO", "block_on: LOW"}) {
		t.Errorf("overrides = %q", got)
	}
	if (config{}).scanRules().overrides() != nil {
		t.Error("no config, overrides listed")
	}

	for _, tt := range []struct {
		verdict, failOn string
		want            int
	}{{verdictPass, "", exitOK}, {verdictReview, "", scanReview}, {verdictBlock, "", scanBlock}, {verdictReview, "block", exitOK}, {verdictBlock, "block", scanBlock}} {
		if got := scanExit(tt.verdict, tt.failOn); got != tt.want {
			t.Errorf("scanExit(%s, %q) = %d, want %d", tt.verdict, tt.failOn, got, tt.want)
		}
	}
}

func TestScanReportsTakeTheConfig(t *testing.T) {
	t.Parallel()
	var files []traceEvent
	for _, p := range []string{"/a", "/b", "/c", "/d"} {
		files = append(files, traceEvent{kind: "file", op: "read", path: p, result: "ok"})
	}
	runs := []entryRun{{ep: entryPoint{Kind: "hook", Name: "SessionStart", Command: "true"}, ran: true, files: files}}
	findings := [][]finding{nil}

	var text strings.Builder
	writeScanReport(&text, nil, runs, findings, verdictPass, nil, 2)
	if !strings.Contains(text.String(), "… 2 more") {
		t.Errorf("max_listed 2: report\n%s\nwant the list cut after two", text.String())
	}

	var out bytes.Buffer
	if err := writeScanJSON(&out, "github.com/o/r@abc", nil, runs, findings, verdictPass, nil, []string{"severity: connect LOW"}); err != nil {
		t.Fatal(err)
	}
	var report struct {
		Target string `json:"target"`
		Config struct {
			Overrides []string `json:"overrides"`
		} `json:"config"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil || report.Target != "github.com/o/r@abc" || !slices.Equal(report.Config.Overrides, []string{"severity: connect LOW"}) {
		t.Errorf("JSON %s: %v", out.String(), err)
	}
	out.Reset()
	if err := writeScanJSON(&out, "x", nil, runs, findings, verdictPass, nil, nil); err != nil || strings.Contains(out.String(), `"config"`) {
		t.Errorf("no overrides: %s, %v; want no config object", out.String(), err)
	}
}
