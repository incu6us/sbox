package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// _fakes stand in for the VM's tools when sbox-run.sh runs on the test
// machine. Each logs its call to $FAKE_LOG, one line per call.
var _fakes = map[string]string{
	"ip":         `log ip "$@"`,
	"id":         `echo 999`,
	"mkfs.ext4":  `log mkfs.ext4 "$@"; if [ -n "$FAKE_DRIVE_COPY" ] && [ "$2" = -d ]; then cp -R "$3" "$FAKE_DRIVE_COPY"; fi`,
	"setpriv":    `log setpriv "$@"`,
	"chown":      `log chown "$@"`,
	"truncate":   `for a; do last=$a; done; : >"$last"`,
	"install":    `log install "$@"; for a; do last=$a; done; case " $* " in *" -d "*) mkdir -p "$last" ;; *) : >"$last" ;; esac`,
	"timeout":    `while [ "${1#-}" != "$1" ]; do shift; done; shift; exec "$@"`,
	"setsid":     `exec python3 -c 'import os, sys; os.setsid(); os.execvp(sys.argv[1], sys.argv[1:])' "$@"`,
	"curl":       `log curl "$@"; exit "${FAKE_CURL_EXIT:-0}"`,
	"ss":         `log ss "$@"; case "$*" in *":${FAKE_BUSY_PORT:-none}"*) echo "LISTEN 0 64 127.0.0.1:$FAKE_BUSY_PORT 0.0.0.0:*" ;; esac`,
	"jailer.sh":  _fakeJailer,
	"logfunc.sh": `log() { name=$1; shift; echo "$name $*" >>"$FAKE_LOG"; }`,
}

// _fakeJailer logs its arguments and plays the guest: it prints the run's
// START marker (unless FAKE_JAILER=silent), then exits or, with
// FAKE_JAILER=sleep, keeps running like a live microVM.
const _fakeJailer = `log jailer "$@"
while [ $# -gt 0 ]; do
	[ "$1" = --id ] && jid=$2
	[ "$1" = --chroot-base-dir ] && chroot=$2
	shift
done
id=${jid%-pre}
[ -n "$FAKE_VMJSON" ] && cp "$chroot/firecracker/$jid/root/vm.json" "$FAKE_VMJSON"
if [ "$FAKE_JAILER" != silent ]; then
	echo "SBOX-$(cat "$SBOX_STATE/runs/$id/drive/sbox/nonce") START"
fi
[ "$FAKE_JAILER" = sleep ] && exec sleep 30
exit 0`

// sboxRunEnv is a sandbox for running scripts/sbox-run.sh with fakes.
type sboxRunEnv struct {
	t                       *testing.T
	base, state, jail, fake string
	log                     string
	extra                   []string // more NAME=value settings
}

