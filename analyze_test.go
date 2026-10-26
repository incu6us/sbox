package main

import (
	"slices"
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
			got := analyze(tt.give)
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
