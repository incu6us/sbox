package main

import (
	"bytes"
	"context"
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

// TestDevMainWorkspaces: a project asks; the user's file grants. The
// downloaded-repo case asks for a key, a secret and the network, and
// nothing starts; a project inside its workspace's allowance starts, with
// the workspace's grants on top.
func TestDevMainWorkspaces(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	user := filepath.Join(t.TempDir(), "config.yaml")
	dev := func(t *testing.T, dir string) (*fakeVM, string, int) {
		vm := &fakeVM{answer: func(args []string) string {
			if args[0] == "start" {
				return "READY " + args[1] + "\n"
			}
			return ""
		}}
		var stdout, stderr bytes.Buffer
		host := devHost{stateDir: t.TempDir(), userConfig: user, home: home, lookupEnv: func(k string) (string, bool) { return "value-of-" + k, true }}
		code := devMain(context.Background(), []string{dir, "--", "true"}, strings.NewReader(""), &stdout, &stderr, vm.run, host)
		return vm, stderr.String(), code
	}

	stranger := t.TempDir()
	writeTestFile(t, filepath.Join(stranger, "sbox.yaml"), `mounts: [{host: ~/.ssh, guest: /home/runner/.ssh}]
secrets: {X: "env:AWS_SECRET_ACCESS_KEY"}
egress: [evil.example]
`)
	writeTestFile(t, user, "egress: [api.anthropic.com]\n")
	vm, notes, code := dev(t, stranger)
	for _, want := range []string{
		"BLOCKED mount   ~/.ssh → /home/runner/.ssh  not allowed in workspace default → workspaces.default.allow.mounts: [~/.ssh]",
		"BLOCKED secret  X ← env:AWS_SECRET_ACCESS_KEY  not allowed in workspace default → workspaces.default.allow.secrets: [AWS_SECRET_ACCESS_KEY]",
		"BLOCKED egress  evil.example:443  not allowed in workspace default → workspaces.default.allow.egress: [evil.example]",
		"asks for more than workspace default allows; nothing was started",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("stderr lacks %q:\n%s", want, notes)
		}
	}
	if code != exitFail || !slices.EqualFunc(vm.calls, [][]string{{"ps"}}, slices.Equal) {
		t.Errorf("the stranger's repo: exit %d, calls %q; want only a look for a running session", code, vm.calls)
	}

	project := filepath.Join(home, "work", "client-a", "api")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(home, ".gitconfig-a"), "[user]\n")
	writeTestFile(t, user, `egress: [api.anthropic.com]
workspaces:
  client-a:
    paths: [~/work/client-a]
    egress: ["*.client-a.com"]
    mounts: [{host: ~/.gitconfig-a, guest: /home/runner/.gitconfig}]
    allow:
      egress: [registry.npmjs.org]
      services: ["5432"]
      secrets: [CLIENT_A_NPM_TOKEN]
`)
	writeTestFile(t, filepath.Join(project, "sbox.yaml"), "egress: [registry.npmjs.org, api.anthropic.com]\nservices: [5432]\nsecrets: {NPM_TOKEN: \"env:CLIENT_A_NPM_TOKEN\"}\n")
	vm, notes, code = dev(t, project)
	if code != exitOK {
		t.Fatalf("an allowed project: exit %d\n%s", code, notes)
	}
	files := tarFiles(t, vm.stdins[slices.IndexFunc(vm.calls, func(c []string) bool { return c[0] == "start" })])
	if files["sbox/egress"] != "*.client-a.com:443\napi.anthropic.com:443\nregistry.npmjs.org:443\n" || files["sbox/services"] != "5432\n" ||
		files["mounts/0"] != "[user]\n" || files["sbox/secrets"] != "NPM_TOKEN=value-of-CLIENT_A_NPM_TOKEN\n" {
		t.Errorf("the job: egress %q, services %q, mount %q, secrets %q", files["sbox/egress"], files["sbox/services"], files["mounts/0"], files["sbox/secrets"])
	}

	writeTestFile(t, filepath.Join(project, "sbox.yaml"), "egress: [pypi.org]\n")
	if _, notes, code := dev(t, project); code != exitFail || !strings.Contains(notes, "→ workspaces.client-a.allow.egress: [pypi.org]") {
		t.Errorf("pypi.org in client-a: exit %d\n%s", code, notes)
	}
}

