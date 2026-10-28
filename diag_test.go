package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const _events = `RULE api.anthropic.com:443
RULE *.github.com:443
EGRESS ALLOW api.anthropic.com:443
EGRESS ALLOW api.anthropic.com:443
EGRESS ALLOW api.github.com:443
EGRESS DENY telemetry.example:443
EGRESS DENY telemetry.example:443
EGRESS DENY telemetry.example:443
EGRESS DENY once.example:8443
EGRESS DENY GET http://plain.example/ HTTP/1.1
EGRESS FAIL down.github.com:443
DNS A registry.yarnpkg.com
DNS AAAA registry.yarnpkg.com
DNS A registry.yarnpkg.com
DNS A api.github.com
DNS A x` + "\x1b[2J" + `
`

func TestBlockedEvents(t *testing.T) {
	t.Parallel()
	ev := parseEvents(_events)
	got := ev.blocked()
	want := []string{
		"BLOCKED egress  telemetry.example:443 ×3  not in egress → egress: [telemetry.example]",
		"BLOCKED egress  once.example:8443 ×1  not in egress",
		"BLOCKED egress  GET http://plain.example/ HTTP/1.1 ×1  not HTTPS: the proxy only tunnels HTTPS",
		"FAILED  egress  down.github.com:443 ×1  allowed, but the connection failed",
		"BLOCKED dns     registry.yarnpkg.com (A, AAAA) ×3  not in egress → egress: [registry.yarnpkg.com]",
		"BLOCKED dns     x\\x1b[2J (A) ×1  not in egress",
	}
	if !slices.Equal(got, want) {
		t.Errorf("blocked =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if allowed := ev.allowedSummary(); allowed != "api.anthropic.com:443 ×2, api.github.com:443 ×1" {
		t.Errorf("allowedSummary = %q", allowed)
	}
}

func TestPortLines(t *testing.T) {
	t.Parallel()
	ss := `LISTEN 0 4096 172.16.0.2:22 0.0.0.0:*
LISTEN 0 511 0.0.0.0:5173 0.0.0.0:*
LISTEN 0 511 127.0.0.1:3000 0.0.0.0:*
LISTEN 0 511 [::1]:3000 [::]:*
LISTEN 0 511 127.0.0.1:9229 0.0.0.0:*
LISTEN 0 511 *:8080 *:*
LISTEN 0 511 0.0.0.0:80 0.0.0.0:*
`
	got := portLines(ss, []int{3000, 8080})
	want := []string{
		"BLOCKED port    3000  listening on loopback only, the forward can't reach it → listen on 0.0.0.0 (e.g. --host 0.0.0.0)",
		"BLOCKED port    5173  guest is listening, not forwarded → ports: [5173]",
	}
	if !slices.Equal(got, want) {
		t.Errorf("portLines =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestSessionSummary(t *testing.T) {
	t.Parallel()
	got := sessionSummary(2*time.Hour+5*time.Minute, "./app", 3, parseEvents(_events), []string{"BLOCKED port    5173  guest is listening, not forwarded → ports: [5173]"})
	for _, want := range []string{"session 2h5m · the guest has 3 changes: sbox sync ./app", "egress   api.anthropic.com:443 ×2", "BLOCKED egress  telemetry.example:443", "BLOCKED port    5173"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary lacks %q:\n%s", want, got)
		}
	}
	if quiet := sessionSummary(time.Minute, "./app", 0, parseEvents(""), nil); !strings.Contains(quiet, "nothing to sync") || strings.Contains(quiet, "egress") || strings.Contains(quiet, "BLOCKED") {
		t.Errorf("a quiet session's summary %q, want just the first line", quiet)
	}
}

func TestDevPreflight(t *testing.T) {
	t.Parallel()
	var cfg config
	cfg.Egress = []string{"*.github.com:443", "api.anthropic.com:443"}
	cfg.Ports, cfg.Services = []int{3000}, []int{5432}
	cfg.MCP.Servers = map[string]mcpServer{
		"pg":     {Command: "npx"},
		"gh":     {URL: "https://api.github.com/mcp"},
		"docs":   {URL: "https://mcp.example.com/mcp"},
		"legacy": {URL: "http://old.github.com/mcp"},
	}
	project := map[string]mcpServer{"local": {Command: "node"}, "far": {URL: "https://far.example:8443/sse"}}
	got := devPreflight(cfg, map[string]string{"GITHUB_TOKEN": "x", "CLAUDE_CODE_OAUTH_TOKEN": "y"}, project)
	want := []string{
		"egress   *.github.com:443, api.anthropic.com:443",
		"ports    3000 → the Mac's localhost:3000",
		"services host.sbox:5432 → the Mac's localhost:5432",
		"secrets  CLAUDE_CODE_OAUTH_TOKEN, GITHUB_TOKEN",
		"mcp      docs (remote), gh (remote), legacy (remote), pg (in the guest), far (remote, .mcp.json), local (in the guest, .mcp.json)",
		"BLOCKED mcp     docs  host mcp.example.com:443 not in egress → egress: [mcp.example.com]",
		"BLOCKED mcp     legacy  plain http: the proxy only tunnels HTTPS",
		"BLOCKED mcp     far  host far.example:8443 not in egress → egress: [far.example:8443]",
	}
	if !slices.Equal(got, want) {
		t.Errorf("devPreflight =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got := devPreflight(config{}, nil, nil); !slices.Equal(got, []string{"egress   none: the guest reaches nothing outside"}) {
		t.Errorf("empty config: %q", got)
	}
}

func TestReadMCPJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if got := readMCPJSON(dir); len(got) != 0 {
		t.Errorf("no file: %v", got)
	}
	writeTestFile(t, dir+"/.mcp.json", `{"mcpServers": {"a": {"command": "node", "args": ["s.js"]}, "b": {"type": "http", "url": "https://b.example/mcp"}}}`)
	if got := readMCPJSON(dir); got["a"].Command != "node" || got["b"].URL != "https://b.example/mcp" {
		t.Errorf("readMCPJSON = %+v", got)
	}
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseEventsPorts(t *testing.T) {
	t.Parallel()
	if ev := parseEvents("PORT 3000\nPORT 5173\nPORT x\n"); !slices.Equal(ev.ports, []int{3000, 5173}) {
		t.Errorf("ports = %v, want the forwarded ones", ev.ports)
	}
}

// eventsVM plays a session whose events grow by one denied host per
// call, and which is gone after rounds calls.
type eventsVM struct {
	fakeVM
	rounds int
}

func (v *eventsVM) run(ctx context.Context, in io.Reader, out, errOut io.Writer, args ...string) error {
	v.calls = append(v.calls, args)
	n := 0
	for _, c := range v.calls {
		if c[0] == args[0] {
			n++
		}
	}
	switch args[0] {
	case "events":
		if n > v.rounds {
			return errors.New("sbox-run: no session")
		}
		_, _ = io.WriteString(out, "PORT 3000\nEGRESS DENY a.example:443\n")
		for i := 1; i < n; i++ {
			_, _ = fmt.Fprintf(out, "EGRESS DENY b.example:443\n")
		}
	case "attach":
		if slices.Contains(args, _denialScript) {
			if n >= 4 { // from the second round on: each round attaches twice
				_, _ = io.WriteString(out, "user-rejected\tBash\n")
			}
			return nil
		}
		_, _ = io.WriteString(out, "LISTEN 0 511 127.0.0.1:3000 0.0.0.0:*\n")
	case "log":
		if n > v.rounds {
			return errors.New("sbox-run: no session")
		}
		_, _ = io.WriteString(out, strings.Repeat("tick\r\n", n))
	case "ps":
		_, _ = io.WriteString(out, "0123abcd running 1700000000 0011223344556677\n")
	}
	return nil
}

func TestWatchMain(t *testing.T) {
	t.Parallel()
	vm := &eventsVM{rounds: 3}
	var stdout, stderr bytes.Buffer
	if code := watchMain(context.Background(), nil, &stdout, &stderr, vm.run, time.Millisecond); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	got := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	want := []string{
		"watching session 0123abcd (Ctrl-C to stop)",
		"BLOCKED egress  a.example:443 ×1  not in egress",
		"BLOCKED port    3000  listening on loopback only, the forward can't reach it → listen on 0.0.0.0 (e.g. --host 0.0.0.0)",
		"BLOCKED egress  b.example:443 ×1  not in egress",
		"BLOCKED tool    Bash ×1  rejected at the prompt",
		"BLOCKED egress  b.example:443 ×2  not in egress",
		"session 0123abcd ended",
	}
	if !slices.Equal(got, want) {
		t.Errorf("watch printed\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if code := watchMain(context.Background(), []string{"x", "y"}, &stdout, &stderr, vm.run, time.Millisecond); code != exitUsage {
		t.Errorf("two args: exit %d, want usage", code)
	}
}

func TestLogMain(t *testing.T) {
	t.Parallel()
	vm := &eventsVM{rounds: 3}
	var stdout, stderr bytes.Buffer
	if code := logMain(context.Background(), []string{"-f", "0123abcd"}, &stdout, &stderr, vm.run, time.Millisecond); code != exitOK {
		t.Fatalf("-f: exit %d: %s", code, stderr.String())
	}
	if stdout.String() != "tick\ntick\ntick\n" {
		t.Errorf("-f printed %q, want each line once, as it came", stdout.String())
	}

	stdout.Reset()
	blocked := &eventsVM{rounds: 1}
	if code := logMain(context.Background(), []string{"-blocked", "0123abcd"}, &stdout, &stderr, blocked.run, time.Millisecond); code != exitOK ||
		stdout.String() != "BLOCKED egress  a.example:443 ×1  not in egress\n" {
		t.Errorf("-blocked: exit %d, stdout %q", code, stdout.String())
	}
}

func TestLogMainConsole(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		"hi\n\x1b[2Jx\n":       "hi\n\\x1b[2Jx\n",  // guest output escaped
		"tick 1\r\ntick 2\r\n": "tick 1\ntick 2\n", // the serial console's CR dropped
	} {
		vm := fakeVM{stdout: raw}
		var stdout, stderr bytes.Buffer
		if code := logMain(context.Background(), []string{"0123456789abcdef"}, &stdout, &stderr, vm.run, 0); code != exitOK || stdout.String() != want {
			t.Errorf("log of %q: exit %d, %q; want %q", raw, code, stdout.String(), want)
		}
	}
}

func TestDenialScript(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "projects", "-home-runner-work")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := strings.Join([]string{
		`{"type":"assistant","timestamp":"2026-10-28T10:00:00.000Z","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"rm -rf /"}}]}}`,
		`{"type":"user","timestamp":"2026-10-28T10:00:01.000Z","toolDenialKind":"permission-rule","message":{"content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"denied"}]}}`,
		`{"type":"assistant","timestamp":"2026-10-28T09:00:00.000Z","message":{"content":[{"type":"tool_use","id":"t0","name":"WebFetch","input":{}}]}}`,
		`{"type":"user","timestamp":"2026-10-28T09:00:01.000Z","toolDenialKind":"permission-rule","message":{"content":[{"type":"tool_result","tool_use_id":"t0","is_error":true}]}}`,
		`{"type":"assistant","timestamp":"2026-10-28T10:01:00.000Z","message":{"content":[{"type":"tool_use","id":"t2","name":"mcp__pg__execute","input":{}}]}}`,
		`{"type":"user","timestamp":"2026-10-28T10:01:01.000Z","toolDenialKind":"user-rejected","message":{"content":[{"type":"tool_result","tool_use_id":"t2","is_error":true}]}}`,
		`not json`,
	}, "\n") + "\n"
	writeTestFile(t, filepath.Join(dir, "s.jsonl"), transcript)
	cmd := exec.Command("python3", "-I", "-c", _denialScript, "2026-10-28T09:30:00")
	cmd.Env = []string{"HOME=" + home}
	out, err := cmd.CombinedOutput()
	if err != nil || string(out) != "permission-rule\tBash\nuser-rejected\tmcp__pg__execute\n" {
		t.Errorf("denial script: %v, %q; want the two denials after the start, the earlier one left out", err, out)
	}

	got := denialLines("permission-rule\tBash\npermission-rule\tBash\nuser-rejected\tmcp__pg__execute\nweird kind\tx\nother-kind\tTool name with spaces\n")
	want := []string{
		"BLOCKED tool    Bash ×2  denied by a permission rule (permissions.deny or mcp.deny)",
		"BLOCKED tool    mcp__pg__execute ×1  rejected at the prompt",
	}
	if !slices.Equal(got, want) {
		t.Errorf("denialLines =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestDevTrace(t *testing.T) {
	t.Parallel()
	wrapped := traceCommand([]string{"claude", "--continue"})
	if len(wrapped) != 3 || wrapped[0] != "bash" || wrapped[1] != "-lc" ||
		!strings.Contains(wrapped[2], "exec strace -ff -qq -s 200 -e trace="+_traced+" -o /tmp/sbox-trace/t -- 'claude' '--continue'") {
		t.Errorf("traceCommand = %q", wrapped)
	}
	if shell := traceCommand(nil); !strings.HasSuffix(shell[2], "-- bash -l") {
		t.Errorf("traceCommand(no command) = %q, want a login shell under strace", shell)
	}

	out := strings.Join([]string{
		`T 12 execve("/usr/local/bin/node", ["node", "install.js"], 0x0 /* 3 vars */) = 0`,
		`T 12 openat(AT_FDCWD, "/home/runner/.aws/credentials", O_RDONLY) = 3`,
		`D 12 openat(AT_FDCWD, "/usr/local/lib/node_modules/x/y", O_WRONLY|O_CREAT|O_TRUNC, 0644) = -1 EROFS (Read-only file system)`,
		`D 12 openat(AT_FDCWD, "/etc/hosts", O_WRONLY, 0644) = -1 EACCES (Permission denied)`,
		`D 12 openat(AT_FDCWD, "/home/runner/x", O_WRONLY, 0644) = -1 EACCES (Permission denied)`,
		`garbage`,
	}, "\n")
	got := traceLines(out, "c0ffee")
	for _, want := range []string{
		"INFO    credential-read  reads /home/runner/.aws/credentials",
		"BLOCKED write   /usr/local/lib/node_modules/x/y  EROFS: the root is read-only → install into ~/.local or the project",
		"BLOCKED write   /etc/hosts  EACCES: not the runner's",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("traceLines lacks %q:\n%s", want, strings.Join(got, "\n"))
		}
	}
	if slices.ContainsFunc(got, func(l string) bool { return strings.Contains(l, "/home/runner/x") }) {
		t.Errorf("a refused write in the home is the runner's own business, not the sandbox's:\n%s", strings.Join(got, "\n"))
	}
}