func newSboxRunEnv(t *testing.T) *sboxRunEnv {
	t.Helper()
	root := t.TempDir()
	e := &sboxRunEnv{
		t:     t,
		base:  filepath.Join(root, "opt"),
		state: filepath.Join(root, "state"),
		jail:  filepath.Join(root, "jail"),
		fake:  filepath.Join(root, "fake"),
		log:   filepath.Join(root, "calls.log"),
	}
	for _, d := range []string{filepath.Join(e.base, "bin"), e.state, e.jail, e.fake} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"vmlinux", "rootfs.ext4"} {
		if err := os.WriteFile(filepath.Join(e.base, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	logFunc := filepath.Join(e.fake, "logfunc.sh")
	for name, body := range _fakes {
		path := filepath.Join(e.fake, name)
		switch name {
		case "logfunc.sh":
			body += "\n"
		case "jailer.sh":
			path = filepath.Join(e.base, "bin", "jailer")
			fallthrough
		default:
			body = "#!/bin/sh\n. " + logFunc + "\n" + body + "\n"
		}
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

// run runs sbox-run.sh with args and stdin, and returns stdout, stderr and
// the exit code.
func (e *sboxRunEnv) run(stdin []byte, args ...string) (string, string, int) {
	e.t.Helper()
	cmd := exec.Command("bash", append([]string{"scripts/sbox-run.sh"}, args...)...)
	cmd.Env = append(os.Environ(),
		"PATH="+e.fake+":"+os.Getenv("PATH"),
		"SBOX_BASE="+e.base, "SBOX_STATE="+e.state, "SBOX_JAIL="+e.jail, "FAKE_LOG="+e.log)
	cmd.Env = append(cmd.Env, e.extra...)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		e.t.Fatal(err)
	}
	return stdout.String(), stderr.String(), code
}

// calls returns the logged calls whose program is name.
func (e *sboxRunEnv) calls(name string) []string {
	e.t.Helper()
	data, err := os.ReadFile(e.log)
	if err != nil && !os.IsNotExist(err) {
		e.t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, name+" "); ok {
			out = append(out, rest)
		}
	}
	return out
}

// runTar is a run's stdin, as the host sends it.
func runTar(t *testing.T, nonce string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := writeRunTar(&buf, t.TempDir(), guestJob{command: []string{"true"}}, nonce, "c0ffee"); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSboxRunOneShotHasNoAPI(t *testing.T) {
	t.Parallel()
	e := newSboxRunEnv(t)

	stdout, stderr, code := e.run(runTar(t, "n0nce"), "0123456789abcdef", "5")
	if code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	jailer := e.calls("jailer")
	if len(jailer) != 1 || !strings.Contains(jailer[0], "-- --no-api --config-file /vm.json") {
		t.Errorf("jailer calls %q, want one with --no-api", jailer)
	}
	if !strings.Contains(stdout, "SBOX-n0nce START") {
		t.Errorf("console %q lacks the guest's START marker", stdout)
	}
}

func TestSboxRunSessionModeHasAPISocket(t *testing.T) {
	t.Parallel()
	e := newSboxRunEnv(t)
	e.extra = []string{"SBOX_SESSION=1"}

	stdout, stderr, code := e.run(runTar(t, "n0nce"), "0123456789abcdef", "0")
	if code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	jailer := e.calls("jailer")
	if len(jailer) != 1 || !strings.Contains(jailer[0], "-- --api-sock /run/firecracker.socket --config-file /vm.json") {
		t.Errorf("jailer calls %q, want one with the API socket", jailer)
	}
	runDir := filepath.Join(e.jail, "firecracker", "0123456789abcdef", "root", "run")
	if chown := e.calls("chown"); len(chown) != 1 || chown[0] != "sbox-vm "+runDir {
		t.Errorf("chown calls %q, want the jail's run folder given to sbox-vm", chown)
	}
}

// deadPID returns the pid of a process that has already exited.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.ProcessState.Pid()
}

// addSession writes a session folder as sbox-run start leaves it.
func (e *sboxRunEnv) addSession(id string, pid int, state string, started int64) {
	e.t.Helper()
	dir := filepath.Join(e.state, "sessions", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.t.Fatal(err)
	}
	for name, body := range map[string]string{"pid": strconv.Itoa(pid), "state": state, "started": strconv.FormatInt(started, 10)} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body+"\n"), 0o644); err != nil {
			e.t.Fatal(err)
		}
	}
}

func TestSboxRunPS(t *testing.T) {
	t.Parallel()
	e := newSboxRunEnv(t)

	if stdout, stderr, code := e.run(nil, "ps"); code != 0 || stdout != "" {
		t.Fatalf("no sessions: exit %d, stdout %q, stderr %q; want exit 0 and no output", code, stdout, stderr)
	}
	e.addSession("aaaaaaaaaaaaaaaa", deadPID(t), "running", 1700000000)
	e.addSession("bbbbbbbbbbbbbbbb", deadPID(t), "paused", 1700000100)
	stdout, stderr, code := e.run(nil, "ps")
	want := "aaaaaaaaaaaaaaaa exited 1700000000\nbbbbbbbbbbbbbbbb exited 1700000100\n"
	if code != 0 || stdout != want {
		t.Errorf("exit %d, stdout %q, stderr %q; want %q", code, stdout, stderr, want)
	}
}

// sessionFile reads one file from a session's folder.
func (e *sboxRunEnv) sessionFile(id, name string) string {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join(e.state, "sessions", id, name))
	if err != nil {
		e.t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

// killSessionOnCleanup ends a session's process group when the test ends,
// whatever the test did.
func (e *sboxRunEnv) killSessionOnCleanup(id string) {
	e.t.Cleanup(func() {
		data, err := os.ReadFile(filepath.Join(e.state, "sessions", id, "pid"))
		if pid, _ := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	})
}

func TestSboxRunStart(t *testing.T) {
	t.Parallel()
	e := newSboxRunEnv(t)
	e.extra = []string{"FAKE_JAILER=sleep"}
	const id = "cccccccccccccccc"
	e.killSessionOnCleanup(id)

	before := time.Now().Unix()
	stdout, stderr, code := e.run(runTar(t, "n0nce"), "start", id)
	if code != 0 || stdout != "READY "+id+"\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q; want READY", code, stdout, stderr)
	}
	if got := e.sessionFile(id, "state"); got != "running" {
		t.Errorf("state %q, want running", got)
	}
	started, _ := strconv.ParseInt(e.sessionFile(id, "started"), 10, 64)
	if started < before || started > time.Now().Unix() {
		t.Errorf("started %d, want between %d and now", started, before)
	}
	if console := e.sessionFile(id, "console.log"); !strings.Contains(console, "SBOX-n0nce START") {
		t.Errorf("console.log %q lacks the guest's output", console)
	}
	if jailer := e.calls("jailer"); len(jailer) != 1 || !strings.Contains(jailer[0], "--api-sock") {
		t.Errorf("jailer calls %q, want one session-mode boot", jailer)
	}
	ps, _, _ := e.run(nil, "ps")
	if want := fmt.Sprintf("%s running %d\n", id, started); ps != want {
		t.Errorf("ps %q, want %q", ps, want)
	}
}