// _allowAll is a user config whose default workspace lets a project ask
// for anything: for tests about other things than workspaces.
const _allowAll = "workspaces: {default: {allow: {egress: [\"*\"], services: [\"*\"], mounts: [/], rw: [/], secrets: [GH, GH_SBOX, GH_SBOX_TEST, CLAUDE_CODE_OAUTH_TOKEN]}}}\n"

// allowAll writes _allowAll to a file and returns its path.
func allowAll(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	writeTestFile(t, p, _allowAll)
	return p
}

// TestDevMainApproveOnce: in a terminal the user can allow a project's
// requests for that project alone; the answer is remembered, and only
// new requests are asked about.
func TestDevMainApproveOnce(t *testing.T) {
	t.Parallel()
	dir, state := t.TempDir(), t.TempDir()
	user := filepath.Join(t.TempDir(), "config.yaml")
	writeTestFile(t, user, "egress: [api.anthropic.com]\n")
	writeTestFile(t, filepath.Join(dir, "sbox.yaml"), "egress: [one.example]\n")
	var asked [][]string
	answer := true
	dev := func() (string, int) {
		vm := &fakeVM{answer: func(args []string) string {
			if args[0] == "start" {
				return "READY " + args[1] + "\n"
			}
			return ""
		}}
		var stdout, stderr bytes.Buffer
		host := devHost{stateDir: state, userConfig: user, approve: func(lines []string) bool { asked = append(asked, lines); return answer }}
		code := devMain(context.Background(), []string{dir, "--", "true"}, strings.NewReader(""), &stdout, &stderr, vm.run, host)
		return stderr.String(), code
	}
	if notes, code := dev(); code != exitOK || len(asked) != 1 || len(asked[0]) != 1 || !strings.Contains(asked[0][0], "one.example:443") {
		t.Fatalf("first run: exit %d, asked %q\n%s", code, asked, notes)
	}
	data, _ := os.ReadFile(filepath.Join(state, "projects", mustProjectID(t, dir), "approved.json"))
	if !strings.Contains(string(data), "egress one.example:443") {
		t.Errorf("approved.json = %s", data)
	}
	if notes, code := dev(); code != exitOK || len(asked) != 1 {
		t.Errorf("second run: exit %d, asked %d times; want no question\n%s", code, len(asked), notes)
	}
	writeTestFile(t, filepath.Join(dir, "sbox.yaml"), "egress: [one.example, two.example]\n")
	answer = false
	if _, code := dev(); code != exitFail || len(asked) != 2 || len(asked[1]) != 1 || !strings.Contains(asked[1][0], "two.example:443") {
		t.Errorf("a new request, declined: exit %d, asked %q; want two.example alone, and a refusal", code, asked)
	}
}

