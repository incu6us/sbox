package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestWorkspacesKey(t *testing.T) {
	t.Parallel()
	user := filepath.Join(t.TempDir(), "config.yaml")
	writeTestFile(t, user, `workspaces:
  client-a:
    paths: [~/work/client-a/]
    egress: ["*.client-a.com"]
    mounts: [{host: ~/.config/client-a/gitconfig, guest: /home/runner/.gitconfig}]
    secrets: {GITHUB_TOKEN: "env:CLIENT_A_GITHUB_TOKEN"}
    services: [5432]
    allow:
      egress: [registry.npmjs.org, "*.github.com", "github.com:22"]
      services: ["6379", "*"]
      mounts: [~/work/client-a/shared/]
      rw: [/tmp/out/]
      secrets: [CLIENT_A_NPM_TOKEN]
`)
	cfg, err := loadConfig(user, "")
	if err != nil {
		t.Fatal(err)
	}
	ws := cfg.Workspaces["client-a"]
	if !slices.Equal(ws.Paths, []string{"~/work/client-a/"}) || !slices.Equal(ws.Egress, []string{"*.client-a.com:443"}) ||
		ws.Secrets["GITHUB_TOKEN"] != "env:CLIENT_A_GITHUB_TOKEN" || !slices.Equal(ws.Services, []int{5432}) || len(ws.Mounts) != 1 {
		t.Errorf("grants = %+v", ws)
	}
	if !slices.Equal(ws.Allow.Egress, []string{"registry.npmjs.org:443", "*.github.com:443", "github.com:22"}) ||
		!slices.Equal(ws.Allow.Services, []string{"6379", "*"}) || !slices.Equal(ws.Allow.Secrets, []string{"CLIENT_A_NPM_TOKEN"}) {
		t.Errorf("allow = %+v", ws.Allow)
	}

	project := filepath.Join(t.TempDir(), "sbox.yaml")
	writeTestFile(t, project, "workspaces: {x: {paths: [/tmp]}}\n")
	if _, err := loadConfig(user, project); err == nil || !strings.Contains(err.Error(), "your own") {
		t.Errorf("workspaces in a project file: %v, want a refusal", err)
	}
	for _, bad := range []string{
		"workspaces: {Bad_Name: {}}",
		"workspaces: {a: {paths: [relative/dir]}}",
		"workspaces: {a: {egress: [\"https://x.example\"]}}",
		"workspaces: {a: {allow: {egress: [\"x example\"]}}}",
		"workspaces: {a: {allow: {services: [\"http\"]}}}",
		"workspaces: {a: {allow: {mounts: [relative]}}}",
		"workspaces: {a: {allow: {rw: [relative]}}}",
		"workspaces: {a: {allow: {secrets: [\"1BAD\"]}}}",
		"workspaces: {a: {mounts: [{host: /tmp/x, guest: /etc/x}]}}",
	} {
		writeTestFile(t, user, bad+"\n")
		if _, err := loadConfig(user, ""); err == nil {
			t.Errorf("%s: loaded, want a refusal", bad)
		}
	}
}
