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
	if cfg.devCommand() != "claude" || len(cfg.Egress) == 0 {
		t.Errorf("starter config %+v: want claude as the command and its API host in egress", cfg)
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
	if err := os.WriteFile(user, []byte("egress: [api.anthropic.com]\nsecrets: {CLAUDE_CODE_OAUTH_TOKEN: env:CLAUDE_CODE_OAUTH_TOKEN}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sbox.yaml"), []byte("ports: [3000]\nsecrets: {GITHUB_TOKEN: env:GH_SBOX}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lookup := func(k string) (string, bool) { return "secret-value", k == "CLAUDE_CODE_OAUTH_TOKEN" }
	var stdout, stderr bytes.Buffer
	if code := configMain([]string{dir}, &stdout, &stderr, devHost{userConfig: user, lookupEnv: lookup}); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"api.anthropic.com:443", "3000", "CLAUDE_CODE_OAUTH_TOKEN: env:CLAUDE_CODE_OAUTH_TOKEN # set", "GITHUB_TOKEN: env:GH_SBOX # unset on this Mac"} {
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
