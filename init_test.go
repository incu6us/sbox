package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitMain(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := initMain([]string{dir}, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	file := filepath.Join(dir, "sbox.yaml")
	cfg, err := loadConfig("", file)
	if err != nil {
		t.Fatalf("the starter file doesn't load: %v", err)
	}
	if cfg.devCommand() != "claude" {
		t.Errorf("starter config %+v: want claude as the command", cfg)
	}
	if _, _, misses, err := layered("", file, dir, "", t.TempDir(), nil); err != nil || len(misses) != 0 {
		t.Errorf("the starter asks for %v, %v; want nothing a workspace must allow, so sbox dev starts", misses, err)
	}
	if !strings.Contains(stdout.String(), "~/.config/sbox/config.yaml") || !strings.Contains(stdout.String(), "api.anthropic.com") {
		t.Errorf("sbox init says %q; want where the agent's host and token go", stdout.String())
	}
	starter, _ := os.ReadFile(file)
	for _, key := range []string{"secrets:", "ports:", "services:", "setup:", "start:", "mcp:", "permissions:"} {
		if !strings.Contains(string(starter), key) {
			t.Errorf("starter file lacks a commented %s", key)
		}
	}

	if err := os.WriteFile(file, []byte("ports: [3000]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := initMain([]string{dir}, &stdout, &stderr); code != exitUsage || !strings.Contains(stderr.String(), "exists") {
		t.Errorf("existing file: exit %d, stderr %q; want a refusal", code, stderr.String())
	}
	if data, _ := os.ReadFile(file); string(data) != "ports: [3000]\n" {
		t.Errorf("the existing file was changed: %q", data)
	}
}

func TestConfigMain(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	user := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(user, []byte("egress: [api.anthropic.com]\nsecrets: {CLAUDE_CODE_OAUTH_TOKEN: env:CLAUDE_CODE_OAUTH_TOKEN}\n"+_allowAll), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sbox.yaml"), []byte("ports: [3000]\nsecrets: {GITHUB_TOKEN: env:GH_SBOX}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, ".gitignore"), ".env\n")
	writeTestFile(t, filepath.Join(dir, ".env"), "X=1\n")
	lookup := func(k string) (string, bool) { return "secret-value", k == "CLAUDE_CODE_OAUTH_TOKEN" }
	var stdout, stderr bytes.Buffer
	if code := configMain([]string{dir}, &stdout, &stderr, devHost{userConfig: user, lookupEnv: lookup}); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"api.anthropic.com:443", "3000", "CLAUDE_CODE_OAUTH_TOKEN: env:CLAUDE_CODE_OAUTH_TOKEN # set", "GITHUB_TOKEN: env:GH_SBOX # unset on this Mac", "# ports    3000 → the Mac's localhost:3000",
		"# left out by .gitignore: .env"} {
		if !strings.Contains(out, want) {
			t.Errorf("sbox config lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "secret-value") {
		t.Errorf("sbox config printed a secret's value:\n%s", out)
	}

	if err := os.WriteFile(filepath.Join(dir, "sbox.yaml"), []byte("prots: [1]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := configMain([]string{dir}, &stdout, &stderr, devHost{userConfig: user, lookupEnv: lookup}); code != exitUsage || !strings.Contains(stderr.String(), "prots") {
		t.Errorf("bad file: exit %d, stderr %q", code, stderr.String())
	}
}

// TestConfigMainSources: each key in sbox config's output names the file
// it came from; a list both files add to names both.
func TestConfigMainSources(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	user := filepath.Join(t.TempDir(), "config.yaml")
	writeTestFile(t, user, "egress: [a.example]\ntimeouts: {run: 2m}\nsecrets: {CLAUDE_CODE_OAUTH_TOKEN: env:TOKEN}\n"+_allowAll)
	writeTestFile(t, filepath.Join(dir, "sbox.yaml"), "egress: [b.example]\nports: [3000]\ntimeouts: {setup: 5m}\nsecrets: {GITHUB_TOKEN: env:GH}\n")
	var stdout, stderr bytes.Buffer
	lookup := func(k string) (string, bool) { return "", k == "TOKEN" }
	if code := configMain([]string{dir}, &stdout, &stderr, devHost{userConfig: user, lookupEnv: lookup}); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"ports: # project", "egress: # user, project", "  run: 2m0s # user", "  setup: 5m0s # project",
		"CLAUDE_CODE_OAUTH_TOKEN: env:TOKEN # set (user)", "GITHUB_TOKEN: env:GH # unset on this Mac (project)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("sbox config lacks %q:\n%s", want, out)
		}
	}
}
