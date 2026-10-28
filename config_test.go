package main

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestLoadConfig(t *testing.T) {
	t.Parallel()
	write := func(t *testing.T, body string) string {
		p := filepath.Join(t.TempDir(), "sbox.yaml")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	missing := filepath.Join(t.TempDir(), "none.yaml")

	t.Run("none", func(t *testing.T) {
		t.Parallel()
		cfg, err := loadConfig(missing, missing)
		if err != nil || len(cfg.Ports)+len(cfg.Services) != 0 {
			t.Errorf("loadConfig with no files = %+v, %v; want empty", cfg, err)
		}
	})

	t.Run("lists append, duplicates once", func(t *testing.T) {
		t.Parallel()
		user := write(t, "services: [5432]\nports: [8080]\n")
		project := write(t, "# the app\nports: [3000, 8080]\nservices: [6379]\n")
		cfg, err := loadConfig(user, project)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(cfg.Ports, []int{3000, 8080}) || !slices.Equal(cfg.Services, []int{5432, 6379}) {
			t.Errorf("loadConfig = %+v, want ports [3000 8080], services [5432 6379]", cfg)
		}
	})

	for _, tt := range []struct {
		name, body, wantErr string
	}{
		{"unknown key", "prots: [3000]\n", "prots"},
		{"privileged port", "ports: [80]\n", "1024"},
		{"port out of range", "ports: [70000]\n", "65535"},
		{"service out of range", "services: [0]\n", "1 to 65535"},
		{"not yaml", "ports: [3000\n", "sbox.yaml"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			project := write(t, tt.body)
			_, err := loadConfig(missing, project)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !strings.Contains(err.Error(), project) {
				t.Errorf("err = %v, want one naming the file and %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadConfigEgress(t *testing.T) {
	t.Parallel()
	write := func(t *testing.T, body string) string {
		p := filepath.Join(t.TempDir(), "sbox.yaml")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	user := write(t, "egress: [api.anthropic.com]\n")
	project := write(t, "egress: [\"*.github.com\", github.com, \"github.com:22\", \"Example.org:*\", api.anthropic.com]\n")
	cfg, err := loadConfig(user, project)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"*.github.com:443", "api.anthropic.com:443", "example.org:*", "github.com:22", "github.com:443"}
	if !slices.Equal(cfg.Egress, want) {
		t.Errorf("egress = %q, want %q", cfg.Egress, want)
	}
	if got := cfg.rawEgress(); !slices.Equal(got, []string{"github.com:22"}) {
		t.Errorf("rawEgress = %q, want the exact host on a port other than 443", got)
	}

	for _, tt := range []struct{ name, body, wantErr string }{
		{"a URL", "egress: [\"https://github.com\"]\n", "https://github.com"},
		{"wildcard inside", "egress: [\"a.*.com\"]\n", "a.*.com"},
		{"port zero", "egress: [\"a.com:0\"]\n", "a.com:0"},
		{"two hosts on one raw port", "egress: [\"github.com:22\", \"gitlab.com:22\"]\n", "one host per port"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := loadConfig(filepath.Join(t.TempDir(), "none"), write(t, tt.body)); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want one naming %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadConfigImage(t *testing.T) {
	t.Parallel()
	write := func(t *testing.T, body string) string {
		p := filepath.Join(t.TempDir(), "sbox.yaml")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	missing := filepath.Join(t.TempDir(), "none.yaml")
	cfg, err := loadConfig(write(t, "image:\n  packages: [rustc, golang-go, g++, golang-go]\n"), missing)
	if err != nil || !slices.Equal(cfg.Image.Packages, []string{"g++", "golang-go", "rustc"}) {
		t.Errorf("image.packages = %q, %v; want them sorted, once each", cfg.Image.Packages, err)
	}

	for _, tt := range []struct{ name, user, project, wantErr string }{
		{"a comma would add another package", "image: {packages: [\"git,curl\"]}\n", "", "git,curl"},
		{"an option", "image: {packages: [\"--allow-unauthenticated\"]}\n", "", "--allow-unauthenticated"},
		{"a URL", "image: {packages: [\"https://example.com/x.deb\"]}\n", "", "x.deb"},
		{"in a project's file", "", "image: {packages: [golang-go]}\n", "~/.config/sbox/config.yaml"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			user, project := missing, missing
			if tt.user != "" {
				user = write(t, tt.user)
			}
			if tt.project != "" {
				project = write(t, tt.project)
			}
			if _, err := loadConfig(user, project); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want one naming %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadConfigDevHooks(t *testing.T) {
	t.Parallel()
	write := func(t *testing.T, body string) string {
		p := filepath.Join(t.TempDir(), "sbox.yaml")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	user := write(t, "dev:\n  env: {EDITOR: vi, NODE_ENV: production}\n  setup: [corepack enable]\ntimeouts: {setup: 5m}\n")
	project := write(t, "dev:\n  env: {NODE_ENV: development}\n  setup: [npm ci, npm ci]\n  start: [\"npm run dev &\"]\ntimeouts: {setup: 20m}\n")
	cfg, err := loadConfig(user, project)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(cfg.Dev.Env, map[string]string{"EDITOR": "vi", "NODE_ENV": "development"}) {
		t.Errorf("dev.env = %v, want both files', the project's winning", cfg.Dev.Env)
	}
	if !slices.Equal(cfg.Dev.Setup, []string{"corepack enable", "npm ci", "npm ci"}) || !slices.Equal(cfg.Dev.Start, []string{"npm run dev &"}) {
		t.Errorf("dev.setup %q, dev.start %q; want steps appended in order, repeats kept", cfg.Dev.Setup, cfg.Dev.Start)
	}
	if cfg.Timeouts.Setup != 20*time.Minute {
		t.Errorf("timeouts.setup = %v, want the project's 20m", cfg.Timeouts.Setup)
	}
	if none, _ := loadConfig(filepath.Join(t.TempDir(), "none"), ""); none.setupTimeout() != 10*time.Minute {
		t.Errorf("default setup timeout = %v, want 10m", none.setupTimeout())
	}

	for _, tt := range []struct{ name, body, wantErr string }{
		{"a variable name with a dash", "dev: {env: {MY-VAR: x}}\n", "MY-VAR"},
		{"a value over two lines", "dev: {env: {A: \"x\\ny\"}}\n", "one line"},
		{"an empty step", "dev: {setup: [\"\"]}\n", "empty"},
		{"a negative timeout", "timeouts: {setup: -1m}\n", "timeouts.setup"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := loadConfig(filepath.Join(t.TempDir(), "none"), write(t, tt.body)); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want one naming %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadConfigSecrets(t *testing.T) {
	t.Parallel()
	write := func(t *testing.T, body string) string {
		p := filepath.Join(t.TempDir(), "sbox.yaml")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	user := write(t, "secrets: {CLAUDE_CODE_OAUTH_TOKEN: env:CLAUDE_CODE_OAUTH_TOKEN, GITHUB_TOKEN: env:GH_ALL}\n")
	project := write(t, "secrets: {GITHUB_TOKEN: env:GH_REPO}\n")
	cfg, err := loadConfig(user, project)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(cfg.Secrets, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "env:CLAUDE_CODE_OAUTH_TOKEN", "GITHUB_TOKEN": "env:GH_REPO"}) {
		t.Errorf("secrets = %v, want both files', the project's winning", cfg.Secrets)
	}

	host := map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat-1", "GH_REPO": "ghp_2"}
	lookup := func(k string) (string, bool) { v, ok := host[k]; return v, ok }
	got, err := cfg.secretValues(lookup)
	if err != nil || !maps.Equal(got, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat-1", "GITHUB_TOKEN": "ghp_2"}) {
		t.Errorf("secretValues = %v, %v; want the host's values", got, err)
	}
	delete(host, "GH_REPO")
	if _, err := cfg.secretValues(lookup); err == nil || !strings.Contains(err.Error(), "GH_REPO") || !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Errorf("a missing host variable: err %v, want both names", err)
	}
	host["GH_REPO"] = "two\nlines"
	if _, err := cfg.secretValues(lookup); err == nil || strings.Contains(err.Error(), "two") {
		t.Errorf("a two-line value: err %v, want a refusal that doesn't show the value", err)
	}

	for _, tt := range []struct{ name, body, wantErr, notInErr string }{
		{"a literal value", "secrets: {GITHUB_TOKEN: ghp_literal}\n", "env:", "ghp_literal"},
		{"a bad guest name", "secrets: {GITHUB-TOKEN: env:X}\n", "GITHUB-TOKEN", ""},
		{"a bad host name", "secrets: {A: \"env:X Y\"}\n", "env:", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := loadConfig(filepath.Join(t.TempDir(), "none"), write(t, tt.body))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) || (tt.notInErr != "" && strings.Contains(err.Error(), tt.notInErr)) {
				t.Errorf("err = %v, want one naming %q and not showing %q", err, tt.wantErr, tt.notInErr)
			}
		})
	}
}
