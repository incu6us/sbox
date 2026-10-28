//go:build vm

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// lastTick reads a session's console and returns the counter's last value.
func lastTick(t *testing.T, id string) int {
	t.Helper()
	var out bytes.Buffer
	if code := sessionMain(context.Background(), "log", []string{id}, &out, io.Discard, sboxRun); code != exitOK {
		t.Fatalf("sbox log: exit %d", code)
	}
	m := _counterTick.FindAllStringSubmatch(out.String(), -1)
	if len(m) == 0 {
		t.Fatalf("no ticks in the console:\n%s", out.String())
	}
	n, _ := strconv.Atoi(m[len(m)-1][1])
	return n
}

func sessionCmd(t *testing.T, cmd string, args ...string) string {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := sessionMain(context.Background(), cmd, args, &out, &errOut, sboxRun); code != exitOK {
		t.Fatalf("sbox %s %v: exit %d: %s", cmd, args, code, errOut.String())
	}
	return out.String()
}

// TestSessionLifecycleOnVM starts a session whose guest counts once a
// second, and checks that pause freezes the count, resume continues it,
// and stop leaves nothing in sbox ps.
func TestSessionLifecycleOnVM(t *testing.T) {
	ctx := context.Background()
	if ok, _ := doctor(ctx, io.Discard); !ok {
		t.Skip("the sbox VM is not ready: run sbox setup")
	}

	var out, errOut bytes.Buffer
	if code := startMain(ctx, []string{t.TempDir(), "--", "sh", "-c", _counter}, &out, &errOut, sboxRun); code != exitOK {
		t.Fatalf("sbox start: exit %d: %s", code, errOut.String())
	}
	id := strings.TrimSpace(out.String())
	t.Cleanup(func() { _ = sessionMain(ctx, "stop", []string{id}, io.Discard, io.Discard, sboxRun) })

	if ps := sessionCmd(t, "ps"); !strings.Contains(ps, id+"  running") {
		t.Fatalf("sbox ps:\n%s\nwant %s running", ps, id)
	}
	time.Sleep(3 * time.Second)

	sessionCmd(t, "pause", id)
	if ps := sessionCmd(t, "ps"); !strings.Contains(ps, id+"  paused") {
		t.Errorf("sbox ps:\n%s\nwant %s paused", ps, id)
	}
	frozen := lastTick(t, id)
	time.Sleep(3 * time.Second)
	if got := lastTick(t, id); got != frozen {
		t.Errorf("counter moved from %d to %d while paused", frozen, got)
	}

	sessionCmd(t, "resume", id)
	time.Sleep(3 * time.Second)
	if got := lastTick(t, id); got <= frozen {
		t.Errorf("counter at %d after resume, was %d when paused", got, frozen)
	}

	sessionCmd(t, "stop", id)
	if ps := sessionCmd(t, "ps"); strings.Contains(ps, id) {
		t.Errorf("sbox ps after stop:\n%s\nstill lists %s", ps, id)
	}
}

// TestAttachOnVM attaches to a session over SSH, runs a command in the
// project folder as the runner, and gets its output and exit code back.
func TestAttachOnVM(t *testing.T) {
	ctx := context.Background()
	if ok, _ := doctor(ctx, io.Discard); !ok {
		t.Skip("the sbox VM is not ready: run sbox setup")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello from the host\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var errOut bytes.Buffer
	id, _, code := startSession(ctx, dir, "", guestJob{command: []string{"sleep", "infinity"}}, &errOut, sboxRun)
	if code != exitOK {
		t.Fatalf("start: exit %d: %s", code, errOut.String())
	}
	t.Cleanup(func() { _ = sessionMain(ctx, "stop", []string{id}, io.Discard, io.Discard, sboxRun) })

	var out bytes.Buffer
	code = attachMain(ctx, []string{id, "--", "sh", "-c", "cat hello.txt; id -u; pwd; exit 7"}, strings.NewReader(""), &out, &errOut, sboxRun)
	if code != 7 {
		t.Errorf("attach: exit %d, want the guest command's 7; stderr: %s", code, errOut.String())
	}
	for _, want := range []string{"hello from the host", "1000", "/home/runner/work"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("attach output %q lacks %q", out.String(), want)
		}
	}
}