func TestSboxRunStartFailsWhenTheGuestNeverStarts(t *testing.T) {
	t.Parallel()
	e := newSboxRunEnv(t)
	e.extra = []string{"FAKE_JAILER=silent"}
	const id = "dddddddddddddddd"
	e.killSessionOnCleanup(id)

	stdout, stderr, code := e.run(runTar(t, "n0nce"), "start", id)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "did not start") {
		t.Errorf("exit %d, stdout %q, stderr %q; want exit 1 and 'did not start'", code, stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(e.state, "sessions", id)); !os.IsNotExist(err) {
		t.Errorf("session folder left behind (stat: %v)", err)
	}
}

// startSession starts a session whose guest keeps running.
func (e *sboxRunEnv) startSession(id string) int {
	e.t.Helper()
	e.extra = append(e.extra, "FAKE_JAILER=sleep")
	e.killSessionOnCleanup(id)
	if stdout, stderr, code := e.run(runTar(e.t, "n0nce"), "start", id); code != 0 {
		e.t.Fatalf("start: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	pid, err := strconv.Atoi(e.sessionFile(id, "pid"))
	if err != nil {
		e.t.Fatal(err)
	}
	return pid
}

func TestSboxRunStop(t *testing.T) {
	t.Parallel()
	e := newSboxRunEnv(t)
	const id = "eeeeeeeeeeeeeeee"
	pid := e.startSession(id)

	stdout, stderr, code := e.run(nil, "stop", id)
	if code != 0 {
		t.Fatalf("stop: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	flush := "netns exec sbox-" + id + " timeout 10 ssh -i " + filepath.Join(e.state, "sessions", id, "root_key")
	if ip := e.calls("ip"); !slices.ContainsFunc(ip, func(c string) bool { return strings.HasPrefix(c, flush) && strings.HasSuffix(c, " root@172.16.0.2") }) {
		t.Errorf("ip calls %q, want the guest flushed over root's sync-only key before the kill", ip)
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Errorf("session process %d still alive", pid)
	}
	for _, p := range []string{filepath.Join(e.state, "sessions", id), filepath.Join(e.state, "runs", id)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s left behind (stat: %v)", p, err)
		}
	}
	if !slices.Contains(e.calls("ip"), "netns del sbox-"+id) {
		t.Errorf("ip calls %q, want the namespace deleted", e.calls("ip"))
	}
	if stdout, stderr, code := e.run(nil, "stop", id); code != 1 || !strings.Contains(stderr, "no session") {
		t.Errorf("second stop: exit %d, stdout %q, stderr %q; want exit 1, no session", code, stdout, stderr)
	}
}

func TestSboxRunPauseResume(t *testing.T) {
	t.Parallel()
	e := newSboxRunEnv(t)
	const id = "ffffffffffffffff"
	e.startSession(id)
	sock := filepath.Join(e.jail, "firecracker", id, "root", "run", "firecracker.socket")

	steps := []struct {
		args      []string
		curlExit  string
		wantCode  int
		wantState string
		wantCurl  string // the JSON body sent, empty when curl must not run
	}{
		{args: []string{"pause", id}, wantState: "paused", wantCurl: `{"state":"Paused"}`},
		{args: []string{"pause", id}, wantCode: 1, wantState: "paused"},
		{args: []string{"resume", id}, wantState: "running", wantCurl: `{"state":"Resumed"}`},
		{args: []string{"resume", id}, wantCode: 1, wantState: "running"},
		{args: []string{"pause", id}, curlExit: "22", wantCode: 1, wantState: "running", wantCurl: `{"state":"Paused"}`},
		{args: []string{"pause", "0000000000000000"}, wantCode: 1, wantState: "running"},
		{args: []string{"pause", "NOT-HEX"}, wantCode: 2, wantState: "running"},
	}
	for _, s := range steps {
		before := len(e.calls("curl"))
		e.extra = []string{"FAKE_CURL_EXIT=" + s.curlExit}
		stdout, stderr, code := e.run(nil, s.args...)
		if code != s.wantCode {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want exit %d", s.args, code, stdout, stderr, s.wantCode)
		}
		if got := e.sessionFile(id, "state"); got != s.wantState {
			t.Errorf("%v: state %q, want %q", s.args, got, s.wantState)
		}
		curl := e.calls("curl")[before:]
		switch {
		case s.wantCurl == "" && len(curl) != 0:
			t.Errorf("%v: curl called %q, want no call", s.args, curl)
		case s.wantCurl != "" && (len(curl) != 1 || !strings.Contains(curl[0], "--unix-socket "+sock+" ") ||
			!strings.Contains(curl[0], "-X PATCH") || !strings.Contains(curl[0], s.wantCurl) ||
			!strings.HasSuffix(curl[0], "http://localhost/vm")):
			t.Errorf("%v: curl calls %q, want one PATCH /vm with %s on %s", s.args, curl, s.wantCurl, sock)
		}
	}
	ps, _, _ := e.run(nil, "ps")
	if !strings.HasPrefix(ps, id+" running ") {
		t.Errorf("ps %q, want the session running", ps)
	}
}

func TestSboxRunStartStopsStaleSessions(t *testing.T) {
	t.Parallel()
	e := newSboxRunEnv(t)
	now := time.Now().Unix()
	e.addSession("1111111111111111", deadPID(t), "paused", now-2*86400)
	e.addSession("2222222222222222", deadPID(t), "running", now-3600)

	e.startSession("3333333333333333")
	if _, err := os.Stat(filepath.Join(e.state, "sessions", "1111111111111111")); !os.IsNotExist(err) {
		t.Errorf("a two-day-old session is still there (stat: %v)", err)
	}
	if !slices.Contains(e.calls("ip"), "netns del sbox-1111111111111111") {
		t.Errorf("ip calls %q, want the stale session torn down", e.calls("ip"))
	}
	if _, err := os.Stat(filepath.Join(e.state, "sessions", "2222222222222222")); err != nil {
		t.Errorf("an hour-old exited session is gone: %v", err)
	}
}

func TestSboxRunLog(t *testing.T) {
	t.Parallel()
	e := newSboxRunEnv(t)
	const id = "4444444444444444"
	e.startSession(id)

	if stdout, stderr, code := e.run(nil, "log", id); code != 0 || !strings.Contains(stdout, "SBOX-n0nce START") {
		t.Errorf("log: exit %d, stdout %q, stderr %q; want the console", code, stdout, stderr)
	}
	if _, stderr, code := e.run(nil, "log", "5555555555555555"); code != 1 || !strings.Contains(stderr, "no session") {
		t.Errorf("log of a missing session: exit %d, stderr %q; want exit 1, no session", code, stderr)
	}
}

func TestSboxRunSessionModeMakesSSHKeys(t *testing.T) {
	t.Parallel()
	e := newSboxRunEnv(t)
	drive := filepath.Join(t.TempDir(), "drive")
	e.extra = []string{"SBOX_SESSION=1", "FAKE_DRIVE_COPY=" + drive}
	const id = "6666666666666666"

	if stdout, stderr, code := e.run(runTar(t, "n0nce"), id, "0"); code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	session := filepath.Join(e.state, "sessions", id)
	key, err := os.Stat(filepath.Join(session, "key"))
	if err != nil || key.Mode().Perm() != 0o600 {
		t.Fatalf("client key: %v, %v; want a 0600 file in the session folder", err, key)
	}
	pub := e.sessionFile(id, "key.pub")
	authorized, err := os.ReadFile(filepath.Join(drive, "sbox", "ssh", "authorized_keys.runner"))
	if err != nil || strings.TrimSpace(string(authorized)) != pub {
		t.Errorf("drive authorized_keys.runner %q (%v), want the client's public key %q", authorized, err, pub)
	}
	if rootKey, err := os.Stat(filepath.Join(session, "root_key")); err != nil || rootKey.Mode().Perm() != 0o600 {
		t.Errorf("root key: %v, %v; want a 0600 file in the session folder", err, rootKey)
	}
	rootAuthorized, err := os.ReadFile(filepath.Join(drive, "sbox", "ssh", "authorized_keys.root"))
	want := `command="sync",no-pty,no-port-forwarding,no-agent-forwarding,no-X11-forwarding ` + e.sessionFile(id, "root_key.pub")
	if err != nil || strings.TrimSpace(string(rootAuthorized)) != want {
		t.Errorf("drive authorized_keys.root %q (%v), want root's key limited to sync: %q", rootAuthorized, err, want)
	}
	hostKey, err := os.Stat(filepath.Join(drive, "sbox", "ssh", "host_ed25519"))
	if err != nil || hostKey.Mode().Perm() != 0o600 {
		t.Errorf("guest host key on the drive: %v, %v; want a 0600 file", err, hostKey)
	}
	hostPub, err := os.ReadFile(filepath.Join(drive, "sbox", "ssh", "host_ed25519.pub"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := e.sessionFile(id, "known_hosts"), "sbox-guest "+strings.TrimSpace(string(hostPub)); got != want {
		t.Errorf("known_hosts %q, want the guest's host key pinned as %q", got, want)
	}
}

func TestSboxRunOneShotHasNoSSH(t *testing.T) {
	t.Parallel()
	e := newSboxRunEnv(t)
	drive := filepath.Join(t.TempDir(), "drive")
	e.extra = []string{"FAKE_DRIVE_COPY=" + drive}

	if stdout, stderr, code := e.run(runTar(t, "n0nce"), "7777777777777777", "5"); code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(drive, "sbox", "ssh")); !os.IsNotExist(err) {
		t.Errorf("a one-shot run's drive has SSH keys (stat: %v)", err)
	}
}

func TestSboxRunAttach(t *testing.T) {
	t.Parallel()
	e := newSboxRunEnv(t)
	const id = "8888888888888888"
	e.startSession(id)
	session := filepath.Join(e.state, "sessions", id)
	ssh := "netns exec sbox-" + id + " ssh -i " + session + "/key -o UserKnownHostsFile=" + session + "/known_hosts" +
		" -o HostKeyAlias=sbox-guest -o StrictHostKeyChecking=yes -o ConnectionAttempts=10 -o LogLevel=ERROR runner@172.16.0.2 "

	tests := []struct {
		args []string
		want string // the logged ip call
	}{
		{args: []string{"attach", id, "--", "sh", "-c", "echo hi"}, want: ssh + `cd /home/runner/work && exec sh -c echo\ hi`},
		{args: []string{"attach", id}, want: ssh + "cd /home/runner/work && exec bash -l"},
	}
	for _, tt := range tests {
		before := len(e.calls("ip"))
		if stdout, stderr, code := e.run(nil, tt.args...); code != 0 {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", tt.args, code, stdout, stderr)
		}
		if calls := e.calls("ip")[before:]; len(calls) != 1 || calls[0] != tt.want {
			t.Errorf("%v: ip calls %q, want [%q]", tt.args, calls, tt.want)
		}
	}

	if _, stderr, code := e.run(nil, "attach", "9999999999999999"); code != 1 || !strings.Contains(stderr, "no session") {
		t.Errorf("attach to a missing session: exit %d, stderr %q; want exit 1, no session", code, stderr)
	}
	if _, _, code := e.run(nil, "pause", id); code != 0 {
		t.Fatal("pause failed")
	}
	if _, stderr, code := e.run(nil, "attach", id); code != 1 || !strings.Contains(stderr, "not running") {
		t.Errorf("attach to a paused session: exit %d, stderr %q; want exit 1, not running", code, stderr)
	}
}

func TestSboxRunStartWithAProjectDrive(t *testing.T) {
	t.Parallel()
	e := newSboxRunEnv(t)
	vmJSON := filepath.Join(t.TempDir(), "vm.json")
	e.extra = []string{"FAKE_VMJSON=" + vmJSON}
	const id, project = "aaaa000000000001", "0123456789abcdef"
	work := filepath.Join(e.state, "projects", project, "work.ext4")
	e.extra = append(e.extra, "FAKE_JAILER=sleep")
	e.killSessionOnCleanup(id)

	if stdout, stderr, code := e.run(runTar(t, "n0nce"), "start", id, project); code != 0 {
		t.Fatalf("start: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if _, err := os.Stat(work); err != nil {
		t.Fatalf("the project's work drive: %v", err)
	}
	if mkfs := e.calls("mkfs.ext4"); !slices.Contains(mkfs, "-q -L sbox-work -F "+work+".tmp") {
		t.Errorf("mkfs.ext4 calls %q, want the work drive formatted with its label", mkfs)
	}
	if !slices.Contains(e.calls("chown"), "sbox-vm "+work) {
		t.Errorf("chown calls %q, want the work drive given to sbox-vm", e.calls("chown"))
	}
	cfg, err := os.ReadFile(vmJSON)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"drive_id": "work", "path_on_host": "/work.ext4", "is_root_device": false, "is_read_only": false}`; !strings.Contains(string(cfg), want) {
		t.Errorf("vm.json lacks %s:\n%s", want, cfg)
	}
	if got := e.sessionFile(id, "project"); got != project {
		t.Errorf("session's project %q, want %q", got, project)
	}
	ps, _, _ := e.run(nil, "ps")
	if want := fmt.Sprintf("%s running %s %s\n", id, e.sessionFile(id, "started"), project); ps != want {
		t.Errorf("ps %q, want %q", ps, want)
	}
}

func TestSboxRunProjectDriveIsKeptAndExclusive(t *testing.T) {
	t.Parallel()
	e := newSboxRunEnv(t)
	e.extra = []string{"FAKE_JAILER=sleep"}
	const first, second, third, project = "bbbb000000000001", "bbbb000000000002", "bbbb000000000003", "fedcba9876543210"
	for _, id := range []string{first, second, third} {
		e.killSessionOnCleanup(id)
	}
	var stdout string
	start := func(id string) (string, int) {
		var stderr string
		var code int
		stdout, stderr, code = e.run(runTar(t, "n0nce"), "start", id, project)
		return stderr, code
	}

	if stderr, code := start(first); code != 0 {
		t.Fatalf("first start: exit %d, %s", code, stderr)
	}
	if want := "NEW " + first + "\nREADY " + first + "\n"; stdout != want {
		t.Errorf("first start printed %q, want %q: the drive is new", stdout, want)
	}
	if stderr, code := start(second); code != 1 || !strings.Contains(stderr, "in use by session "+first) {
		t.Errorf("second start while the first runs: exit %d, stderr %q; want exit 1, in use by %s", code, stderr, first)
	}
	if _, err := os.Stat(filepath.Join(e.state, "sessions", second)); !os.IsNotExist(err) {
		t.Errorf("the refused session's folder was left behind (stat: %v)", err)
	}
	if _, stderr, code := e.run(nil, "stop", first); code != 0 {
		t.Fatalf("stop: exit %d, %s", code, stderr)
	}
	if stderr, code := start(third); code != 0 {
		t.Fatalf("start after stop: exit %d, %s", code, stderr)
	}
	if want := "READY " + third + "\n"; stdout != want {
		t.Errorf("start on a kept drive printed %q, want %q", stdout, want)
	}
	formats := 0
	for _, c := range e.calls("mkfs.ext4") {
		if strings.Contains(c, "sbox-work") {
			formats++
		}
	}
	if formats != 1 {
		t.Errorf("the work drive was formatted %d times, want once", formats)
	}
}

func TestSboxRunForgetAndClean(t *testing.T) {
	t.Parallel()
	e := newSboxRunEnv(t)
	e.extra = []string{"FAKE_JAILER=sleep"}
	const id, busy, idle = "cccc000000000001", "1111222233334444", "5555666677778888"
	e.killSessionOnCleanup(id)
	projects := filepath.Join(e.state, "projects")
	if _, stderr, code := e.run(runTar(t, "n0nce"), "start", id, busy); code != 0 {
		t.Fatalf("start: exit %d, %s", code, stderr)
	}
	if err := os.MkdirAll(filepath.Join(projects, idle), 0o755); err != nil {
		t.Fatal(err)
	}
	exists := func(p string) bool { _, err := os.Stat(filepath.Join(projects, p)); return err == nil }

	if _, stderr, code := e.run(nil, "forget", busy); code != 1 || !strings.Contains(stderr, "in use by session "+id) || !exists(busy) {
		t.Errorf("forget a busy project: exit %d, stderr %q, kept %v; want exit 1, in use, kept", code, stderr, exists(busy))
	}
	if _, stderr, code := e.run(nil, "clean"); code != 1 || !strings.Contains(stderr, "in use") || !exists(busy) || !exists(idle) {
		t.Errorf("clean with a busy project: exit %d, stderr %q; want exit 1 and nothing removed", code, stderr)
	}
	if _, stderr, code := e.run(nil, "forget", idle); code != 0 || exists(idle) {
		t.Errorf("forget an idle project: exit %d, stderr %q, kept %v; want it removed", code, stderr, exists(idle))
	}
	if _, _, code := e.run(nil, "forget", "NOT-HEX"); code != 2 {
		t.Errorf("forget a bad id: exit %d, want 2", code)
	}
	if _, stderr, code := e.run(nil, "stop", id); code != 0 {
		t.Fatalf("stop: exit %d, %s", code, stderr)
	}
	if _, stderr, code := e.run(nil, "clean"); code != 0 || exists(busy) {
		t.Errorf("clean with nothing running: exit %d, stderr %q; want every project removed", code, stderr)
	}
}

func TestSboxRunStopResumesAPausedSessionToFlushIt(t *testing.T) {
	t.Parallel()
	e := newSboxRunEnv(t)
	const id = "dddd000000000001"
	e.startSession(id)
	if _, _, code := e.run(nil, "pause", id); code != 0 {
		t.Fatal("pause failed")
	}
	if _, stderr, code := e.run(nil, "stop", id); code != 0 {
		t.Fatalf("stop: exit %d, %s", code, stderr)
	}
	curl := e.calls("curl")
	if len(curl) != 2 || !strings.Contains(curl[1], `{"state":"Resumed"}`) {
		t.Errorf("curl calls %q, want the pause and then a resume before the flush", curl)
	}
}

func TestSboxRunForwardsPortsAndServices(t *testing.T) {
	t.Parallel()
	e := newSboxRunEnv(t)
	const id = "eeee000000000001"
	var tarBuf bytes.Buffer
	job := guestJob{command: []string{"true"}, ports: []int{3000}, services: []int{5432}}
	if err := writeRunTar(&tarBuf, t.TempDir(), job, "n0nce", "c0ffee"); err != nil {
		t.Fatal(err)
	}
	if stdout, stderr, code := e.run(tarBuf.Bytes(), id, "5"); code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	run := filepath.Join(e.state, "runs", id)
	asVM := "setpriv --reuid=sbox-vm --regid=sbox-vm --clear-groups python3 " + filepath.Join(e.base, "bin", "sbox-proxy.py")
	inNS := "netns exec sbox-" + id + " " + asVM
	for _, want := range []struct{ prog, call string }{
		{"ip", inNS + " expose " + run + "/fwd/port-3000.sock 172.16.0.2 3000"},
		{"setpriv", "--reuid=sbox-vm --regid=sbox-vm --clear-groups python3 " + filepath.Join(e.base, "bin", "sbox-proxy.py") + " relay 127.0.0.1 3000 " + run + "/fwd/port-3000.sock"},
		{"ip", inNS + " relay 172.16.0.1 5432 " + run + "/fwd/svc-5432.sock"},
		{"setpriv", "--reuid=sbox-vm --regid=sbox-vm --clear-groups python3 " + filepath.Join(e.base, "bin", "sbox-proxy.py") + " expose " + run + "/fwd/svc-5432.sock host.lima.internal 5432"},
	} {
		if !slices.Contains(e.calls(want.prog), want.call) {
			t.Errorf("%s calls %q lack %q", want.prog, e.calls(want.prog), want.call)
		}
	}
}

func TestRunTarCarriesPortsAndServices(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	job := guestJob{command: []string{"true"}, ports: []int{3000, 5173}, services: []int{5432}}
	if err := writeRunTar(&buf, t.TempDir(), job, "n", "c"); err != nil {
		t.Fatal(err)
	}
	files := tarFiles(t, buf.Bytes())
	if files["sbox/ports"] != "3000\n5173\n" || files["sbox/services"] != "5432\n" {
		t.Errorf("sbox/ports %q, sbox/services %q", files["sbox/ports"], files["sbox/services"])
	}
	buf.Reset()
	if err := writeRunTar(&buf, t.TempDir(), guestJob{command: []string{"true"}}, "n", "c"); err != nil {
		t.Fatal(err)
	}
	if files := tarFiles(t, buf.Bytes()); files["sbox/ports"] != "" || files["sbox/services"] != "" {
		t.Error("a run without ports carries port files")
	}
}

func TestSboxRunStartRefusesAPortInUse(t *testing.T) {
	t.Parallel()
	e := newSboxRunEnv(t)
	e.extra = []string{"FAKE_JAILER=sleep", "FAKE_BUSY_PORT=3000"}
	const id = "eeee000000000002"
	e.killSessionOnCleanup(id)
	var tarBuf bytes.Buffer
	if err := writeRunTar(&tarBuf, t.TempDir(), guestJob{command: []string{"true"}, ports: []int{3000}}, "n0nce", "c0ffee"); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := e.run(tarBuf.Bytes(), "start", id)
	if code != 1 || !strings.Contains(stderr, "port 3000 is already in use") {
		t.Errorf("exit %d, stderr %q; want exit 1 naming the port", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(e.state, "sessions", id)); !os.IsNotExist(err) {
		t.Errorf("session folder left behind (stat: %v)", err)
	}
}

func TestSboxRunEgressPatternsAndRawTCP(t *testing.T) {
	t.Parallel()
	e := newSboxRunEnv(t)
	const id = "eeee000000000003"
	var tarBuf bytes.Buffer
	job := guestJob{command: []string{"true"}, egress: []string{"*.github.com:443", "api.anthropic.com:443", "github.com:*", "github.com:22"}}
	if err := writeRunTar(&tarBuf, t.TempDir(), job, "n0nce", "c0ffee"); err != nil {
		t.Fatal(err)
	}
	if stdout, stderr, code := e.run(tarBuf.Bytes(), id, "5"); code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	run := filepath.Join(e.state, "runs", id)
	py := "setpriv --reuid=sbox-vm --regid=sbox-vm --clear-groups python3 " + filepath.Join(e.base, "bin")
	inNS := "netns exec sbox-" + id + " " + py
	for _, want := range []struct{ prog, call string }{
		{"setpriv", strings.TrimPrefix(py, "setpriv ") + "/sbox-proxy.py serve " + run + "/proxy/sock " + run +
			"/egress.log *.github.com:443 api.anthropic.com:443 github.com:* github.com:22"},
		{"ip", inNS + "/sbox-dns.py 172.16.0.1 53 " + run + "/dns.log github.com"},
		// Only sbox's helpers listen in the namespace; a raw port like 22 must be bindable by them.
		{"ip", "netns exec sbox-" + id + " sysctl -qw net.ipv4.ip_unprivileged_port_start=0"},
		{"ip", inNS + "/sbox-proxy.py relay 172.16.0.1 22 " + run + "/fwd/raw-22.sock"},
		{"setpriv", strings.TrimPrefix(py, "setpriv ") + "/sbox-proxy.py expose " + run + "/fwd/raw-22.sock github.com 22"},
	} {
		if !slices.Contains(e.calls(want.prog), want.call) {
			t.Errorf("%s calls %q lack %q", want.prog, e.calls(want.prog), want.call)
		}
	}
}

func TestSboxRunSizesTheMicroVM(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		job  guestJob
		want string
	}{
		{"default", guestJob{command: []string{"true"}}, `"vcpu_count": 2, "mem_size_mib": 1024`},
		{"from the job", guestJob{command: []string{"true"}, vcpus: 4, memoryMiB: 4096}, `"vcpu_count": 4, "mem_size_mib": 4096`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newSboxRunEnv(t)
			vmJSON := filepath.Join(t.TempDir(), "vm.json")
			e.extra = []string{"SBOX_SESSION=1", "FAKE_VMJSON=" + vmJSON}
			var tarball bytes.Buffer
			if err := writeRunTar(&tarball, t.TempDir(), tt.job, "n0nce", "c0ffee"); err != nil {
				t.Fatal(err)
			}
			if stdout, stderr, code := e.run(tarball.Bytes(), "0123456789abcdef", "0"); code != 0 {
				t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
			}
			if cfg, _ := os.ReadFile(vmJSON); !strings.Contains(string(cfg), tt.want) {
				t.Errorf("vm.json lacks %s:\n%s", tt.want, cfg)
			}
		})
	}
}

func TestSboxRunEvents(t *testing.T) {
	t.Parallel()
	e := newSboxRunEnv(t)
	const id = "6666666666666666"
	e.startSession(id)
	run := filepath.Join(e.state, "runs", id)
	for name, body := range map[string]string{
		"egress.log":        "DENY telemetry.example:443\nALLOW api.anthropic.com:443\n",
		"dns.log":           "A telemetry.example\n",
		"drive/sbox/egress": "api.anthropic.com:443\n",
		"drive/sbox/ports":  "3000\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(run, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(run, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stdout, stderr, code := e.run(nil, "events", id)
	want := "RULE api.anthropic.com:443\nPORT 3000\nEGRESS DENY telemetry.example:443\nEGRESS ALLOW api.anthropic.com:443\nDNS A telemetry.example\n"
	if code != 0 || stdout != want {
		t.Errorf("events: exit %d, stdout %q, stderr %q; want %q", code, stdout, stderr, want)
	}
	if _, stderr, code := e.run(nil, "events", "7777777777777777"); code != 1 || !strings.Contains(stderr, "no session") {
		t.Errorf("events of a missing session: exit %d, stderr %q", code, stderr)
	}
}

func TestSboxRunRedactsSecrets(t *testing.T) {
	t.Parallel()
	e := newSboxRunEnv(t)
	const id, secret = "8888888888888888", "ghp_abcdefgh12345678"
	e.startSession(id)
	helper, err := os.ReadFile("scripts/sbox-redact.py")
	if err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(e.state, "runs", id)
	hexed := fmt.Sprintf("%x", secret)
	for name, body := range map[string]string{
		filepath.Join(e.base, "bin", "sbox-redact.py"):        string(helper),
		filepath.Join(run, "drive", "sbox", "secrets"):        "GITHUB_TOKEN=" + secret + "\n",
		filepath.Join(run, "dns.log"):                         "A " + hexed + ".evil.example\n",
		filepath.Join(e.state, "sessions", id, "console.log"): "leaked " + secret + "\n",
	} {
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, cmd := range []string{"events", "log"} {
		stdout, stderr, code := e.run(nil, cmd, id)
		if code != 0 || strings.Contains(stdout, secret) || strings.Contains(stdout, hexed) || !strings.Contains(stdout, "[redacted:GITHUB_TOKEN]") {
			t.Errorf("%s: exit %d, stdout %q, stderr %q; want the secret redacted", cmd, code, stdout, stderr)
		}
	}
}
