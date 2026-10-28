package main

import (
	"os"
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

func TestWorkspaceFor(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	for _, d := range []string{"work/client-a/api", "work/client-a/inner/x", "work/client-ab", "src/p", "Downloads/r"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(home, "work", "client-a"), filepath.Join(home, "linked")); err != nil {
		t.Fatal(err)
	}
	cfg := config{Workspaces: map[string]workspace{
		"client-a": {Paths: []string{"~/work/client-a/"}},
		"inner":    {Paths: []string{filepath.Join(home, "work", "client-a", "inner")}},
		"personal": {Paths: []string{"~/src"}},
		"via-link": {Paths: []string{"~/linked/api"}},
	}}
	for dir, want := range map[string]string{
		"work/client-a":         "client-a",
		"work/client-a/inner/x": "inner",
		"work/client-ab":        "default", // a prefix of the name isn't the folder
		"src/p":                 "personal",
		"Downloads/r":           "default",
		"work/client-a/api":     "via-link", // the link resolved: longer than client-a's
		"linked/inner/x":        "inner",
	} {
		if got, err := cfg.workspaceFor(filepath.Join(home, dir), "", home); err != nil || got != want {
			t.Errorf("workspaceFor(%s) = %q, %v; want %q", dir, got, err, want)
		}
	}
	if got, err := cfg.workspaceFor(filepath.Join(home, "Downloads/r"), "personal", home); err != nil || got != "personal" {
		t.Errorf("-workspace personal: %q, %v", got, err)
	}
	if got, err := cfg.workspaceFor(filepath.Join(home, "src/p"), "default", home); err != nil || got != "default" {
		t.Errorf("-workspace default: %q, %v", got, err)
	}
	if _, err := cfg.workspaceFor(filepath.Join(home, "src/p"), "nope", home); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("-workspace nope: %v, want a refusal naming it", err)
	}
}

func TestAllowsEgress(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		allow []string
		req   string
		want  bool
	}{
		{[]string{"registry.npmjs.org:443"}, "registry.npmjs.org:443", true},
		{[]string{"registry.npmjs.org:443"}, "registry.npmjs.org:80", false},
		{[]string{"*.github.com:443"}, "api.github.com:443", true},
		{[]string{"*.github.com:443"}, "github.com:443", false},
		{[]string{"*.github.com:443"}, "evilgithub.com:443", false},
		{[]string{"github.com:*"}, "github.com:22", true},
		{[]string{"*.a.com:*"}, "x.y.a.com:8443", true},
		{[]string{"*.a.com:443"}, "*.b.a.com:443", true},
		{[]string{"*.b.a.com:443"}, "*.a.com:443", false},
		{[]string{"a.com:443"}, "*.a.com:443", false},
		{[]string{"github.com:443"}, "github.com:*", false},
		{[]string{"github.com:*"}, "github.com:*", true},
		{[]string{"*"}, "anything.example:22", true},
		{nil, "x.example:443", false},
	} {
		if got := allowsEgress(tt.allow, tt.req); got != tt.want {
			t.Errorf("allowsEgress(%q, %q) = %v, want %v", tt.allow, tt.req, got, tt.want)
		}
	}
}

func TestAllowsServiceSecretMount(t *testing.T) {
	t.Parallel()
	if !allowsService([]string{"5432"}, 5432) || allowsService([]string{"5432"}, 6379) || !allowsService([]string{"*"}, 1) {
		t.Error("allowsService")
	}
	if !allowsSecret([]string{"CLIENT_A_NPM_TOKEN"}, "env:CLIENT_A_NPM_TOKEN") || allowsSecret([]string{"CLIENT_A_NPM_TOKEN"}, "env:AWS_SECRET_ACCESS_KEY") {
		t.Error("allowsSecret")
	}

	home := t.TempDir()
	for _, d := range []string{"work/shared/sub", "work/out", "elsewhere"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(home, "work"), filepath.Join(home, "w")); err != nil {
		t.Fatal(err)
	}
	ro, rw := []string{"~/w/shared/"}, []string{"~/work/out"}
	for _, tt := range []struct {
		host string
		rw   bool
		want bool
	}{
		{"~/work/shared", false, true},
		{"~/work/shared/sub", false, true},
		{"~/work/shared/sub", true, false}, // read-only allowance, rw asked
		{"~/work/out", true, true},
		{"~/work/out", false, true}, // rw covers read-only
		{"~/elsewhere", false, false},
		{"~/.ssh", false, false},
		{"~/work/shared/../../elsewhere", false, false},
	} {
		if got := allowsMount(ro, rw, tt.host, tt.rw, home); got != tt.want {
			t.Errorf("allowsMount(%s, rw %v) = %v, want %v", tt.host, tt.rw, got, tt.want)
		}
	}
}