// TestWorkspaceFlagLiveRunConfig: -workspace picks the workspace for one
// run; a running session keeps what it booted with when a new request is
// refused; sbox run takes the target's workspace grants; sbox config says
// which workspace, why, and what it would refuse.
func TestWorkspaceFlagLiveRunConfig(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	user := filepath.Join(t.TempDir(), "config.yaml")
	writeTestFile(t, user, `egress: [api.anthropic.com]
workspaces:
  personal:
    paths: [~/src]
    egress: [personal.example]
    allow: {egress: ["*"]}
  client-a:
    paths: [~/work/client-a]
    egress: ["*.client-a.com"]
`)
	dir := filepath.Join(home, "Downloads", "r")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "sbox.yaml"), "egress: [registry.npmjs.org]\n")
	start := func(args []string) string {
		if args[0] == "start" {
			return "READY " + args[1] + "\n"
		}
		return ""
	}

	// -workspace personal: its allow lets registry.npmjs.org through.
	vm := &fakeVM{answer: start}
	var stdout, stderr bytes.Buffer
	host := devHost{stateDir: t.TempDir(), userConfig: user, home: home}
	if code := devMain(context.Background(), []string{"-workspace", "personal", dir, "--", "true"}, strings.NewReader(""), &stdout, &stderr, vm.run, host); code != exitOK {
		t.Errorf("-workspace personal: exit %d\n%s", code, stderr.String())
	}
	// Any egress there, but still no mount or secret it doesn't list.
	asks := filepath.Join(home, "Downloads", "asks")
	if err := os.MkdirAll(asks, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(asks, "sbox.yaml"), "egress: [evil.example]\nmounts: [{host: ~/.ssh, guest: /home/runner/.ssh}]\nsecrets: {X: \"env:AWS_SECRET_ACCESS_KEY\"}\n")
	stderr.Reset()
	if code := devMain(context.Background(), []string{"-workspace", "personal", asks, "--", "true"}, strings.NewReader(""), &stdout, &stderr, vm.run, host); code != exitFail ||
		strings.Contains(stderr.String(), "BLOCKED egress") || !strings.Contains(stderr.String(), "BLOCKED mount") || !strings.Contains(stderr.String(), "BLOCKED secret") {
		t.Errorf("personal with a mount and a secret: exit %d\n%s", code, stderr.String())
	}

	// A running session, a refused request: attached, the request left out.
	project := mustProjectID(t, dir)
	live := &fakeVM{answer: func(args []string) string {
		if args[0] == "ps" {
			return "0123abcd running 1700000000 " + project + "\n"
		}
		return ""
	}}
	stderr.Reset()
	if code := devMain(context.Background(), []string{dir, "--", "true"}, strings.NewReader(""), &stdout, &stderr, live.run, host); code != exitOK ||
		!strings.Contains(stderr.String(), "BLOCKED egress  registry.npmjs.org:443") || !strings.Contains(stderr.String(), "session 0123abcd keeps what it booted with") ||
		slices.ContainsFunc(live.calls, func(c []string) bool { return c[0] == "reconfigure" }) {
		t.Errorf("reattach with a refused request: exit %d, calls %q\n%s", code, live.calls, stderr.String())
	}

	// sbox run: the target's workspace grants, nothing of the others'.
	target := filepath.Join(home, "work", "client-a", "x")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, ws, err := runConfig(user, "", target, "", home)
	if err != nil || ws != "client-a" || !slices.Equal(cfg.Egress, []string{"*.client-a.com:443", "api.anthropic.com:443"}) {
		t.Errorf("runConfig: %q %q %v", ws, cfg.Egress, err)
	}

	// sbox config: the workspace, why, the sources, what it would refuse.
	stdout.Reset()
	if code := configMain([]string{dir}, &stdout, &stderr, devHost{userConfig: user, home: home}); code != exitOK {
		t.Fatalf("sbox config: exit %d: %s", code, stderr.String())
	}
	for _, want := range []string{
		"# workspace default (no workspace's paths contain this folder)",
		"# BLOCKED egress  registry.npmjs.org:443  not allowed in workspace default",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("sbox config lacks %q:\n%s", want, stdout.String())
		}
	}
	stdout.Reset()
	if code := configMain([]string{target}, &stdout, &stderr, devHost{userConfig: user, home: home}); code != exitOK ||
		!strings.Contains(stdout.String(), "# workspace client-a ("+macPath("~/work/client-a", home)+" contains this folder)") ||
		!strings.Contains(stdout.String(), "egress: # user, workspace client-a") {
		t.Errorf("sbox config in client-a: exit %d\n%s", code, stdout.String())
	}
}
