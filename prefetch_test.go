package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRunnerPackage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		cmd         string
		args        []string
		wantRunner  string
		wantPackage string
	}{
		{cmd: "npx", args: []string{"-y", "@scope/server@1.2.0", "--root", "x"}, wantRunner: "npx", wantPackage: "@scope/server@1.2.0"},
		{cmd: "/usr/local/bin/npx", args: []string{"--package=mcp-x", "mcp-x-cli"}, wantRunner: "npx", wantPackage: "mcp-x"},
		{cmd: "uvx", args: []string{"--from", "mcp-tool==0.3", "mcp-tool"}, wantRunner: "uvx", wantPackage: "mcp-tool==0.3"},
		{cmd: "uvx", args: []string{"mcp-server-fetch"}, wantRunner: "uvx", wantPackage: "mcp-server-fetch"},
		{cmd: "node", args: []string{"server.js"}},
	}

	for _, tt := range tests {
		t.Run(tt.cmd+" "+strings.Join(tt.args, " "), func(t *testing.T) {
			t.Parallel()
			runner, pkg := runnerPackage(tt.cmd, tt.args)
			if runner != tt.wantRunner || pkg != tt.wantPackage {
				t.Errorf("runnerPackage = %q, %q; want %q, %q", runner, pkg, tt.wantRunner, tt.wantPackage)
			}
		})
	}
}

func TestPlanPrefetch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for name, body := range map[string]string{
		"package.json":      `{"dependencies":{"is-number":"7.0.0"}}`,
		"package-lock.json": `{}`,
		"requirements.txt":  "six==1.16.0\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	eps := []entryPoint{
		{Kind: "mcp", Runner: "npx", Package: "it's-pkg"},
		{Kind: "mcp", Runner: "uvx", Package: "tool"},
		{Kind: "hook"},
	}

	steps := planPrefetch(dir, eps)
	var names []string
	for _, s := range steps {
		names = append(names, s.Name)
	}
	if want := []string{"npm", "pip:requirements.txt", "npx:it's-pkg", "uvx:tool"}; !slices.Equal(names, want) {
		t.Fatalf("steps = %v, want %v", names, want)
	}
	if steps[0].Command != "npm ci && cp -a node_modules /mnt/" {
		t.Errorf("npm step = %q, want npm ci (lockfile present)", steps[0].Command)
	}

	script := prefetchScript(steps)
	for _, want := range []string{
		"npm_config_ignore_scripts=true",
		"UV_NO_BUILD=1",
		"PIP_ONLY_BINARY=:all:",
		`HTTPS_PROXY=$proxy`,
		`step 'npx:it'\''s-pkg' ''\''npm'\'' '\''exec'\'' '\''--yes'\'' '\''--package=it'\''\'\'''\''s-pkg'\''`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("prefetch script lacks %q", want)
		}
	}

	// node_modules bundled: nothing to install.
	if err := os.Mkdir(filepath.Join(dir, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if hasNPMDependencies(dir) {
		t.Error("hasNPMDependencies = true with node_modules bundled")
	}
}

func TestCollectPrefetch(t *testing.T) {
	t.Parallel()

	steps := []prefetchStep{{Name: "npm"}, {Name: "pip:requirements.txt"}, {Name: "npx:x"}}
	markers := []string{
		"PREFETCH-BEGIN",
		"STEP npm",
		"STEP-RESULT npm 0",
		"STEP-RESULT pip:requirements.txt 1",
		"FETCH ALLOW registry.npmjs.org:443",
		"FETCH ALLOW registry.npmjs.org:443",
		"FETCH DENY codeload.github.com:443",
		"FETCH DENY codeload.github.com:443",
		"RESULT 1 0", // a scan marker: ignored here
	}
	res := collectPrefetch(steps, markers)

	if !res.Ran || res.TimedOut {
		t.Errorf("ran %v timed out %v, want ran and not timed out", res.Ran, res.TimedOut)
	}
	if len(res.Fetched) != 1 || res.Fetched[0] != (fetchCount{Target: "registry.npmjs.org:443", Count: 2}) {
		t.Errorf("fetched = %+v", res.Fetched)
	}
	if !slices.Equal(res.Refused, []string{"codeload.github.com:443"}) {
		t.Errorf("refused = %v", res.Refused)
	}
	var rules []string
	for _, f := range res.Findings {
		rules = append(rules, f.Rule)
	}
	if want := []string{"prefetch-refused", "prefetch-failed"}; !slices.Equal(rules, want) {
		t.Errorf("findings = %v, want %v", rules, want)
	}
	if res.Steps[2].Done {
		t.Error("npx:x reported done without a result")
	}
}

func TestProxySelftest(t *testing.T) {
	t.Parallel()

	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	out, err := exec.Command(python, "-I", "scripts/sbox-proxy.py", "--selftest").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Errorf("sbox-proxy.py --selftest: %v: %s", err, out)
	}
}