// TestAttachTerminalOnVM attaches from a terminal (script(1) provides one)
// and checks the guest command gets a terminal too.
func TestAttachTerminalOnVM(t *testing.T) {
	ctx := context.Background()
	if ok, _ := doctor(ctx, io.Discard); !ok {
		t.Skip("the sbox VM is not ready: run sbox setup")
	}
	var errOut bytes.Buffer
	id, _, code := startSession(ctx, t.TempDir(), "", guestJob{command: []string{"sleep", "infinity"}}, &errOut, sboxRun)
	if code != exitOK {
		t.Fatalf("start: exit %d: %s", code, errOut.String())
	}
	t.Cleanup(func() { _ = sessionMain(ctx, "stop", []string{id}, io.Discard, io.Discard, sboxRun) })

	out, err := exec.Command("script", "-q", "/dev/null", limactlBin(), "shell", "--workdir", "/", _vmName,
		"sudo", _runScriptPath, "attach", id, "--", "sh", "-c", "tty; stty size").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "/dev/pts/") {
		t.Errorf("attach from a terminal: %v, output %q; want a /dev/pts terminal in the guest", err, out)
	}
}

// TestDevStateIsKeptOnVM checks that a project's home (with the project in
// it) survives from one session to the next, and that forgetting the
// project starts it fresh from the host's copy.
func TestDevStateIsKeptOnVM(t *testing.T) {
	ctx := context.Background()
	if ok, _ := doctor(ctx, io.Discard); !ok {
		t.Skip("the sbox VM is not ready: run sbox setup")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("host version\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	project, err := projectID(dir)
	if err != nil {
		t.Fatal(err)
	}
	forget := func() {
		if err := sboxRun(ctx, nil, io.Discard, io.Discard, "forget", project); err != nil {
			t.Fatalf("forget: %v", err)
		}
	}
	forget()
	t.Cleanup(forget)

	// session runs one command in a new session on the project and stops it.
	session := func(script string) string {
		t.Helper()
		var out, errOut bytes.Buffer
		id, _, code := startSession(ctx, dir, project, guestJob{command: []string{"sleep", "infinity"}}, &errOut, sboxRun)
		if code != exitOK {
			t.Fatalf("start: exit %d: %s", code, errOut.String())
		}
		defer func() { _ = sessionMain(ctx, "stop", []string{id}, io.Discard, io.Discard, sboxRun) }()
		if code := attachMain(ctx, []string{id, "--", "sh", "-c", script}, strings.NewReader(""), &out, &errOut, sboxRun); code != exitOK {
			t.Fatalf("attach %q: exit %d: %s", script, code, errOut.String())
		}
		return out.String()
	}

	session("echo kept > ~/state.txt && echo guest edit > README")
	if out := session("cat ~/state.txt README"); !strings.Contains(out, "kept") || !strings.Contains(out, "guest edit") {
		t.Errorf("second session sees %q, want the home file and the guest's edit", out)
	}
	forget()
	if out := session("cat README; test -e ~/state.txt && echo still-there || echo gone"); !strings.Contains(out, "host version") || !strings.Contains(out, "gone") {
		t.Errorf("after forget the session sees %q, want the host's README and no home file", out)
	}
}

// TestSyncOnVM edits a project inside sbox dev and on the Mac, then
// checks sbox sync's dry run, its -apply, and both sides afterwards.
func TestSyncOnVM(t *testing.T) {
	ctx := context.Background()
	if ok, _ := doctor(ctx, io.Discard); !ok {
		t.Skip("the sbox VM is not ready: run sbox setup")
	}
	dir, state := t.TempDir(), t.TempDir()
	for name, body := range map[string]string{"README": "v1\n", "keep.txt": "keep\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	project, err := projectID(dir)
	if err != nil {
		t.Fatal(err)
	}
	forget := func() { _ = sboxRun(ctx, nil, io.Discard, io.Discard, "forget", project) }
	forget()
	t.Cleanup(forget)
	dev := func(script string) string {
		t.Helper()
		var out, errOut bytes.Buffer
		if code := devMain(ctx, []string{dir, "--", "sh", "-c", script}, strings.NewReader(""), &out, &errOut, sboxRun, state, "", nil); code != exitOK {
			t.Fatalf("dev %q: exit %d: %s", script, code, errOut.String())
		}
		return out.String()
	}

	dev("echo guest > README && echo new > added.txt && rm keep.txt")
	if err := os.WriteFile(filepath.Join(dir, "mac.txt"), []byte("from the mac\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := syncMain(ctx, []string{dir}, &out, &errOut, sboxRun, state); code != exitOK {
		t.Fatalf("dry run: exit %d: %s", code, errOut.String())
	}
	for _, want := range []string{"changed  README", "added    added.txt", "deleted  keep.txt", "added    mac.txt", "+guest"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("dry run lacks %q:\n%s", want, out.String())
		}
	}
	if code := syncMain(ctx, []string{"-apply", dir}, &out, &errOut, sboxRun, state); code != exitOK {
		t.Fatalf("apply: exit %d: %s", code, errOut.String())
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "README")); string(got) != "guest\n" {
		t.Errorf("README on the Mac = %q, want the guest's", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "keep.txt")); !os.IsNotExist(err) {
		t.Errorf("keep.txt still on the Mac (stat: %v)", err)
	}
	if got := dev("cat mac.txt added.txt"); !strings.Contains(got, "from the mac") || !strings.Contains(got, "new") {
		t.Errorf("the guest sees %q, want mac.txt pushed and added.txt kept", got)
	}
}

// TestPortsAndServicesOnVM serves a page from the guest on a forwarded
// port and fetches it on the Mac, and reaches a service on the Mac from
// the guest as host.sbox.
func TestPortsAndServicesOnVM(t *testing.T) {
	ctx := context.Background()
	if ok, _ := doctor(ctx, io.Discard); !ok {
		t.Skip("the sbox VM is not ready: run sbox setup")
	}
	service := echoServer(t) // on the Mac's 127.0.0.1
	port := freePort(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("hello from the guest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	job := guestJob{
		command:  []string{"python3", "-m", "http.server", strconv.Itoa(port)},
		ports:    []int{port},
		services: []int{service},
	}
	var errOut bytes.Buffer
	id, _, code := startSession(ctx, dir, "", job, &errOut, sboxRun)
	if code != exitOK {
		t.Fatalf("start: exit %d: %s", code, errOut.String())
	}
	t.Cleanup(func() { _ = sessionMain(ctx, "stop", []string{id}, io.Discard, io.Discard, sboxRun) })

	var page string
	for i := 0; i < 60 && !strings.Contains(page, "hello"); i++ { // Lima notices the port within seconds
		if resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/index.html", port)); err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			page = string(body)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !strings.Contains(page, "hello from the guest") {
		t.Errorf("the guest's page on the Mac's localhost:%d = %q", port, page)
	}

	var out bytes.Buffer
	script := fmt.Sprintf(`import socket; s = socket.create_connection(("host.sbox", %d), timeout=5); s.sendall(b"ping\n"); print(s.recv(100).decode())`, service)
	if code := attachMain(ctx, []string{id, "--", "python3", "-c", script}, strings.NewReader(""), &out, &errOut, sboxRun); code != exitOK ||
		!strings.Contains(out.String(), "ping") {
		t.Errorf("the Mac's service from the guest: exit %d, output %q, stderr %s", code, out.String(), errOut.String())
	}
}

// TestEgressOnVM checks a session's egress from an attached terminal:
// an allowed HTTPS host answers through the proxy, another is refused, and
// an exact host on another port (github.com:22) is reached over raw TCP.
func TestEgressOnVM(t *testing.T) {
	ctx := context.Background()
	if ok, _ := doctor(ctx, io.Discard); !ok {
		t.Skip("the sbox VM is not ready: run sbox setup")
	}
	job := guestJob{command: []string{"sleep", "infinity"}, egress: []string{"*.github.com:443", "github.com:22"}}
	var errOut bytes.Buffer
	id, _, code := startSession(ctx, t.TempDir(), "", job, &errOut, sboxRun)
	if code != exitOK {
		t.Fatalf("start: exit %d: %s", code, errOut.String())
	}
	t.Cleanup(func() { _ = sessionMain(ctx, "stop", []string{id}, io.Discard, io.Discard, sboxRun) })
	run := func(script string) (string, int) {
		var out bytes.Buffer
		code := attachMain(ctx, []string{id, "--", "sh", "-c", script}, strings.NewReader(""), &out, io.Discard, sboxRun)
		return out.String(), code
	}

	if out, _ := run("curl -s -o /dev/null -w '%{http_code}' --max-time 20 https://api.github.com"); out == "" || out == "000" {
		t.Errorf("api.github.com through the proxy: HTTP %q, want an answer", out)
	}
	if out, code := run("curl -s -o /dev/null -w '%{http_code}' --max-time 20 https://example.com"); code == 0 && out != "000" {
		t.Errorf("example.com: HTTP %q, exit %d; want it refused", out, code)
	}
	banner := `import socket; s = socket.create_connection(("github.com", 22), timeout=15); print(s.recv(20).decode())`
	if out, code := run("python3 -c '" + banner + "'"); code != exitOK || !strings.Contains(out, "SSH-2.0") {
		t.Errorf("github.com:22 over raw TCP: exit %d, output %q; want GitHub's SSH banner", code, out)
	}
}

// TestDevReattachOnVM keeps a sbox dev session at detach, pauses it, and
// checks that the next sbox dev resumes and attaches to the same guest.
func TestDevReattachOnVM(t *testing.T) {
	ctx := context.Background()
	if ok, _ := doctor(ctx, io.Discard); !ok {
		t.Skip("the sbox VM is not ready: run sbox setup")
	}
	dir, state := t.TempDir(), t.TempDir()
	project, err := projectID(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sboxRun(ctx, nil, io.Discard, io.Discard, "forget", project) })

	var kept string
	dev := func(script string, keep bool) (string, string) {
		t.Helper()
		var out, errOut bytes.Buffer
		code := devMain(ctx, []string{dir, "--", "sh", "-c", script}, strings.NewReader(""), &out, &errOut, sboxRun, state, "",
			func(id string) bool { kept = id; return keep })
		if code != exitOK {
			t.Fatalf("sbox dev %q: exit %d: %s", script, code, errOut.String())
		}
		return out.String(), errOut.String()
	}

	dev("sleep 300 </dev/null >/dev/null 2>&1 & echo $! > /tmp/sleeper", true)
	t.Cleanup(func() { _ = sessionMain(ctx, "stop", []string{kept}, io.Discard, io.Discard, sboxRun) })
	sessionCmd(t, "pause", kept)

	out, notes := dev("kill -0 $(cat /tmp/sleeper) && echo same-guest", false)
	if !strings.Contains(out, "same-guest") || !strings.Contains(notes, "attached to session "+kept) {
		t.Errorf("second sbox dev: out %q, notes %q; want the first guest's process and the attach note", out, notes)
	}
	if ps := sessionCmd(t, "ps"); strings.Contains(ps, kept) && !strings.Contains(ps, kept+"  exited") {
		t.Errorf("sbox ps after stopping at detach:\n%s", ps)
	}
}
