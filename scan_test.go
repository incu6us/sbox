package main

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseTrace(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		give         string
		wantCategory string
		wantItem     string
	}{
		{
			name:         "process",
			give:         `412 execve("/usr/local/bin/node", ["node", "/home/runner/work/a.js"], 0xffff /* 5 vars */) = 0`,
			wantCategory: "proc", wantItem: "node /home/runner/work/a.js",
		},
		{
			name:         "escaped argv",
			give:         `412 execve("/bin/sh", ["sh", "-c", "node \"${X}/a.js\""], 0xffff /* 5 vars */) = 0`,
			wantCategory: "proc", wantItem: `sh -c node "${X}/a.js"`,
		},
		{
			name: "PATH lookup is not a process",
			give: `412 execve("/usr/local/sbin/node", ["node"], 0xffff /* 5 vars */) = -1 ENOENT (No such file or directory)`,
		},
		{
			name:         "blocked connection",
			give:         `413 connect(3, {sa_family=AF_INET, sin_port=htons(443), sin_addr=inet_addr("140.82.112.3")}, 16) = -1 ENETUNREACH (Network is unreachable)`,
			wantCategory: "net", wantItem: "140.82.112.3:443 → ENETUNREACH",
		},
		{
			name:         "ipv6 connection",
			give:         `413 connect(3, {sa_family=AF_INET6, sin6_port=htons(80), sin6_flowinfo=htonl(0), inet_pton(AF_INET6, "2606:4700::1", &sin6_addr), sin6_scope_id=0}, 28) = -1 ENETUNREACH (Network is unreachable)`,
			wantCategory: "net", wantItem: "[2606:4700::1]:80 → ENETUNREACH",
		},
		{
			name:         "non-blocking connect",
			give:         `413 connect(5, {sa_family=AF_INET, sin_port=htons(80), sin_addr=inet_addr("93.184.215.14")}, 16) = -1 EINPROGRESS (Operation now in progress)`,
			wantCategory: "net", wantItem: "93.184.215.14:80 → attempted",
		},
		{
			name:         "DNS lookup",
			give:         `413 connect(5, {sa_family=AF_INET, sin_port=htons(53), sin_addr=inet_addr("127.0.0.53")}, 16) = 0`,
			wantCategory: "net", wantItem: "DNS lookup via 127.0.0.53:53 (hostname not recorded)",
		},
		{
			name: "nscd probe is noise",
			give: `413 connect(5, {sa_family=AF_UNIX, sun_path="/var/run/nscd/socket"}, 110) = -1 ENOENT (No such file or directory)`,
		},
		{
			name:         "unix socket",
			give:         `413 connect(4, {sa_family=AF_UNIX, sun_path="/run/x.sock"}, 110) = -1 ENOENT (No such file or directory)`,
			wantCategory: "net", wantItem: "unix:/run/x.sock → ENOENT",
		},
		{
			name:         "read of a missing file",
			give:         `414 openat(AT_FDCWD, "/home/runner/.aws/credentials", O_RDONLY|O_CLOEXEC) = -1 ENOENT (No such file or directory)`,
			wantCategory: "file", wantItem: "read /home/runner/.aws/credentials (ENOENT)",
		},
		{
			name:         "write",
			give:         `414 openat(AT_FDCWD, "/home/runner/.zshrc", O_WRONLY|O_CREAT|O_APPEND, 0666) = 3`,
			wantCategory: "file", wantItem: "write /home/runner/.zshrc",
		},
		{
			name:         "delete",
			give:         `415 unlinkat(AT_FDCWD, "/tmp/x", 0) = 0`,
			wantCategory: "file", wantItem: "delete /tmp/x",
		},
		{
			name:         "rename",
			give:         `415 renameat2(AT_FDCWD, "/tmp/a", AT_FDCWD, "/tmp/b", RENAME_NOREPLACE) = 0`,
			wantCategory: "file", wantItem: "rename /tmp/a → /tmp/b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ev, ok := parseTrace(tt.give)
			if !ok {
				ev = traceEvent{}
			}
			item := ""
			if ok {
				item = ev.String()
			}
			if ev.kind != tt.wantCategory || item != tt.wantItem {
				t.Errorf("parseTrace = %q, %q; want %q, %q", ev.kind, item, tt.wantCategory, tt.wantItem)
			}
		})
	}
}

func TestCollectRuns(t *testing.T) {
	t.Parallel()

	eps := []entryPoint{{Kind: "hook", Name: "A"}, {Kind: "hook", Name: "B"}}
	markers := []string{
		"ENTRY 1",
		"RESULT 1 0",
		`TRACE 1 9 execve("/bin/sh", ["sh", "-c", "x"], 0x1) = 0`,
		`TRACE 1 9 execve("/bin/sh", ["sh", "-c", "x"], 0x1) = 0`, // duplicate
		"TRACE 7 out of range",
		"ENTRY 2",
		"DNS A entry-1-n0nce.sbox.invalid",
		"DNS A example.com",
		"DNS AAAA example.com",
		"DNS A entry-2-forged.sbox.invalid", // wrong nonce: an ordinary lookup
		"DNS A entry-2-n0nce.sbox.invalid",
		"DNS A api.other.io",
	}
	runs := collectRuns(eps, markers, "n0nce")

	if !runs[0].ran || runs[0].exitCode != 0 || !slices.Equal(eventStrings(runs[0].procs), []string{"sh -c x"}) {
		t.Errorf("run 1 = %+v, want ran, exit 0, one process", runs[0])
	}
	if runs[1].ran {
		t.Errorf("run 2 = %+v, want not ran (no RESULT)", runs[1])
	}
	wantDNS := [][]string{
		{"example.com (A, AAAA)", "entry-2-forged.sbox.invalid (A)"},
		{"api.other.io (A)"},
	}
	for i, want := range wantDNS {
		var got []string
		for _, d := range runs[i].dns {
			got = append(got, d.String())
		}
		if !slices.Equal(got, want) {
			t.Errorf("run %d DNS = %v, want %v", i+1, got, want)
		}
	}
}

