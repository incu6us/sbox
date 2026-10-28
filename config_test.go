package main

import (
	"encoding/json"
	"fmt"
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

func TestLoadConfigDevSize(t *testing.T) {
	t.Parallel()
	write := func(t *testing.T, body string) string {
		p := filepath.Join(t.TempDir(), "sbox.yaml")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cfg, err := loadConfig(write(t, "dev: {vcpus: 2, memory_mib: 2048}\n"), write(t, "dev: {vcpus: 4}\n"))
	if err != nil || cfg.Dev.VCPUs != 4 || cfg.Dev.MemoryMiB != 2048 {
		t.Errorf("dev size = %d vCPUs, %d MiB, %v; want the project's vcpus and the user's memory", cfg.Dev.VCPUs, cfg.Dev.MemoryMiB, err)
	}
	if job := devJob(cfg, nil); job.vcpus != 4 || job.memoryMiB != 2048 {
		t.Errorf("devJob size = %d, %d; want the config's", job.vcpus, job.memoryMiB)
	}
	for _, body := range []string{"dev: {vcpus: 64}\n", "dev: {vcpus: -1}\n", "dev: {memory_mib: 64}\n", "dev: {memory_mib: 1000000}\n"} {
		if _, err := loadConfig(filepath.Join(t.TempDir(), "none"), write(t, body)); err == nil || !strings.Contains(err.Error(), "dev.") {
			t.Errorf("%q: err %v, want a refusal naming the key", body, err)
		}
	}
}

func TestLoadConfigAgent(t *testing.T) {
	t.Parallel()
	write := func(t *testing.T, body string) string {
		p := filepath.Join(t.TempDir(), "sbox.yaml")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	user := write(t, `dev: {command: claude}
permissions: {deny: ["Bash(rm -rf *)"]}
mcp:
  servers:
    docs: {url: "https://mcp.example.com/mcp"}
`)
	project := write(t, `permissions: {allow: ["Bash(npm run *)"], deny: [WebFetch]}
mcp:
  servers:
    pg: {command: npx, args: ["-y", "@modelcontextprotocol/server-postgres"], env: {PGHOST: host.sbox}}
  allow: ["mcp__pg__query"]
  deny: ["mcp__pg__execute"]
`)
	cfg, err := loadConfig(user, project)
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(cfg.claudeSettings(), &want); err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(want)
	expect := `{"mcpServers":{"docs":{"type":"http","url":"https://mcp.example.com/mcp"},` +
		`"pg":{"args":["-y","@modelcontextprotocol/server-postgres"],"command":"npx","env":{"PGHOST":"host.sbox"}}},` +
		`"settings":{"permissions":{"allow":["Bash(npm run *)","mcp__pg__query"],"defaultMode":"bypassPermissions","deny":["Bash(rm -rf *)","WebFetch","mcp__pg__execute"]},` +
		`"skipDangerousModePermissionPrompt":true}}`
	if string(got) != expect {
		t.Errorf("claudeSettings =\n%s\nwant\n%s", got, expect)
	}
	if cfg.devCommand() != "claude" {
		t.Errorf("devCommand = %q, want claude", cfg.devCommand())
	}

	ask, err := loadConfig(write(t, "dev: {permissions: ask}\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	if s := string(ask.claudeSettings()); !strings.Contains(s, `"defaultMode":"default"`) || strings.Contains(s, "skipDangerous") {
		t.Errorf("permissions ask: %s, want the default mode and no prompt skipping", s)
	}
	if ask.devCommand() != "" {
		t.Errorf("devCommand unset = %q, want empty (a login shell)", ask.devCommand())
	}

	for _, tt := range []struct{ name, body, wantErr string }{
		{"permissions neither", "dev: {permissions: maybe}\n", "dev.permissions"},
		{"server with both", "mcp: {servers: {x: {command: a, url: \"https://a\"}}}\n", "mcp.servers.x"},
		{"server with neither", "mcp: {servers: {x: {args: [a]}}}\n", "mcp.servers.x"},
		{"server url not http", "mcp: {servers: {x: {url: \"file:///etc/passwd\"}}}\n", "mcp.servers.x"},
		{"server env from the host", "mcp: {servers: {x: {command: a, env: {T: \"env:TOKEN\"}}}}\n", "secrets"},
		{"server name", "mcp: {servers: {\"a b\": {command: a}}}\n", "a b"},
		{"empty rule", "permissions: {deny: [\"\"]}\n", "empty"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := loadConfig(filepath.Join(t.TempDir(), "none"), write(t, tt.body)); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want one naming %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadConfigMounts(t *testing.T) {
	t.Parallel()
	write := func(t *testing.T, body string) string {
		p := filepath.Join(t.TempDir(), "sbox.yaml")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	user := write(t, "mounts:\n  - {host: ~/.gitconfig, guest: /home/runner/.gitconfig}\n")
	project := write(t, "mounts:\n  - {host: ~/notes, guest: /home/runner/notes, mode: ro}\n")
	cfg, err := loadConfig(user, project)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Mounts) != 2 || cfg.Mounts[0].Host != "~/.gitconfig" || cfg.Mounts[1].Guest != "/home/runner/notes" {
		t.Errorf("mounts = %+v, want both files', in order", cfg.Mounts)
	}

	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	srcs, err := cfg.mountSources(home)
	real, _ := filepath.EvalSymlinks(home) // macOS: /var is /private/var
	if err != nil || len(srcs) != 2 || srcs[0] != filepath.Join(real, ".gitconfig") || srcs[1] != filepath.Join(real, "notes") {
		t.Errorf("mountSources = %q, %v; want ~ expanded", srcs, err)
	}
	if err := os.Remove(filepath.Join(home, ".gitconfig")); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.mountSources(home); err == nil || !strings.Contains(err.Error(), "~/.gitconfig") || !strings.Contains(err.Error(), "no such") {
		t.Errorf("a missing host path: err %v, want it named", err)
	}

	for _, tt := range []struct{ name, body, wantErr string }{
		{"rw", "mounts: [{host: /tmp/x, guest: /home/runner/x, mode: rw}]\n", "sbox sync"},
		{"outside the home", "mounts: [{host: /tmp/x, guest: /etc/x}]\n", "/home/runner"},
		{"the home itself", "mounts: [{host: /tmp/x, guest: /home/runner}]\n", "/home/runner"},
		{"the project", "mounts: [{host: /tmp/x, guest: /home/runner/work/x}]\n", "project"},
		{"dot dot", "mounts: [{host: /tmp/x, guest: /home/runner/../../etc}]\n", "/home/runner"},
		{"relative host", "mounts: [{host: x, guest: /home/runner/x}]\n", "absolute"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := loadConfig(filepath.Join(t.TempDir(), "none"), write(t, tt.body)); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want one naming %q", err, tt.wantErr)
			}
		})
	}
}

func TestBootKeys(t *testing.T) {
	t.Parallel()
	var a config
	a.Egress, a.Ports = []string{"api.anthropic.com:443"}, []int{3000}
	a.Dev.Env = map[string]string{"A": "1"}
	a.Dev.Command = "claude"
	a.Secrets = map[string]string{"T": "env:T"}
	b := a
	b.Egress = []string{"api.anthropic.com:443", "registry.npmjs.org:443"}
	b.Dev.Command = "bash" // applied at every attach: not a boot key
	b.Dev.Env = map[string]string{"A": "2"}
	b.Ports = slices.Clone(a.Ports)
	if got := changedKeys(a.bootKeys(), b.bootKeys()); !slices.Equal(got, []string{"dev.env", "egress"}) {
		t.Errorf("changedKeys = %q, want dev.env and egress", got)
	}
	if got := changedKeys(a.bootKeys(), a.bootKeys()); len(got) != 0 {
		t.Errorf("same config: %q", got)
	}
	if !strings.Contains(fmt.Sprint(a.bootKeys()), "env:T") {
		t.Error("secrets should be compared by name and source")
	}
}

func TestLoadConfigDevIgnore(t *testing.T) {
	t.Parallel()
	write := func(t *testing.T, body string) string {
		p := filepath.Join(t.TempDir(), "sbox.yaml")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cfg, err := loadConfig(write(t, "dev: {ignore: [\"*.log\"]}\n"), write(t, "dev: {ignore: [dist, build/cache, /out, \"!.env.example\"]}\n"))
	if err != nil || !slices.Equal(cfg.Dev.Ignore, []string{"*.log", "dist", "build/cache", "/out", "!.env.example"}) {
		t.Errorf("dev.ignore = %q, %v; want both files', in order", cfg.Dev.Ignore, err)
	}
	for _, bad := range []string{"../up", "//abs", "/", "!", "!../up", "a/../b", "", "[", "./x"} {
		if _, err := loadConfig("", write(t, "dev: {ignore: [\""+bad+"\"]}\n")); err == nil || !strings.Contains(err.Error(), "dev.ignore") {
			t.Errorf("dev.ignore %q: err %v, want a refusal", bad, err)
		}
	}
}

func TestLoadConfigPauseAfter(t *testing.T) {
	t.Parallel()
	write := func(t *testing.T, body string) string {
		p := filepath.Join(t.TempDir(), "sbox.yaml")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cfg, err := loadConfig(write(t, "dev: {pause_after: 30m}\n"), "")
	if err != nil || cfg.Dev.PauseAfter != 30*time.Minute || devJob(cfg, nil).pauseAfter != 30*time.Minute {
		t.Errorf("pause_after = %v, %v; want 30m, in the job", cfg.Dev.PauseAfter, err)
	}
	if _, ok := cfg.bootKeys()["dev.pause_after"]; !ok {
		t.Error("dev.pause_after is set at boot, so it is a boot key")
	}
	if _, err := loadConfig(write(t, "dev: {pause_after: 10s}\n"), ""); err == nil || !strings.Contains(err.Error(), "dev.pause_after") {
		t.Errorf("10s: err %v, want a refusal (a minute at least)", err)
	}
}

// TestRunAndScanKeys: the keys sbox run and sbox scan take from the
// user's file and -config, and what each mode makes of them.
func TestRunAndScanKeys(t *testing.T) {
	t.Parallel()
	write := func(t *testing.T, body string) string {
		p := filepath.Join(t.TempDir(), "sbox.yaml")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	var defaults config
	if defaults.runTimeout() != time.Minute || defaults.entryTimeout() != 20*time.Second || defaults.prefetchTimeout() != 3*time.Minute || !defaults.prefetchOn() {
		t.Errorf("defaults: run %v, entry %v, prefetch %v on %v", defaults.runTimeout(), defaults.entryTimeout(), defaults.prefetchTimeout(), defaults.prefetchOn())
	}

	cfg, err := loadConfig(
		write(t, "vm: {vcpus: 4}\ntimeouts: {run: 5m, entry: 30s}\nprefetch: {registries: [npm.internal.example.com]}\n"),
		write(t, "vm: {memory_mib: 4096}\ntimeouts: {prefetch: 1m}\nprefetch: {enabled: false, registries: [pypi.internal.example.com]}\negress: [api.github.com]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.runTimeout() != 5*time.Minute || cfg.entryTimeout() != 30*time.Second || cfg.prefetchTimeout() != time.Minute || cfg.prefetchOn() {
		t.Errorf("run %v, entry %v, prefetch %v on %v", cfg.runTimeout(), cfg.entryTimeout(), cfg.prefetchTimeout(), cfg.prefetchOn())
	}
	job := cfg.runJob([]string{"make", "test"})
	if !slices.Equal(job.command, []string{"make", "test"}) || !slices.Equal(job.egress, []string{"api.github.com:443"}) || job.vcpus != 4 || job.memoryMiB != 4096 ||
		!slices.Equal(job.registries, []string{"npm.internal.example.com", "pypi.internal.example.com"}) {
		t.Errorf("runJob = %+v", job)
	}
	if dev := devJob(cfg, nil); dev.vcpus != 4 || dev.memoryMiB != 4096 {
		t.Errorf("dev without dev.vcpus: %d vCPUs, %d MiB; want vm's", dev.vcpus, dev.memoryMiB)
	}
	cfg.Dev.VCPUs = 2
	if dev := devJob(cfg, nil); dev.vcpus != 2 {
		t.Errorf("dev.vcpus set: %d, want it over vm's", dev.vcpus)
	}

	for _, bad := range []string{"vm: {vcpus: 99}", "vm: {memory_mib: 10}", "timeouts: {run: 500ms}", "timeouts: {entry: 0.5s}",
		"prefetch: {registries: [\"bad host\"]}", "prefetch: {registries: [\"https://npm.example.com\"]}"} {
		if _, err := loadConfig("", write(t, bad+"\n")); err == nil {
			t.Errorf("%s: loaded, want a refusal", bad)
		}
	}
}

// TestToolConfig: sbox run and scan read the user's file and -config,
// which must exist, and never a file inside the target.
func TestToolConfig(t *testing.T) {
	t.Parallel()
	user, extra := filepath.Join(t.TempDir(), "config.yaml"), filepath.Join(t.TempDir(), "ci.yaml")
	writeTestFile(t, user, "timeouts: {run: 2m}\n")
	writeTestFile(t, extra, "egress: [api.github.com]\n")
	cfg, err := toolConfig(user, extra)
	if err != nil || cfg.runTimeout() != 2*time.Minute || !slices.Equal(cfg.Egress, []string{"api.github.com:443"}) {
		t.Errorf("toolConfig = %+v, %v; want both files", cfg, err)
	}
	if _, err := toolConfig(user, filepath.Join(t.TempDir(), "none.yaml")); err == nil || !strings.Contains(err.Error(), "-config") {
		t.Errorf("a missing -config: %v, want a refusal", err)
	}
	if cfg, err := toolConfig(user, ""); err != nil || len(cfg.Egress) != 0 {
		t.Errorf("no -config: %+v, %v", cfg, err)
	}
}
