package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindEntryPoints(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	files := map[string]string{
		"hooks/hooks.json": `{"hooks":{"SessionStart":[{"matcher":"startup","hooks":[{"type":"command","command":"node \"${CLAUDE_PLUGIN_ROOT}/a.js\""}]}],
			"UserPromptSubmit":[{"hooks":[{"type":"command","command":"node b.js"}]}]}}`,
		".cursor/hooks.json":          `{"version":1,"hooks":{"beforeShellExecution":[{"command":"./guard.sh"}]}}`,
		".mcp.json":                   `{"mcpServers":{"files":{"command":"npx","args":["-y","pkg@1.0.0","--root","a b"]},"remote":{"url":"https://x.io/mcp"}}}`,
		"package.json":                `{"scripts":{"postinstall":"node setup.js","test":"jest"}}`,
		"setup.py":                    "",
		"node_modules/dep/hooks.json": `{"hooks":{"SessionStart":[{"command":"skipped"}]}}`,
	}
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	eps, err := findEntryPoints(dir)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		want    string // entryPoint.String()
		command string
		stdin   string // substring
	}{
		{want: "hook SessionStart (hooks/hooks.json)", command: `node "${CLAUDE_PLUGIN_ROOT}/a.js"`, stdin: `"source":"startup"`},
		{want: "hook UserPromptSubmit (hooks/hooks.json)", command: "node b.js", stdin: `"prompt":"hello"`},
		{want: "hook beforeShellExecution (.cursor/hooks.json)", command: "./guard.sh", stdin: `"command":"ls"`},
		{want: "mcp files (.mcp.json)", command: `'npx' '-y' 'pkg@1.0.0' '--root' 'a b'`, stdin: `"method":"tools/list"`},
		{want: "npm postinstall (package.json)", command: "npm run-script postinstall"},
		{want: "setup.py egg_info (setup.py)", command: "python3 setup.py egg_info"},
	}
	got := make(map[string]entryPoint, len(eps))
	for _, ep := range eps {
		got[ep.String()] = ep
	}
	if len(eps) != len(tests) {
		t.Errorf("found %d entry points, want %d: %v", len(eps), len(tests), eps)
	}
	for _, tt := range tests {
		ep, ok := got[tt.want]
		switch {
		case !ok:
			t.Errorf("missing %s", tt.want)
		case ep.Command != tt.command:
			t.Errorf("%s: command %q, want %q", tt.want, ep.Command, tt.command)
		case !strings.Contains(ep.Stdin, tt.stdin):
			t.Errorf("%s: stdin %q, want it to contain %q", tt.want, ep.Stdin, tt.stdin)
		}
	}
}