func TestScanScript(t *testing.T) {
	t.Parallel()

	eps := []entryPoint{
		{Kind: "hook", Name: "SessionStart", Command: `node "${CLAUDE_PLUGIN_ROOT}/it's.js"`, Stdin: "{}\n"},
		{Kind: "mcp", Name: "files", Command: "'npx' 'pkg'", Stdin: "init\n"},
		{Kind: "npm", Name: "rebuild", Command: "npm rebuild", Env: []string{"npm_config_userconfig=/dev/null"}},
	}
	script, files := scanScript(eps, 20*time.Second)

	for _, want := range []string{
		`strace -ff -qq -s 200 -u runner`,
		`sed "s/^/SBOX-$nonce TRACE 1 ${f##*.} /"`,
		`-o /run/sbox/trace.1 sh -c 'node "${CLAUDE_PLUGIN_ROOT}/it'\''s.js"' </drive/sbox/in/stdin.1`,
		`sh -c '(cat /drive/sbox/in/stdin.2; sleep 18) | '\''npx'\'' '\''pkg'\''' </dev/null`,
		`pkill -KILL -u runner`,
		`NODE_DISABLE_COMPILE_CACHE=1 'npm_config_userconfig=/dev/null' "$@" \`,
		`[ -f /drive/sbox/secrets ] && while IFS= read -r kv; do set -- "$@" "$kv"; done </drive/sbox/secrets`,
		`getent hosts "entry-2-$nonce.sbox.invalid"`,
		`echo "SBOX-$nonce RESULT 2 $status"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q:\n%s", want, script)
		}
	}
	if files["stdin.1"] != "{}\n" || files["stdin.2"] != "init\n" {
		t.Errorf("files = %v", files)
	}
}

func TestDNSLoggerSelftest(t *testing.T) {
	t.Parallel()

	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	out, err := exec.Command(python, "-I", "scripts/sbox-dns.py", "--selftest").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Errorf("sbox-dns.py --selftest: %v: %s", err, out)
	}
}

func TestAttribute(t *testing.T) {
	t.Parallel()

	// An agent (pid 10) with a worker thread (11) runs a tool: a shell (12)
	// that runs cat (13). Only the shell's subtree is judged.
	var tr processTraces
	for _, l := range []struct {
		pid  int
		line string
	}{
		{10, `execve("/usr/local/bin/claude", ["claude", "-p", "x"], 0x1 /* 5 vars */) = 0`},
		{10, `clone3({flags=CLONE_VM|CLONE_THREAD, exit_signal=0}, 88) = 11`},
		{10, `openat(AT_FDCWD, "/home/runner/.claude/settings.json", O_RDONLY) = 3`},
		{11, `openat(AT_FDCWD, "/home/runner/.claude.json", O_WRONLY|O_CREAT, 0600) = 4`},
		{10, `clone(child_stack=NULL, flags=SIGCHLD) = 12`},
		{12, `execve("/bin/bash", ["bash", "-c", "cat ~/.aws/credentials"], 0x1 /* 5 vars */) = 0`},
		{12, `vfork() = 13`},
		{13, `execve("/usr/bin/cat", ["cat", "/home/runner/.aws/credentials"], 0x1 /* 5 vars */) = 0`},
		{13, `openat(AT_FDCWD, "/home/runner/.aws/credentials", O_RDONLY) = 3`},
	} {
		tr.add(l.pid, l.line)
	}

	r := entryRun{ep: entryPoint{Harness: []string{"claude"}}}
	attribute(&r, tr)

	if got := eventStrings(r.procs); !slices.Equal(got, []string{"bash -c cat ~/.aws/credentials", "cat /home/runner/.aws/credentials"}) {
		t.Errorf("procs = %v", got)
	}
	if got := eventStrings(r.files); !slices.Equal(got, []string{"read /home/runner/.aws/credentials"}) {
		t.Errorf("files = %v", got)
	}
	if r.files[0].prog != "cat" {
		t.Errorf("credential read attributed to %q, want cat", r.files[0].prog)
	}
	if r.hidden != 3 { // claude's exec, its config read, the thread's write
		t.Errorf("hidden = %d, want 3", r.hidden)
	}

	// Without a harness, everything is judged.
	r = entryRun{}
	attribute(&r, tr)
	if len(r.files) != 3 || r.hidden != 0 {
		t.Errorf("no harness: files %v hidden %d, want 3 files, 0 hidden", eventStrings(r.files), r.hidden)
	}
}

func TestProgramName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path string
		argv []string
		want string
	}{
		{path: "/usr/local/bin/npm", argv: []string{"npm", "rebuild"}, want: "npm"},
		{path: "/usr/local/bin/node", argv: []string{"node", "/usr/local/bin/npm", "rebuild"}, want: "npm"},
		{path: "/usr/local/bin/node", argv: []string{"node", "install.js"}, want: "install.js"},
		{path: "/usr/local/bin/node", argv: []string{"node", "-e", "x"}, want: "node"},
		{path: "/bin/sh", argv: []string{"sh", "-c", "x"}, want: "sh"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.argv, " "), func(t *testing.T) {
			t.Parallel()
			if got := programName(traceEvent{path: tt.path, argv: tt.argv}); got != tt.want {
				t.Errorf("programName = %q, want %q", got, tt.want)
			}
		})
	}
}
