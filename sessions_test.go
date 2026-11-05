package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseSessions(t *testing.T) {
	t.Parallel()

	give := "aaaaaaaaaaaaaaaa running 1700000000\n" +
		"junk\n" +
		"bbbbbbbbbbbbbbbb paused notanumber\n" +
		"cccccccccccccccc exited 1700000100\n" +
		"dddddddddddddddd running 1700000200 0123456789abcdef\n"
	want := []session{
		{ID: "aaaaaaaaaaaaaaaa", State: "running", Started: time.Unix(1700000000, 0)},
		{ID: "cccccccccccccccc", State: "exited", Started: time.Unix(1700000100, 0)},
		{ID: "dddddddddddddddd", State: "running", Started: time.Unix(1700000200, 0), Project: "0123456789abcdef"},
	}
	if got := parseSessions(give); !slices.Equal(got, want) {
		t.Errorf("parseSessions = %v, want %v", got, want)
	}
}

func TestWriteSessions(t *testing.T) {
	t.Parallel()

	now := time.Unix(1700100000, 0)
	tests := []struct {
		name string
		give []session
		want string
	}{
		{name: "none", want: "no sessions\n"},
		{
			name: "ages",
			give: []session{
				{ID: "aaaaaaaaaaaaaaaa", State: "running", Started: now.Add(-42 * time.Second)},
				{ID: "bbbbbbbbbbbbbbbb", State: "paused", Started: now.Add(-125 * time.Minute)},
				{ID: "cccccccccccccccc", State: "exited", Started: now.Add(-50 * time.Hour)},
			},
			want: "ID                STATE    AGE\n" +
				"aaaaaaaaaaaaaaaa  running  42s\n" +
				"bbbbbbbbbbbbbbbb  paused   2h5m\n" +
				"cccccccccccccccc  exited   2d2h\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			writeSessions(&buf, tt.give, now)
			if got := buf.String(); got != tt.want {
				t.Errorf("got\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

// fakeVM stands in for sbox-run in the VM: it records each call and
// answers with canned output.
// _fakeVMMu guards every fakeVM's records: sbox dev's exit summary calls in
// parallel, and table tests copy the struct, so it can't hold a lock.
var _fakeVMMu sync.Mutex

type fakeVM struct {
	calls          [][]string
	stdin          []byte   // the last stdin read
	stdins         [][]byte // stdin per call, nil when there was none
	stdout, stderr string
	answer         func(args []string) string // stdout computed from the call, instead of stdout
	errFor         func(args []string) error  // error computed from the call, instead of err
	err            error
}

// withoutSummary drops the calls sbox dev makes for its exit summary
// (TestDevMainPreflightAndSummary covers them).
func withoutSummary(calls [][]string) [][]string {
	return slices.DeleteFunc(slices.Clone(calls), func(c []string) bool {
		return c[0] == "events" || (c[0] == "attach" && slices.Equal(c[2:], []string{"--", "ss", "-Hltn"})) ||
			(c[0] == "attach" && len(c) == 6 && strings.Contains(c[5], "sha256sum")) ||
			(c[0] == "attach" && slices.Contains(c, _denialScript))
	})
}

func (f *fakeVM) run(_ context.Context, in io.Reader, out, errOut io.Writer, args ...string) error {
	var data []byte
	if in != nil {
		var err error
		if data, err = io.ReadAll(in); err != nil {
			return err
		}
	}
	_fakeVMMu.Lock()
	f.calls = append(f.calls, args)
	if in != nil {
		f.stdin = data
	}
	f.stdins = append(f.stdins, data)
	_fakeVMMu.Unlock()
	if f.answer != nil {
		_, _ = io.WriteString(out, f.answer(args))
	} else {
		_, _ = io.WriteString(out, f.stdout)
	}
	_, _ = io.WriteString(errOut, f.stderr)
	if f.errFor != nil {
		return f.errFor(args)
	}
	return f.err
}

func TestSessionMain(t *testing.T) {
	t.Parallel()

	const id = "0123456789abcdef"
	tests := []struct {
		name      string
		cmd       string
		args      []string
		vm        fakeVM
		wantCode  int
		wantOut   string // substring of stdout
		wantErr   string // substring of stderr
		wantCalls [][]string
	}{
		{name: "ps", cmd: "ps", vm: fakeVM{stdout: id + " running 1700000000\n"},
			wantOut: id + "  running", wantCalls: [][]string{{"ps"}}},
		{name: "ps takes no arguments", cmd: "ps", args: []string{id}, wantCode: exitUsage, wantErr: "usage"},
		{name: "stop", cmd: "stop", args: []string{id}, wantOut: id + " stopped", wantCalls: [][]string{{"stop", id}}},
		{name: "pause", cmd: "pause", args: []string{id}, wantOut: id + " paused", wantCalls: [][]string{{"pause", id}}},
		{name: "resume", cmd: "resume", args: []string{id}, wantOut: id + " resumed", wantCalls: [][]string{{"resume", id}}},
		{name: "missing id", cmd: "stop", wantCode: exitUsage, wantErr: "usage"},
		{name: "bad id never reaches the VM", cmd: "pause", args: []string{"NOT-HEX"}, wantCode: exitUsage, wantErr: "session id"},
		{name: "VM-side failure", cmd: "resume", args: []string{id},
			vm:       fakeVM{stderr: "sbox-run: session " + id + " is not paused\n", err: &exec.ExitError{}},
			wantCode: exitFail, wantErr: "is not paused", wantCalls: [][]string{{"resume", id}}},
		{name: "limactl missing", cmd: "ps", vm: fakeVM{err: errors.New(`exec: "limactl": executable file not found in $PATH`)},
			wantCode: exitFail, wantErr: "sbox: exec: \"limactl\"", wantCalls: [][]string{{"ps"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			code := sessionMain(context.Background(), tt.cmd, tt.args, &stdout, &stderr, tt.vm.run)
			if code != tt.wantCode || !strings.Contains(stdout.String(), tt.wantOut) || !strings.Contains(stderr.String(), tt.wantErr) {
				t.Errorf("exit %d, stdout %q, stderr %q; want exit %d, stdout with %q, stderr with %q",
					code, stdout.String(), stderr.String(), tt.wantCode, tt.wantOut, tt.wantErr)
			}
			if !slices.EqualFunc(tt.vm.calls, tt.wantCalls, slices.Equal) {
				t.Errorf("sbox-run calls %q, want %q", tt.vm.calls, tt.wantCalls)
			}
		})
	}
}

// tarFiles reads a run tar into name → content.
func tarFiles(t *testing.T, data []byte) map[string]string {
	t.Helper()
	files := make(map[string]string)
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[hdr.Name] = string(body)
	}
}

func TestStartMain(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	ready := func(args []string) string { return "READY " + args[1] + "\n" }

	t.Run("ready", func(t *testing.T) {
		t.Parallel()
		vm := fakeVM{answer: ready}
		var stdout, stderr bytes.Buffer
		code := startMain(context.Background(), []string{dir, "--", "sh", "-c", "echo hi"}, &stdout, &stderr, vm.run)
		if code != exitOK || len(vm.calls) != 1 {
			t.Fatalf("exit %d, calls %q, stderr %q; want exit 0 and one call", code, vm.calls, stderr.String())
		}
		call := vm.calls[0]
		if len(call) != 2 || call[0] != "start" || !_sessionID.MatchString(call[1]) {
			t.Fatalf("call %q, want start <session id>", call)
		}
		if got := stdout.String(); got != call[1]+"\n" {
			t.Errorf("stdout %q, want the session id", got)
		}
		files := tarFiles(t, vm.stdin)
		if files["skill/app.js"] != "hi" || !strings.Contains(files["sbox/cmd.sh"], "'sh' '-c' 'echo hi'") || len(files["sbox/nonce"]) != 32 {
			t.Errorf("run tar %v lacks the project, the command or the nonce", slices.Sorted(maps.Keys(files)))
		}
	})

	t.Run("no ready line", func(t *testing.T) {
		t.Parallel()
		vm := fakeVM{stdout: "something else\n"}
		var stdout, stderr bytes.Buffer
		code := startMain(context.Background(), []string{dir, "--", "true"}, &stdout, &stderr, vm.run)
		if code != exitFail || stdout.Len() != 0 || !strings.Contains(stderr.String(), "did not report ready") {
			t.Errorf("exit %d, stdout %q, stderr %q; want exit 1 and an explanation", code, stdout.String(), stderr.String())
		}
	})

	t.Run("not a directory", func(t *testing.T) {
		t.Parallel()
		vm := fakeVM{answer: ready}
		var stdout, stderr bytes.Buffer
		code := startMain(context.Background(), []string{filepath.Join(dir, "app.js"), "--", "true"}, &stdout, &stderr, vm.run)
		if code != exitUsage || len(vm.calls) != 0 || !strings.Contains(stderr.String(), "not a directory") {
			t.Errorf("exit %d, calls %q, stderr %q; want usage, no call, not a directory", code, vm.calls, stderr.String())
		}
	})

	for _, args := range [][]string{nil, {dir}, {dir, "true"}, {dir, "--"}} {
		vm := fakeVM{answer: ready}
		var stdout, stderr bytes.Buffer
		if code := startMain(context.Background(), args, &stdout, &stderr, vm.run); code != exitUsage || len(vm.calls) != 0 {
			t.Errorf("args %q: exit %d, calls %q; want usage and no call", args, code, vm.calls)
		}
	}
}

// fakeGuest plays a session whose guest counts once a second, for the
// doctor's probe: wait advances the count unless the session is paused.
type fakeGuest struct {
	id              string
	ticks           int
	paused, stopped bool
	ignorePause     bool // a broken pause: the count keeps moving
	silent          bool // the guest prints no counter
	late            bool // the first log read comes before the first tick
	logs            int
	runTar          []byte // what start was sent
}

func (g *fakeGuest) wait(d time.Duration) {
	if !g.paused || g.ignorePause {
		g.ticks += int(d / time.Second)
	}
}

func (g *fakeGuest) run(_ context.Context, in io.Reader, out, _ io.Writer, args ...string) error {
	if in != nil && args[0] == "start" {
		g.runTar, _ = io.ReadAll(in)
	} else if in != nil {
		_, _ = io.Copy(io.Discard, in)
	}
	switch args[0] {
	case "start":
		g.id = args[1]
		_, _ = fmt.Fprintf(out, "READY %s\n", g.id)
	case "log":
		g.logs++
		if !g.silent && (!g.late || g.logs > 1) {
			_, _ = fmt.Fprintf(out, "noise\r\ntick %d\r\n", g.ticks) // CR LF, as the serial console sends
		}
	case "pause":
		g.paused = true
	case "resume":
		g.paused = false
	case "stop":
		g.stopped = true
	case "ps":
		if !g.stopped {
			_, _ = fmt.Fprintf(out, "%s running 1700000000\n", g.id)
		}
	}
	return nil
}

func TestSessionProbe(t *testing.T) {
	t.Parallel()

	failing := func(checks []check) []string {
		var names []string
		for _, c := range checks {
			if !c.ok {
				names = append(names, c.name)
			}
		}
		return names
	}
	reaches := func(int) bool { return true }
	good := &fakeGuest{}
	var fetched int
	if bad := failing(sessionProbe(context.Background(), good.run, good.wait, func(p int) bool { fetched = p; return true })); len(bad) != 0 || !good.stopped {
		t.Errorf("a well-behaved session: failing %q, stopped %v; want none failing and the session stopped", bad, good.stopped)
	}
	if fwd := tarFiles(t, good.runTar)["sbox/ports"]; fetched < 1024 || fwd != strconv.Itoa(fetched)+"\n" {
		t.Errorf("forwarded %q, fetched port %d; want the forwarded port fetched", fwd, fetched)
	}
	late := &fakeGuest{late: true}
	if bad := failing(sessionProbe(context.Background(), late.run, late.wait, reaches)); len(bad) != 0 {
		t.Errorf("a guest whose first tick comes after the first read: failing %q, want none", bad)
	}
	silent := &fakeGuest{silent: true}
	if bad := failing(sessionProbe(context.Background(), silent.run, silent.wait, reaches)); len(bad) != 3 {
		t.Errorf("a session with no counter output: failing %q, want the three counter checks", bad)
	}
	broken := &fakeGuest{ignorePause: true}
	if bad := failing(sessionProbe(context.Background(), broken.run, broken.wait, reaches)); !slices.Equal(bad, []string{"session: pause freezes the guest"}) || !broken.stopped {
		t.Errorf("a session that ignores pause: failing %q, stopped %v; want only the pause check failing", bad, broken.stopped)
	}
	unreached := &fakeGuest{}
	if bad := failing(sessionProbe(context.Background(), unreached.run, unreached.wait, func(int) bool { return false })); !slices.Equal(bad, []string{"session: a guest port reaches the Mac"}) {
		t.Errorf("a port that doesn't come through: failing %q", bad)
	}
}

// exitStatus returns a real *exec.ExitError carrying code.
func exitStatus(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("sh -c 'exit %d': %v", code, err)
	}
	return err
}

func TestAttachMain(t *testing.T) {
	t.Parallel()
	const id = "0123456789abcdef"
	tests := []struct {
		name      string
		args      []string
		vm        fakeVM
		wantCode  int
		wantCalls [][]string
	}{
		{name: "login shell", args: []string{id}, wantCalls: [][]string{{"attach", id}}},
		{name: "command", args: []string{id, "--", "npm", "test"}, wantCalls: [][]string{{"attach", id, "--", "npm", "test"}}},
		{name: "the guest command's exit code", args: []string{id, "--", "false"}, vm: fakeVM{err: exitStatus(t, 3)},
			wantCode: 3, wantCalls: [][]string{{"attach", id, "--", "false"}}},
		{name: "no id", wantCode: exitUsage},
		{name: "bad id", args: []string{"NOT-HEX"}, wantCode: exitUsage},
		{name: "no command after --", args: []string{id, "--"}, wantCode: exitUsage},
		{name: "junk after the id", args: []string{id, "npm"}, wantCode: exitUsage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stdin := strings.NewReader("typed\n")
			var stdout, stderr bytes.Buffer
			code := attachMain(context.Background(), tt.args, stdin, &stdout, &stderr, tt.vm.run)
			if code != tt.wantCode || !slices.EqualFunc(tt.vm.calls, tt.wantCalls, slices.Equal) {
				t.Errorf("exit %d, calls %q, stderr %q; want exit %d, calls %q", code, tt.vm.calls, stderr.String(), tt.wantCode, tt.wantCalls)
			}
			if len(tt.wantCalls) > 0 && string(tt.vm.stdin) != "typed\n" {
				t.Errorf("the guest got stdin %q, want what was typed", tt.vm.stdin)
			}
		})
	}
}

func TestDevMain(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ready := func(args []string) string {
		if args[0] == "start" {
			return "READY " + args[1] + "\n"
		}
		return ""
	}
	exit3 := exitStatus(t, 3)

	vm := fakeVM{answer: ready, errFor: func(args []string) error {
		if args[0] == "attach" {
			return exit3
		}
		return nil
	}}
	var stdout, stderr bytes.Buffer
	code := devMain(context.Background(), []string{dir, "--", "claude"}, strings.NewReader(""), &stdout, &stderr, vm.run, devHost{stateDir: t.TempDir(), userConfig: "", keep: nil})
	if code != 3 || len(withoutSummary(vm.calls)) != 4 {
		t.Fatalf("exit %d, calls %q, stderr %q; want exit 3 and ps, start, attach, stop", code, withoutSummary(vm.calls), stderr.String())
	}
	id, project := withoutSummary(vm.calls)[1][1], mustProjectID(t, dir)
	want := [][]string{{"ps"}, {"start", id, project}, {"attach", id, "--", "claude"}, {"stop", id}}
	if !slices.EqualFunc(withoutSummary(vm.calls), want, slices.Equal) {
		t.Errorf("calls %q, want %q", withoutSummary(vm.calls), want)
	}

	shell := fakeVM{answer: ready}
	if code := devMain(context.Background(), []string{dir}, strings.NewReader(""), &stdout, &stderr, shell.run, devHost{stateDir: t.TempDir(), userConfig: "", keep: nil}); code != exitOK ||
		len(withoutSummary(shell.calls)) != 4 || !slices.Equal(withoutSummary(shell.calls)[2], []string{"attach", withoutSummary(shell.calls)[1][1]}) {
		t.Errorf("sbox dev <dir>: exit %d, calls %q; want a login shell attached", code, withoutSummary(shell.calls))
	}

	fresh := fakeVM{answer: ready}
	if code := devMain(context.Background(), []string{"-fresh", dir}, strings.NewReader(""), &stdout, &stderr, fresh.run, devHost{stateDir: t.TempDir(), userConfig: "", keep: nil}); code != exitOK ||
		len(withoutSummary(fresh.calls)) != 5 || !slices.Equal(withoutSummary(fresh.calls)[1], []string{"forget", project}) || withoutSummary(fresh.calls)[2][0] != "start" {
		t.Errorf("sbox dev -fresh: exit %d, calls %q; want ps, forget %s, then start, attach, stop", code, withoutSummary(fresh.calls), project)
	}

	failed := fakeVM{stdout: "no ready line\n"}
	if code := devMain(context.Background(), []string{dir}, strings.NewReader(""), &stdout, &stderr, failed.run, devHost{stateDir: t.TempDir(), userConfig: "", keep: nil}); code != exitFail || len(withoutSummary(failed.calls)) != 2 {
		t.Errorf("a session that didn't start: exit %d, calls %q; want exit 1 and no attach", code, withoutSummary(failed.calls))
	}
}

func mustProjectID(t *testing.T, dir string) string {
	t.Helper()
	id, err := projectID(dir)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestProjectID(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	for _, d := range []string{a, b} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(a, link); err != nil {
		t.Fatal(err)
	}

	idA := mustProjectID(t, a)
	if !regexp.MustCompile(`^[a-f0-9]{16}$`).MatchString(idA) {
		t.Errorf("projectID = %q, want 16 hex digits", idA)
	}
	if mustProjectID(t, b) == idA {
		t.Error("two folders share a project id")
	}
	if mustProjectID(t, link) != idA || mustProjectID(t, a+"/.") != idA {
		t.Error("the same folder by another path gets another project id")
	}
	if _, err := projectID(filepath.Join(root, "missing")); err == nil {
		t.Error("a missing folder got a project id")
	}
}

func TestCleanMain(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	base := filepath.Join(state, "projects", "abc", "base.json")
	if err := os.MkdirAll(filepath.Dir(base), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	live := liveProtectFile(state, "abc")
	if err := os.MkdirAll(filepath.Dir(live), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, live, "x\n")
	vm := fakeVM{}
	var stdout, stderr bytes.Buffer
	if code := cleanMain(context.Background(), nil, &stdout, &stderr, vm.run, state); code != exitOK ||
		!slices.EqualFunc(vm.calls, [][]string{{"clean"}}, slices.Equal) || !strings.Contains(stdout.String(), "removed") {
		t.Errorf("sbox clean: exit %d, calls %q, stdout %q", code, vm.calls, stdout.String())
	}
	for _, d := range []string{"projects", "live"} {
		if _, err := os.Stat(filepath.Join(state, d)); !os.IsNotExist(err) {
			t.Errorf("the Mac's %s state is still there (%v); want it removed with the VM's", d, err)
		}
	}
	if _, err := os.Stat(state); err != nil {
		t.Errorf("the state folder itself went too: %v", err)
	}
	if code := cleanMain(context.Background(), []string{"x"}, &stdout, &stderr, vm.run, state); code != exitUsage {
		t.Errorf("sbox clean x: exit %d, want usage", code)
	}
}

func TestDevMainRecordsTheSyncBase(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	project := mustProjectID(t, dir)
	answer := func(prefix string) func([]string) string {
		return func(args []string) string {
			if args[0] == "start" {
				return prefix + "READY " + args[1] + "\n"
			}
			return ""
		}
	}
	run := func(state string, a func([]string) string) {
		vm := fakeVM{answer: a}
		var stdout, stderr bytes.Buffer
		if code := devMain(context.Background(), []string{dir}, strings.NewReader(""), &stdout, &stderr, vm.run, devHost{stateDir: state, userConfig: "", keep: nil}); code != exitOK {
			t.Fatalf("devMain: exit %d, %s", code, stderr.String())
		}
	}

	state := t.TempDir()
	run(state, func(args []string) string {
		if args[0] != "start" {
			return ""
		}
		return answer("NEW " + args[1] + "\n")(args)
	})
	base, err := loadBase(state, project)
	want, _ := readManifest(dir, syncRules(nil))
	if err != nil || !maps.Equal(base, want) {
		t.Errorf("base after a new drive: %v, %v; want the folder's manifest %v", base, err, want)
	}

	kept := t.TempDir()
	run(kept, answer(""))
	if _, err := loadBase(kept, project); err == nil {
		t.Error("a kept drive wrote a base it can't know")
	}
}

// TestDevMainLive: a live session gets the server's port and token
// instead of the project, and nothing is synced at boot, since the guest
// works on the folder itself.
func TestDevMainLive(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sbox.yaml"), []byte("dev: {mount: live, local: [node_modules]}\nprotect: [config/prod.env]\nhide: [.env]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var started []string
	state := t.TempDir()
	stale := liveRefusedFile(state, mustProjectID(t, dir))
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, stale, "write from-an-earlier-session\n")
	host := devHost{stateDir: state, userConfig: allowAll(t), startLive: func(project, folder string, readOnly bool) (*liveRelay, error) {
		started = append(started, project+" "+folder)
		f, err := os.OpenFile(liveRefusedFile(state, project), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
		if err == nil {
			_, _ = f.WriteString("write .git/config\n") // as the server does when the guest is refused
			_ = f.Close()
		}
		return &liveRelay{port: 4242, token: []byte("token")}, err
	}}
	vm := fakeVM{answer: func(args []string) string {
		if args[0] == "start" {
			return "READY " + args[1] + "\n"
		}
		return ""
	}}
	var stdout, stderr bytes.Buffer
	if code := devMain(context.Background(), []string{dir}, strings.NewReader(""), &stdout, &stderr, vm.run, host); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if want := mustProjectID(t, dir) + " " + dir; !slices.Equal(started, []string{want}) {
		t.Errorf("startLive calls %q, want %q", started, want)
	}
	files := tarFiles(t, vm.stdins[1])
	if files["sbox/live"] != "4242\n" || files["live.token"] != "token" || files["skill/sbox.yaml"] != "" || files["sbox/local"] != "node_modules\n" {
		t.Errorf("run tar: sbox/live %q, live.token %q, skill/sbox.yaml %q, sbox/local %q", files["sbox/live"], files["live.token"], files["skill/sbox.yaml"], files["sbox/local"])
	}
	if got, err := os.ReadFile(liveProtectFile(host.stateDir, mustProjectID(t, dir))); string(got) != "config/prod.env\n" {
		t.Errorf("the server's protect file: %q, %v", got, err)
	}
	if got, err := os.ReadFile(liveHideFile(host.stateDir, mustProjectID(t, dir))); string(got) != ".env\n" {
		t.Errorf("the server's hide file: %q, %v", got, err)
	}
	for _, c := range vm.calls {
		if c[0] == "attach" && len(c) > 2 && strings.Contains(strings.Join(c, " "), "tar") {
			t.Errorf("a live session synced at boot: %q", c)
		}
	}
	if _, err := loadBase(host.stateDir, mustProjectID(t, dir)); err == nil {
		t.Error("a live session recorded a sync base")
	}
	for _, kv := range []string{"WATCHPACK_POLLING=true", "CHOKIDAR_USEPOLLING=1", "GIT_OPTIONAL_LOCKS=0"} {
		if !strings.Contains(files["sbox/env"], kv+"\n") {
			t.Errorf("sbox/env lacks %s:\n%s", kv, files["sbox/env"])
		}
	}
	for _, want := range []string{"hidden: .env", "live: the changes are on the Mac already", "BLOCKED live    .git/config  read-only in a live session (write ×1)"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr.String())
		}
	}
	if strings.Contains(stderr.String(), "from-an-earlier-session") {
		t.Errorf("an earlier session's refusals are in this one's summary:\n%s", stderr.String())
	}

	// Attached again, the server is started again if it stopped.
	running := fakeVM{answer: func(args []string) string {
		if args[0] == "ps" {
			return "abcdef0123456789 running 1700000000 " + mustProjectID(t, dir) + "\n"
		}
		return ""
	}}
	started = nil
	stderr.Reset()
	if code := devMain(context.Background(), []string{dir}, strings.NewReader(""), &stdout, &stderr, running.run, host); code != exitOK || len(started) != 1 {
		t.Errorf("reattach: exit %d, startLive calls %q\n%s", code, started, stderr.String())
	}
}

// TestSyncLive: a live session has nothing to sync, and sbox sync says so.
func TestSyncLive(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "sbox.yaml"), "dev: {mount: live}\n")
	vm := fakeVM{}
	var stdout, stderr bytes.Buffer
	if code := syncMain(context.Background(), []string{dir}, &stdout, &stderr, vm.run, devHost{stateDir: t.TempDir(), userConfig: allowAll(t)}); code != exitOK ||
		len(vm.calls) != 0 || !strings.Contains(stdout.String(), "live: the guest works on the folder itself, nothing to sync") {
		t.Errorf("sbox sync on a live project: exit %d, calls %q, stdout %q, stderr %q", code, vm.calls, stdout.String(), stderr.String())
	}
}

func TestDevMainForwardsConfiguredPorts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	user := filepath.Join(t.TempDir(), "config.yaml")
	for p, body := range map[string]string{filepath.Join(dir, "sbox.yaml"): "ports: [3000]\n", user: "services: [5432]\negress: [api.anthropic.com]\n"} {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	vm := fakeVM{answer: func(args []string) string {
		if args[0] == "start" {
			return "READY " + args[1] + "\n"
		}
		return ""
	}}
	var stdout, stderr bytes.Buffer
	if code := devMain(context.Background(), []string{dir}, strings.NewReader(""), &stdout, &stderr, vm.run, devHost{stateDir: t.TempDir(), userConfig: user, keep: nil}); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	files := tarFiles(t, vm.stdins[1]) // start's run tar, after ps
	if files["sbox/ports"] != "3000\n" || files["sbox/services"] != "5432\n" || files["sbox/egress"] != "api.anthropic.com:443\n" {
		t.Errorf("run tar: sbox/ports %q, sbox/services %q, sbox/egress %q", files["sbox/ports"], files["sbox/services"], files["sbox/egress"])
	}

	if err := os.WriteFile(filepath.Join(dir, "sbox.yaml"), []byte("ports: [80]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := fakeVM{}
	if code := devMain(context.Background(), []string{dir}, strings.NewReader(""), &stdout, &stderr, bad.run, devHost{stateDir: t.TempDir(), userConfig: user, keep: nil}); code != exitUsage ||
		len(bad.calls) != 0 || !strings.Contains(stderr.String(), "sbox.yaml") {
		t.Errorf("a bad sbox.yaml: exit %d, calls %q, stderr %q; want usage, no VM call, the file named", code, bad.calls, stderr.String())
	}
}

func TestDevMainReattaches(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	project := mustProjectID(t, dir)
	started := strconv.FormatInt(time.Now().Add(-2*time.Hour).Unix(), 10)
	const live = "0123abcd"
	ps := func(lines string) func([]string) string {
		return func(args []string) string {
			switch args[0] {
			case "ps":
				return lines
			case "start":
				return "READY " + args[1] + "\n"
			}
			return ""
		}
	}
	keep := func(string, int) bool { return true }
	stop := func(string, int) bool { return false }

	tests := []struct {
		name  string
		args  []string
		ps    string
		keep  func(string, int) bool
		want  [][]string // "NEW" stands for the new session's id
		notes string     // on stderr
	}{
		{name: "running", args: []string{dir}, ps: live + " running " + started + " " + project + "\n",
			want: [][]string{{"ps"}, {"attach", live}, {"stop", live}}, notes: "attached to session " + live + ", running 2h"},
		{name: "paused is resumed", args: []string{dir}, ps: live + " paused " + started + " " + project + "\n",
			want: [][]string{{"ps"}, {"resume", live}, {"attach", live}, {"stop", live}}, notes: "attached to session " + live},
		{name: "kept at detach", args: []string{dir}, ps: live + " running " + started + " " + project + "\n", keep: keep,
			want: [][]string{{"ps"}, {"attach", live}}, notes: "sbox stop " + live},
		{name: "stopped at detach", args: []string{dir}, ps: live + " running " + started + " " + project + "\n", keep: stop,
			want: [][]string{{"ps"}, {"attach", live}, {"stop", live}}},
		{name: "exited and other projects are not reused", args: []string{dir},
			ps:   live + " exited " + started + " " + project + "\nffff0000 running " + started + " 0000000000000000\n",
			want: [][]string{{"ps"}, {"start", "NEW", project}, {"attach", "NEW"}, {"stop", "NEW"}}},
		{name: "-fresh stops the live session first", args: []string{"-fresh", dir}, ps: live + " running " + started + " " + project + "\n",
			want: [][]string{{"ps"}, {"stop", live}, {"forget", project}, {"start", "NEW", project}, {"attach", "NEW"}, {"stop", "NEW"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			vm := fakeVM{answer: ps(tt.ps)}
			var stdout, stderr bytes.Buffer
			if code := devMain(context.Background(), tt.args, strings.NewReader(""), &stdout, &stderr, vm.run, devHost{stateDir: t.TempDir(), userConfig: "", keep: tt.keep}); code != exitOK {
				t.Fatalf("exit %d: %s", code, stderr.String())
			}
			var newID string
			for _, c := range withoutSummary(vm.calls) {
				if c[0] == "start" {
					newID = c[1]
				}
			}
			want := make([][]string, len(tt.want))
			for i, c := range tt.want {
				want[i] = slices.Clone(c)
				for j := range want[i] {
					if want[i][j] == "NEW" {
						want[i][j] = newID
					}
				}
			}
			if !slices.EqualFunc(withoutSummary(vm.calls), want, slices.Equal) {
				t.Errorf("calls %q, want %q", withoutSummary(vm.calls), want)
			}
			if !strings.Contains(stderr.String(), tt.notes) {
				t.Errorf("stderr %q, want %q", stderr.String(), tt.notes)
			}
		})
	}
}

func TestAskKeep(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		answer   string
		unsynced int
		want     bool
		asked    string
	}{
		{"\n", 3, true, "the guest has 3 changes not synced; keep session 0123abcd running? [Y/n]"},
		{"n\n", 3, false, "[Y/n]"},
		{"\n", 0, false, "keep session 0123abcd running? [y/N]"},
		{"y\n", 0, true, "[y/N]"},
		{"Yes\n", 0, true, "[y/N]"},
		{"\n", -1, true, "keep session 0123abcd running? [Y/n]"},
		{"", 1, true, "1 change not synced"},
	} {
		var out bytes.Buffer
		if got := askKeep(strings.NewReader(tt.answer), &out)("0123abcd", tt.unsynced); got != tt.want || !strings.Contains(out.String(), tt.asked) {
			t.Errorf("answer %q, %d unsynced: keep %v, asked %q; want %v and %q", tt.answer, tt.unsynced, got, out.String(), tt.want, tt.asked)
		}
	}
}

func TestDevMainSaysWhereToolsGo(t *testing.T) {
	t.Parallel()
	for _, newDrive := range []bool{true, false} {
		vm := fakeVM{answer: func(args []string) string {
			switch {
			case args[0] == "start" && newDrive:
				return "NEW " + args[1] + "\nREADY " + args[1] + "\n"
			case args[0] == "start":
				return "READY " + args[1] + "\n"
			}
			return ""
		}}
		var stdout, stderr bytes.Buffer
		if code := devMain(context.Background(), []string{t.TempDir()}, strings.NewReader(""), &stdout, &stderr, vm.run, devHost{stateDir: t.TempDir(), userConfig: "", keep: nil}); code != exitOK {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
		if said := strings.Contains(stderr.String(), "the root is read-only"); said != newDrive {
			t.Errorf("new drive %v: stderr %q; want the note only on a new home", newDrive, stderr.String())
		}
	}
}

func TestDevMainRunsHooks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	yaml := "dev:\n  env: {NODE_ENV: development, A: \"1 2\"}\n  setup: [npm ci]\n  start: [\"npm run dev &\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "sbox.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	project := mustProjectID(t, dir)
	script := hookScript([]string{"npm ci"}, []string{"npm run dev &"}, 10*time.Minute, false, false)
	run := func(ps string, hooksFail bool) (*fakeVM, string) {
		vm := &fakeVM{answer: func(args []string) string {
			switch args[0] {
			case "ps":
				return ps
			case "start":
				return "READY " + args[1] + "\n"
			}
			return ""
		}, errFor: func(args []string) error {
			if hooksFail && args[0] == "attach" && slices.Contains(args, script) {
				return exitStatus(t, 1)
			}
			return nil
		}}
		var stdout, stderr bytes.Buffer
		if code := devMain(context.Background(), []string{dir, "--", "claude"}, strings.NewReader(""), &stdout, &stderr, vm.run, devHost{stateDir: t.TempDir(), userConfig: "", keep: nil}); code != exitOK {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
		return vm, stderr.String()
	}

	vm, _ := run("", false)
	id := withoutSummary(vm.calls)[1][1]
	want := [][]string{{"ps"}, {"start", id, project}, {"attach", id, "--", "sh", "-c", script}, {"attach", id, "--", "claude"}, {"stop", id}}
	if !slices.EqualFunc(withoutSummary(vm.calls), want, slices.Equal) {
		t.Errorf("fresh boot: calls %q, want the hooks before the terminal", withoutSummary(vm.calls))
	}
	if env := tarFiles(t, vm.stdins[1])["sbox/env"]; env != "A=1 2\nCLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1\nNODE_ENV=development\n" {
		t.Errorf("sbox/env = %q, want the variables, sorted, one per line", env)
	}

	vm, notes := run("", true)
	if len(withoutSummary(vm.calls)) != 5 || withoutSummary(vm.calls)[3][3] != "claude" || !strings.Contains(notes, "sbox setup-env") {
		t.Errorf("failed hooks: calls %q, stderr %q; want the terminal opened anyway and how to rerun", withoutSummary(vm.calls), notes)
	}

	vm, _ = run("0123abcd running 1700000000 "+project+"\n", false)
	if slices.ContainsFunc(withoutSummary(vm.calls), func(c []string) bool { return slices.Contains(c, script) }) {
		t.Errorf("reattach: calls %q, want no hooks", withoutSummary(vm.calls))
	}
}

func TestSetupEnvMain(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sbox.yaml"), []byte("dev:\n  env: {A: b}\n  setup: [npm ci]\n  start: [\"npm run dev &\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	project := mustProjectID(t, dir)
	script := hookScript([]string{"npm ci"}, nil, 10*time.Minute, true, false)
	vmWith := func(ps string) *fakeVM {
		return &fakeVM{answer: func(args []string) string {
			switch args[0] {
			case "ps":
				return ps
			case "start":
				return "READY " + args[1] + "\n"
			}
			return ""
		}, errFor: func(args []string) error {
			if args[0] == "attach" {
				return exitStatus(t, 2)
			}
			return nil
		}}
	}
	var stdout, stderr bytes.Buffer

	live := vmWith("0123abcd running 1700000000 " + project + "\n")
	if code := setupEnvMain(context.Background(), []string{dir}, &stdout, &stderr, live.run, devHost{stateDir: t.TempDir(), userConfig: ""}); code != 2 ||
		!slices.EqualFunc(live.calls, [][]string{{"ps"}, {"attach", "0123abcd", "--", "sh", "-c", script}}, slices.Equal) {
		t.Errorf("live session: exit %d, calls %q; want setup forced in it and its exit status", code, live.calls)
	}

	none := vmWith("")
	if code := setupEnvMain(context.Background(), []string{dir}, &stdout, &stderr, none.run, devHost{stateDir: t.TempDir(), userConfig: ""}); code != 2 || len(none.calls) != 4 ||
		none.calls[1][0] != "start" || none.calls[3][0] != "stop" || tarFiles(t, none.stdins[1])["sbox/env"] != "A=b\nCLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1\n" {
		t.Errorf("no session: exit %d, calls %q; want one started with the config, then stopped", code, none.calls)
	}

	empty := t.TempDir()
	quiet := vmWith("")
	if code := setupEnvMain(context.Background(), []string{empty}, &stdout, &stderr, quiet.run, devHost{stateDir: t.TempDir(), userConfig: ""}); code != exitOK || len(quiet.calls) != 0 {
		t.Errorf("no setup steps: exit %d, calls %q; want nothing done", code, quiet.calls)
	}
	if code := setupEnvMain(context.Background(), nil, &stdout, &stderr, quiet.run, devHost{stateDir: t.TempDir(), userConfig: ""}); code != exitUsage {
		t.Errorf("no folder: exit %d, want usage", code)
	}
}

// tarMode returns the mode of name in a tar, or -1.
func tarMode(t *testing.T, data []byte, name string) int64 {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err != nil {
			return -1
		}
		if hdr.Name == name {
			return hdr.Mode
		}
	}
}

func TestDevMainSecrets(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sbox.yaml"), []byte("secrets: {GITHUB_TOKEN: env:GH_SBOX}\ndev: {env: {GITHUB_TOKEN: fake}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	host := map[string]string{"GH_SBOX": "ghp_real"}
	lookup := func(k string) (string, bool) { v, ok := host[k]; return v, ok }
	vm := fakeVM{answer: func(args []string) string {
		if args[0] == "start" {
			return "READY " + args[1] + "\n"
		}
		return ""
	}}
	var stdout, stderr bytes.Buffer
	if code := devMain(context.Background(), []string{dir}, strings.NewReader(""), &stdout, &stderr, vm.run, devHost{stateDir: t.TempDir(), lookupEnv: lookup, userConfig: allowAll(t)}); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	run := vm.stdins[1]
	if got := tarFiles(t, run)["sbox/secrets"]; got != "GITHUB_TOKEN=ghp_real\n" || tarMode(t, run, "sbox/secrets") != 0o400 {
		t.Errorf("sbox/secrets = %q, mode %o; want the value, root-only", got, tarMode(t, run, "sbox/secrets"))
	}
	if !strings.Contains(stderr.String(), "secrets  GITHUB_TOKEN") || strings.Contains(stderr.String()+stdout.String(), "ghp_real") {
		t.Errorf("stderr %q: want the secret's name and never its value", stderr.String())
	}

	delete(host, "GH_SBOX")
	none := fakeVM{}
	stderr.Reset()
	if code := devMain(context.Background(), []string{dir}, strings.NewReader(""), &stdout, &stderr, none.run, devHost{stateDir: t.TempDir(), lookupEnv: lookup, userConfig: allowAll(t)}); code != exitUsage ||
		len(none.calls) != 0 || !strings.Contains(stderr.String(), "GH_SBOX") {
		t.Errorf("unset host variable: exit %d, calls %q, stderr %q; want usage before any VM call", code, none.calls, stderr.String())
	}
}

func TestDevMainAgentSettings(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sbox.yaml"), []byte("dev: {command: claude --continue}\npermissions: {deny: [WebFetch]}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	vm := fakeVM{answer: func(args []string) string {
		if args[0] == "start" {
			return "READY " + args[1] + "\n"
		}
		return ""
	}}
	var stdout, stderr bytes.Buffer
	if code := devMain(context.Background(), []string{dir}, strings.NewReader(""), &stdout, &stderr, vm.run, devHost{stateDir: t.TempDir()}); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	id := vm.calls[1][1]
	if !slices.Equal(vm.calls[2], []string{"attach", id, "--", "bash", "-lc", "claude --continue"}) {
		t.Errorf("attach %q, want dev.command in a login shell", vm.calls[2])
	}
	cfg, _ := loadConfig("", filepath.Join(dir, "sbox.yaml"))
	files := tarFiles(t, vm.stdins[1])
	if files["sbox/claude.json"] != string(cfg.claudeSettings()) || files["sbox/claude.py"] != _claudeScript {
		t.Errorf("run tar: claude.json %q, claude.py %d bytes; want the settings and the helper", files["sbox/claude.json"], len(files["sbox/claude.py"]))
	}

	// Claude Code's telemetry would be a BLOCKED line in every summary.
	if !strings.Contains(files["sbox/env"], "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1\n") {
		t.Errorf("sbox/env %q, want Claude Code's non-essential traffic off", files["sbox/env"])
	}

	// The key that signs the guest's merge mark: one per project, kept.
	state := t.TempDir()
	key := func(dir string) string {
		vm := fakeVM{answer: func(args []string) string {
			if args[0] == "start" {
				return "READY " + args[1] + "\n"
			}
			return ""
		}}
		var stdout, stderr bytes.Buffer
		if code := devMain(context.Background(), []string{dir}, strings.NewReader(""), &stdout, &stderr, vm.run, devHost{stateDir: state}); code != exitOK {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
		return tarFiles(t, vm.stdins[1])["sbox/claude.key"]
	}
	first := key(dir)
	if len(first) != 64 || key(dir) != first {
		t.Errorf("sbox/claude.key %q, then another; want 64 hex characters, the same each boot", first)
	}
	if key(t.TempDir()) == first {
		t.Error("two projects share a key")
	}
}

func TestDevMainPreflightAndSummary(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sbox.yaml"), []byte("egress: [api.anthropic.com]\nports: [3000]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	vm := fakeVM{answer: func(args []string) string {
		switch {
		case args[0] == "start":
			return "READY " + args[1] + "\n"
		case args[0] == "events":
			return "RULE api.anthropic.com:443\nEGRESS ALLOW api.anthropic.com:443\nEGRESS DENY telemetry.example:443\n"
		case args[0] == "attach" && slices.Contains(args, "ss"):
			return "LISTEN 0 511 0.0.0.0:5173 0.0.0.0:*\n"
		case args[0] == "attach" && slices.Contains(args, _denialScript):
			return "permission-rule\tWebFetch\n"
		}
		return ""
	}}
	var stdout, stderr bytes.Buffer
	if code := devMain(context.Background(), []string{dir, "--", "claude"}, strings.NewReader(""), &stdout, &stderr, vm.run, devHost{stateDir: t.TempDir(), userConfig: allowAll(t)}); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	id := vm.calls[1][1]
	want := [][]string{{"ps"}, {"start", id, mustProjectID(t, dir)}, {"attach", id, "--", "claude"}, {"events", id}, {"attach", id, "--", "ss", "-Hltn"}, {"stop", id}}
	if !slices.EqualFunc(withoutSummary(vm.calls), [][]string{want[0], want[1], want[2], want[5]}, slices.Equal) ||
		!slices.ContainsFunc(vm.calls, func(c []string) bool { return slices.Equal(c, want[3]) }) ||
		!slices.ContainsFunc(vm.calls, func(c []string) bool { return slices.Equal(c, want[4]) }) {
		t.Errorf("calls %q, want %q", vm.calls, want)
	}
	out := stderr.String()
	for _, want := range []string{"egress   api.anthropic.com:443", "ports    3000", "session 0s", "BLOCKED egress  telemetry.example:443", "BLOCKED port    5173",
		"BLOCKED tool    WebFetch ×1  denied by a permission rule"} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "ports    3000") > strings.Index(out, "session 0s") {
		t.Errorf("the preflight should come before the summary:\n%s", out)
	}
}

func TestIsTerminal(t *testing.T) {
	t.Parallel()
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = null.Close() }()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	defer func() { _ = w.Close() }()
	file, err := os.CreateTemp(t.TempDir(), "f")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	for name, f := range map[string]*os.File{"/dev/null": null, "a pipe": r, "a file": file} {
		if isTerminal(f) {
			t.Errorf("isTerminal(%s) = true, want false", name)
		}
	}
	if pty := openTerminal(t); pty != nil && !isTerminal(pty) {
		t.Error("isTerminal(a pseudo-terminal) = false, want true")
	}
}

func TestDevMainMounts(t *testing.T) {
	t.Parallel()
	dir, home := t.TempDir(), t.TempDir()
	writeTestFile(t, filepath.Join(dir, "sbox.yaml"), "mounts:\n  - {host: ~/.gitconfig, guest: /home/runner/.gitconfig}\n  - {host: ~/tools, guest: /home/runner/tools}\n")
	writeTestFile(t, filepath.Join(home, ".gitconfig"), "[user]\n\tname = me\n")
	if err := os.MkdirAll(filepath.Join(home, "tools", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(home, "tools", "bin", "x"), "#!/bin/sh\n")
	vm := fakeVM{answer: func(args []string) string {
		if args[0] == "start" {
			return "READY " + args[1] + "\n"
		}
		return ""
	}}
	var stdout, stderr bytes.Buffer
	if code := devMain(context.Background(), []string{dir}, strings.NewReader(""), &stdout, &stderr, vm.run, devHost{stateDir: t.TempDir(), home: home, userConfig: allowAll(t)}); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	files := tarFiles(t, vm.stdins[1])
	if files["mounts/0"] != "[user]\n\tname = me\n" || files["mounts/1/bin/x"] != "#!/bin/sh\n" ||
		files["sbox/mounts"] != "0 /home/runner/.gitconfig\n1 /home/runner/tools\n" {
		t.Errorf("run tar: mounts/0 %q, mounts/1/bin/x %q, sbox/mounts %q", files["mounts/0"], files["mounts/1/bin/x"], files["sbox/mounts"])
	}
	if !strings.Contains(stderr.String(), "mounts   ~/.gitconfig → /home/runner/.gitconfig, ~/tools → /home/runner/tools") {
		t.Errorf("preflight lacks the mounts:\n%s", stderr.String())
	}

	if err := os.Remove(filepath.Join(home, ".gitconfig")); err != nil {
		t.Fatal(err)
	}
	none := fakeVM{}
	stderr.Reset()
	if code := devMain(context.Background(), []string{dir}, strings.NewReader(""), &stdout, &stderr, none.run, devHost{stateDir: t.TempDir(), home: home, userConfig: allowAll(t)}); code != exitUsage ||
		len(none.calls) != 0 || !strings.Contains(stderr.String(), "~/.gitconfig") {
		t.Errorf("missing mount: exit %d, calls %q, stderr %q; want usage before any VM call", code, none.calls, stderr.String())
	}
}

func TestDevMainConfigChangedAndRestart(t *testing.T) {
	t.Parallel()
	dir, state := t.TempDir(), t.TempDir()
	project := mustProjectID(t, dir)
	writeTestFile(t, filepath.Join(dir, "sbox.yaml"), "egress: [api.anthropic.com]\n")
	var live string // the session the fake reports running
	vm := &fakeVM{answer: func(args []string) string {
		switch args[0] {
		case "ps":
			if live != "" {
				return live + " running 1700000000 " + project + "\n"
			}
		case "start":
			live = args[1]
			return "READY " + args[1] + "\n"
		case "stop":
			live = ""
		}
		return ""
	}}
	keep := func(string, int) bool { return true }
	dev := func(args ...string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := devMain(context.Background(), append(args, dir), strings.NewReader(""), &stdout, &stderr, vm.run, devHost{stateDir: state, keep: keep, userConfig: allowAll(t)}); code != exitOK {
			t.Fatalf("sbox dev %q: exit %d: %s", args, code, stderr.String())
		}
		return stderr.String()
	}

	dev()
	first := live
	if notes := dev(); strings.Contains(notes, "config changed") {
		t.Errorf("reattach with the same config: %q, want no warning", notes)
	}
	writeTestFile(t, filepath.Join(dir, "sbox.yaml"), "dev: {vcpus: 4, memory_mib: 4096}\n")
	notes := dev()
	if !strings.Contains(notes, "the config changed since session "+first+" started (dev.memory_mib, dev.vcpus)") || !strings.Contains(notes, "sbox dev -restart "+dir) {
		t.Errorf("reattach after a change: %q, want the keys and how to apply them", notes)
	}

	vm.calls = nil
	notes = dev("-restart")
	if live == first || !slices.ContainsFunc(vm.calls, func(c []string) bool { return slices.Equal(c, []string{"stop", first}) }) ||
		slices.ContainsFunc(vm.calls, func(c []string) bool { return c[0] == "forget" }) {
		t.Errorf("-restart: calls %q; want the old session stopped, a new one started, the home kept", vm.calls)
	}
	if strings.Contains(notes, "config changed") {
		t.Errorf("-restart: %q, want no warning for the session it just started", notes)
	}
	if notes := dev(); strings.Contains(notes, "config changed") {
		t.Errorf("reattach after -restart: %q, want no warning", notes)
	}

	var stderr bytes.Buffer
	if code := devMain(context.Background(), []string{"-restart", "-fresh", dir}, strings.NewReader(""), io.Discard, &stderr, vm.run, devHost{stateDir: state, userConfig: allowAll(t)}); code != exitUsage {
		t.Errorf("-restart -fresh: exit %d, want usage", code)
	}
}

func TestSetupEnvMainWarnsAboutAnOlderConfig(t *testing.T) {
	t.Parallel()
	dir, state := t.TempDir(), t.TempDir()
	project := mustProjectID(t, dir)
	writeTestFile(t, filepath.Join(dir, "sbox.yaml"), "dev: {setup: [npm ci]}\n")
	old, _ := loadConfig("", filepath.Join(dir, "sbox.yaml"))
	if err := recordBoot(state, project, "0123abcd", "", old); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "sbox.yaml"), "dev: {setup: [npm ci]}\negress: [registry.npmjs.org]\n")
	vm := fakeVM{answer: func(args []string) string {
		if args[0] == "ps" {
			return "0123abcd running 1700000000 " + project + "\n"
		}
		return ""
	}}
	var stdout, stderr bytes.Buffer
	if code := setupEnvMain(context.Background(), []string{dir}, &stdout, &stderr, vm.run, devHost{stateDir: state, userConfig: allowAll(t)}); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "the config changed since session 0123abcd started (egress)") {
		t.Errorf("stderr %q, want the warning before the steps run with the old network", stderr.String())
	}
}

func TestDevFlagsConfigAndCommand(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "sbox.yaml"), "egress: [project.example]\ndev: {command: claude}\n")
	other := filepath.Join(t.TempDir(), "ci.yaml")
	writeTestFile(t, other, "egress: [other.example]\n")
	run := func(args ...string) (*fakeVM, string, int) {
		vm := &fakeVM{answer: func(a []string) string {
			if a[0] == "start" {
				return "READY " + a[1] + "\n"
			}
			return ""
		}}
		var stdout, stderr bytes.Buffer
		code := devMain(context.Background(), args, strings.NewReader(""), &stdout, &stderr, vm.run, devHost{stateDir: t.TempDir(), userConfig: allowAll(t)})
		return vm, stderr.String(), code
	}

	vm, notes, code := run("-config", other, dir)
	if code != exitOK || tarFiles(t, vm.stdins[1])["sbox/egress"] != "other.example:443\n" {
		t.Errorf("-config: exit %d, %s; want the other file's egress", code, notes)
	}
	if !slices.Equal(withoutSummary(vm.calls)[2], []string{"attach", vm.calls[1][1]}) {
		t.Errorf("-config: attach %q, want a shell: the other file has no dev.command", withoutSummary(vm.calls)[2])
	}
	vm, _, code = run("-command", "npm test", dir)
	if code != exitOK || !slices.Equal(withoutSummary(vm.calls)[2], []string{"attach", vm.calls[1][1], "--", "bash", "-lc", "npm test"}) {
		t.Errorf("-command: exit %d, attach %q", code, withoutSummary(vm.calls)[2])
	}
	if _, _, code := run("-command", "x", dir, "--", "y"); code != exitUsage {
		t.Errorf("-command with -- cmd: exit %d, want usage", code)
	}
	if _, notes, code := run("-config", filepath.Join(dir, "missing.yaml"), dir); code != exitUsage || !strings.Contains(notes, "missing.yaml") {
		t.Errorf("-config of a missing file: exit %d, %q; want usage naming it", code, notes)
	}
}

func TestSyncSetupEnvConfigTakeConfigFlag(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	other := filepath.Join(t.TempDir(), "ci.yaml")
	writeTestFile(t, other, "ports: [4000]\n")
	var stdout, stderr bytes.Buffer
	if code := configMain([]string{"-config", other, dir}, &stdout, &stderr, devHost{}); code != exitOK || !strings.Contains(stdout.String(), "4000") {
		t.Errorf("sbox config -config: exit %d:\n%s%s", code, stdout.String(), stderr.String())
	}
	vm := fakeVM{}
	if code := setupEnvMain(context.Background(), []string{"-config", other, dir}, &stdout, &stderr, vm.run, devHost{}); code != exitOK || len(vm.calls) != 0 {
		t.Errorf("sbox setup-env -config (no steps there): exit %d, calls %q", code, vm.calls)
	}
	stderr.Reset()
	if code := syncMain(context.Background(), []string{"-config", filepath.Join(dir, "none.yaml"), dir}, &stdout, &stderr, vm.run, devHost{stateDir: t.TempDir()}); code != exitUsage || !strings.Contains(stderr.String(), "none.yaml") {
		t.Errorf("sbox sync -config of a missing file: exit %d, %q", code, stderr.String())
	}
}

func TestDevMainAppliesLiveChanges(t *testing.T) {
	t.Parallel()
	dir, state, home := t.TempDir(), t.TempDir(), t.TempDir()
	project := mustProjectID(t, dir)
	writeTestFile(t, filepath.Join(home, ".gitconfig"), "[user]\n\tname = new\n")
	writeTestFile(t, filepath.Join(dir, "sbox.yaml"), "egress: [api.anthropic.com]\n")
	old, _ := loadConfig("", filepath.Join(dir, "sbox.yaml"))
	if err := recordBoot(state, project, "0123abcd", "", old); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "sbox.yaml"), "egress: [api.anthropic.com, x.example]\nports: [3000]\npermissions: {deny: [WebFetch]}\nmounts: [{host: ~/.gitconfig, guest: /home/runner/.gitconfig}]\n")
	var refuse bool
	vm := &fakeVM{answer: func(args []string) string {
		if args[0] == "ps" {
			return "0123abcd running 1700000000 " + project + "\n"
		}
		return ""
	}, errFor: func(args []string) error {
		if args[0] == "reconfigure" && refuse {
			return errors.New("sbox-run: raw TCP egress changed")
		}
		return nil
	}}
	dev := func() string {
		var stdout, stderr bytes.Buffer
		if code := devMain(context.Background(), []string{dir, "--", "true"}, strings.NewReader(""), &stdout, &stderr, vm.run, devHost{stateDir: state, home: home, userConfig: allowAll(t)}); code != exitOK {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
		return stderr.String()
	}
	notes := dev()
	cfg, _ := loadConfig("", filepath.Join(dir, "sbox.yaml"))
	var claude, mounts, network []byte
	for i, c := range vm.calls {
		switch {
		case slices.Equal(c, []string{"reconfigure", "0123abcd"}):
			network = vm.stdins[i]
		case slices.Equal(c, []string{"attach", "0123abcd", "--", "python3", "-I", "/drive/sbox/claude.py", "-", "/home/runner"}):
			claude = vm.stdins[i]
		case c[0] == "attach" && len(c) == 6 && strings.Contains(c[5], "rm -rf -- '.gitconfig'"):
			mounts = vm.stdins[i]
		}
	}
	if string(claude) != string(cfg.claudeSettings()) || tarFiles(t, mounts)[".gitconfig"] != "[user]\n\tname = new\n" ||
		string(network) != "egress api.anthropic.com:443\negress x.example:443\nport 3000\n" {
		t.Errorf("live apply: claude settings %q, mounts tar %v, network %q; calls %q", claude, tarFiles(t, mounts), network, vm.calls)
	}
	if !strings.Contains(notes, "applied egress, mounts, permissions, ports to session 0123abcd") || strings.Contains(notes, "config changed") {
		t.Errorf("notes %q: want all of it applied, no restart", notes)
	}
	vm.calls, vm.stdins = nil, nil
	if notes := dev(); strings.Contains(notes, "applied") || strings.Contains(notes, "config changed") {
		t.Errorf("second attach: %q, want nothing applied again, nothing pending", notes)
	}

	// The VM side refusing (raw TCP, say) leaves those keys to -restart.
	writeTestFile(t, filepath.Join(dir, "sbox.yaml"), "egress: [api.anthropic.com, x.example, github.com:22]\nports: [3000]\npermissions: {deny: [WebFetch]}\nmounts: [{host: ~/.gitconfig, guest: /home/runner/.gitconfig}]\n")
	refuse = true
	if notes := dev(); strings.Contains(notes, "applied") || !strings.Contains(notes, "the config changed since session 0123abcd started (egress)") {
		t.Errorf("refused: %q, want egress left to a restart", notes)
	}
}

func TestDevMainTrace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "sbox.yaml"), "dev: {setup: [npm ci]}\n")
	vm := &fakeVM{answer: func(args []string) string {
		switch {
		case args[0] == "start":
			return "READY " + args[1] + "\n"
		case args[0] == "attach" && slices.Contains(args, traceReadBack("terminal")):
			return `T 7 openat(AT_FDCWD, "/home/runner/.netrc", O_RDONLY) = -1 ENOENT (No such file or directory)` + "\n"
		case args[0] == "attach" && slices.Contains(args, traceReadBack("setup")):
			return `T 9 openat(AT_FDCWD, "/home/runner/.aws/credentials", O_RDONLY) = -1 ENOENT (No such file or directory)` + "\n"
		}
		return ""
	}}
	var stdout, stderr bytes.Buffer
	if code := devMain(context.Background(), []string{"-trace", dir, "--", "npm", "install"}, strings.NewReader(""), &stdout, &stderr, vm.run, devHost{stateDir: t.TempDir()}); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	id := vm.calls[1][1]
	if want := append([]string{"attach", id, "--"}, traceCommand([]string{"npm", "install"})...); !slices.Equal(withoutSummary(vm.calls)[3], want) {
		t.Errorf("attach %q, want the command under strace", withoutSummary(vm.calls)[3])
	}
	for _, want := range []string{"INFO    credential-read  reads /home/runner/.netrc (missing)", "INFO    credential-read  reads /home/runner/.aws/credentials (missing) (in dev.setup)"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr.String())
		}
	}
	if hooks := strings.Join(withoutSummary(vm.calls)[2], " "); !strings.Contains(hooks, "strace") {
		t.Errorf("hooks attach %q, want setup under strace", hooks)
	}
}

// TestDevMainMCPCommands: at a fresh boot, a guest MCP server whose
// command the guest lacks is named before the terminal opens.
func TestDevMainMCPCommands(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T, yaml string) (*fakeVM, string) {
		dir := t.TempDir()
		writeTestFile(t, filepath.Join(dir, "sbox.yaml"), yaml)
		vm := &fakeVM{answer: func(args []string) string {
			switch {
			case args[0] == "start":
				return "READY " + args[1] + "\n"
			case args[0] == "attach" && slices.Contains(args, _mcpWhich):
				return "pg-mcp\n"
			}
			return ""
		}}
		var stdout, stderr bytes.Buffer
		if code := devMain(context.Background(), []string{dir, "--", "true"}, strings.NewReader(""), &stdout, &stderr, vm.run, devHost{stateDir: t.TempDir()}); code != exitOK {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
		return vm, stderr.String()
	}
	vm, notes := run(t, "mcp: {servers: {pg: {command: pg-mcp}, fs: {command: node}, docs: {url: \"https://mcp.example.com\"}}}\n")
	i := slices.IndexFunc(vm.calls, func(c []string) bool { return slices.Contains(c, _mcpWhich) })
	if i < 0 || !slices.Equal(vm.calls[i][len(vm.calls[i])-2:], []string{"node", "pg-mcp"}) ||
		!strings.Contains(notes, "BLOCKED mcp     pg  could not start: pg-mcp isn't in the guest → image.packages, or install it into ~/.local") {
		t.Errorf("calls %q, notes:\n%s\nwant the guest asked about node and pg-mcp, and pg named", vm.calls, notes)
	}
	if vm, _ := run(t, "mcp: {servers: {docs: {url: \"https://mcp.example.com\"}}}\n"); slices.ContainsFunc(vm.calls, func(c []string) bool { return slices.Contains(c, _mcpWhich) }) {
		t.Errorf("remote servers only, calls %q: want no guest check", vm.calls)
	}
}

// TestCollectSummaryInParallel: what the exit summary asks the guest for
// goes at once: each answer here waits for all of them to have arrived.
func TestCollectSummaryInParallel(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	arrived := 0
	all := make(chan struct{})
	vm := func(_ context.Context, _ io.Reader, out, _ io.Writer, args ...string) error {
		mu.Lock()
		if arrived++; arrived == 4 {
			close(all)
		}
		mu.Unlock()
		select {
		case <-all:
		case <-time.After(5 * time.Second):
			t.Errorf("%q answered before the others arrived: the calls ran one by one", args)
		}
		if args[0] == "events" {
			_, _ = io.WriteString(out, "EGRESS ALLOW api.anthropic.com:443\n")
		}
		return nil
	}
	count := func() int {
		_ = vm(context.Background(), nil, io.Discard, io.Discard, "attach", "abc", "--", "sums")
		return 2
	}
	summary, unsynced := collectSummary(context.Background(), "abc", "./app", time.Now(), count, config{}, vm, nil)
	if unsynced != 2 || !strings.Contains(summary, "the guest has 2 changes: sbox sync ./app") || !strings.Contains(summary, "api.anthropic.com:443") {
		t.Errorf("unsynced %d, summary %q", unsynced, summary)
	}
}

// TestDevLiveReadOnly: a read-only session asks for a read-only server, the
// guest gets what the project lists on its own disk, and the summary says
// nothing came back.
func TestDevLiveReadOnly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "sbox.yaml"), "dev: {mount: live-ro, local: [build/out, node_modules]}\n")
	var modes []bool
	host := devHost{stateDir: t.TempDir(), userConfig: allowAll(t), startLive: func(project, folder string, readOnly bool) (*liveRelay, error) {
		modes = append(modes, readOnly)
		return &liveRelay{port: 4242, token: []byte("token")}, nil
	}}
	vm := fakeVM{answer: func(args []string) string {
		if args[0] == "start" {
			return "READY " + args[1] + "\n"
		}
		return ""
	}}
	var stdout, stderr bytes.Buffer
	if code := devMain(context.Background(), []string{dir}, strings.NewReader(""), &stdout, &stderr, vm.run, host); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !slices.Equal(modes, []bool{true}) {
		t.Errorf("startLive's readOnly: %v", modes)
	}
	if got := tarFiles(t, vm.stdins[1])["sbox/local"]; got != "build/out\nnode_modules\n" {
		t.Errorf("sbox/local = %q", got)
	}
	for _, p := range []string{"build/out", "node_modules"} {
		if fi, err := os.Lstat(filepath.Join(dir, p)); err != nil || !fi.IsDir() {
			t.Errorf("%s on the Mac: %v, %v", p, fi, err)
		}
	}
	if !strings.Contains(stderr.String(), "live, read-only: nothing came back") {
		t.Errorf("the summary:\n%s", stderr.String())
	}
}

// TestReadOnlyLocal: what a read-only session keeps on the guest's disk. A
// listed path is made on the Mac, but never through a symlink: sbox.yaml is
// the project's, and it mustn't place a folder elsewhere. The defaults leave
// nothing on the Mac.
func TestReadOnlyLocal(t *testing.T) {
	t.Parallel()
	listed := func(p ...string) config {
		var c config
		c.Dev.Local = p
		return c
	}
	t.Run("defaults only where they exist", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, ".venv"), 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := readOnlyLocal(dir, config{})
		if err != nil || !slices.Equal(got, []string{".venv"}) {
			t.Errorf("readOnlyLocal = %q, %v", got, err)
		}
		if _, err := os.Lstat(filepath.Join(dir, "node_modules")); !os.IsNotExist(err) {
			t.Errorf("node_modules on the Mac: %v", err)
		}
	})
	t.Run("a default that is a link or a file isn't bound", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.Symlink(t.TempDir(), filepath.Join(dir, "node_modules")); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(dir, ".venv"), "")
		if got, err := readOnlyLocal(dir, config{}); err != nil || len(got) != 0 {
			t.Errorf("readOnlyLocal = %q, %v", got, err)
		}
	})
	t.Run("listed paths are made, and may exist", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "dist"), 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := readOnlyLocal(dir, listed("dist", "a/b/c"))
		if err != nil || !slices.Equal(got, []string{"dist", "a/b/c"}) {
			t.Errorf("readOnlyLocal = %q, %v", got, err)
		}
		if fi, err := os.Lstat(filepath.Join(dir, "a", "b", "c")); err != nil || !fi.IsDir() {
			t.Errorf("a/b/c: %v, %v", fi, err)
		}
	})
	t.Run("never through a symlink", func(t *testing.T) {
		t.Parallel()
		dir, outside := t.TempDir(), t.TempDir()
		if err := os.Symlink(outside, filepath.Join(dir, "up")); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{"up", "up/planted", "up/planted/deep"} {
			if _, err := readOnlyLocal(dir, listed(p)); err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Errorf("%s: %v, want a refusal", p, err)
			}
		}
		if entries, _ := os.ReadDir(outside); len(entries) != 0 {
			t.Errorf("made outside the project: %v", entries)
		}
	})
	t.Run("not over a file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeTestFile(t, filepath.Join(dir, "dist"), "")
		if _, err := readOnlyLocal(dir, listed("dist")); err == nil {
			t.Error("a file in the way: no error")
		}
	})
}

// TestLiveMountIsNeverCopied: a copied mount's guest path gets rm -rf at boot
// and at attach. Through a read-write link that deletes on the Mac, so links
// stay out of the copies, and a change to one waits for a new session.
func TestLiveMountIsNeverCopied(t *testing.T) {
	t.Parallel()
	dir, home := t.TempDir(), t.TempDir()
	for _, p := range []string{"copied", "linked"} {
		if err := os.Mkdir(filepath.Join(home, p), 0o755); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(home, p, "f"), p+"\n")
	}
	writeTestFile(t, filepath.Join(dir, "sbox.yaml"), `mounts:
  - {host: ~/copied, guest: /home/runner/copied}
  - {host: ~/linked, guest: /home/runner/linked, live: true, mode: rw}
`)
	host := devHost{stateDir: t.TempDir(), userConfig: allowAll(t), home: home}
	cfg, job, err := host.devConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(job.mounts) != 1 || job.mounts[0].guest != "/home/runner/copied" {
		t.Errorf("job.mounts = %+v, want the copy alone", job.mounts)
	}

	vm := fakeVM{answer: func([]string) string { return "" }}
	applied, rest := applyLive(context.Background(), vm.run, "id", cfg, job, []string{"mounts", "mounts.live"}, io.Discard)
	if !slices.Equal(applied, []string{"mounts"}) || !slices.Equal(rest, []string{"mounts.live"}) {
		t.Errorf("applied %q, rest %q", applied, rest)
	}
	for _, c := range vm.calls {
		if strings.Contains(strings.Join(c, " "), "linked") {
			t.Errorf("a call touched the link: %q", c)
		}
	}

	var missing config
	missing.Mounts = []mount{{Host: "~/gone", Guest: "/home/runner/gone", Live: true}}
	if _, err := missing.mountSources(home); err == nil {
		t.Error("a link to a path that isn't there: no error before anything starts")
	}
}

// TestDevLinks: a copy-mode session with links starts a server for each, and the
// guest's drive says what to mount, while the tokens stay outside sbox/, for
// sbox-run to keep off the drive.
func TestDevLinks(t *testing.T) {
	t.Parallel()
	dir, home := t.TempDir(), t.TempDir()
	home, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"shared", "out", "copied"} {
		if err := os.Mkdir(filepath.Join(home, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(dir, "sbox.yaml"), `hide: [.env]
mounts:
  - {host: ~/copied, guest: /home/runner/copied}
  - {host: ~/shared, guest: /home/runner/shared, live: true}
  - {host: ~/out, guest: /home/runner/out dir, live: true, mode: rw}
`)
	var started []liveLink
	state := t.TempDir()
	host := devHost{stateDir: state, userConfig: allowAll(t), home: home, startLink: func(project string, l liveLink) (*liveRelay, error) {
		started = append(started, l)
		return &liveRelay{port: 5000 + len(started), token: []byte("token-" + l.guest)}, nil
	}}
	vm := fakeVM{answer: func(args []string) string {
		if args[0] == "start" {
			return "READY " + args[1] + "\n"
		}
		return ""
	}}
	var stdout, stderr bytes.Buffer
	if code := devMain(context.Background(), []string{dir}, strings.NewReader(""), &stdout, &stderr, vm.run, host); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if len(started) != 2 {
		t.Fatalf("started %+v, want two", started)
	}
	shared, out := started[0], started[1]
	if shared.src != filepath.Join(home, "shared") || !shared.readOnly || shared.guest != "/home/runner/shared" || shared.id != linkID(shared.src, "", true) {
		t.Errorf("the read-only link: %+v", shared)
	}
	if out.src != filepath.Join(home, "out") || out.readOnly || out.guest != "/home/runner/out dir" || out.id != linkID(out.src, "", false) {
		t.Errorf("the read-write link: %+v", out)
	}
	files := tarFiles(t, vm.stdins[1])
	want := shared.id + "\t20490\t5001\tro\tdir\t-\t/home/runner/shared\n" + out.id + "\t20491\t5002\trw\tdir\t-\t/home/runner/out dir\n"
	if files["sbox/links"] != want {
		t.Errorf("sbox/links = %q, want %q", files["sbox/links"], want)
	}
	if files["links/20490.token"] != "token-/home/runner/shared" || files["links/20491.token"] != "token-/home/runner/out dir" {
		t.Errorf("the tokens: %q, %q", files["links/20490.token"], files["links/20491.token"])
	}
	for name, body := range files {
		if strings.HasPrefix(name, "sbox/") && strings.Contains(body, "token-") {
			t.Errorf("a token in %s, on the guest's side of the drive", name)
		}
	}
	if files["sbox/mounts"] != "0 /home/runner/copied\n" {
		t.Errorf("sbox/mounts = %q, want the copy alone", files["sbox/mounts"])
	}
	if got, err := os.ReadFile(liveHideFile(state, mustProjectID(t, dir))); string(got) != ".env\n" {
		t.Errorf("a link's server has no hide list in a copy session: %q, %v", got, err)
	}
	if files["sbox/live"] != "" {
		t.Errorf("a copy session with links says it's live: %q", files["sbox/live"])
	}
}

// TestDevLinkServerFails: a link that can't start stops the session; a guest
// with an empty folder at that path would look like a Mac folder emptied.
func TestDevLinkServerFails(t *testing.T) {
	t.Parallel()
	dir, home := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(home, "shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "sbox.yaml"), "mounts: [{host: ~/shared, guest: /home/runner/shared, live: true}]\n")
	host := devHost{stateDir: t.TempDir(), userConfig: allowAll(t), home: home, startLink: func(string, liveLink) (*liveRelay, error) {
		return nil, errors.New("no port")
	}}
	vm := fakeVM{answer: func([]string) string { return "" }}
	var stdout, stderr bytes.Buffer
	if code := devMain(context.Background(), []string{dir, "--", "true"}, strings.NewReader(""), &stdout, &stderr, vm.run, host); code == exitOK {
		t.Errorf("exit 0\n%s", stderr.String())
	}
	for _, c := range vm.calls {
		if c[0] == "start" {
			t.Errorf("a session started: %q", vm.calls)
		}
	}
}

// TestDevLinksLifecycle: a copy session's summary has its links' refusals, a
// reattach starts their servers again if they stopped, and a change to a link
// waits for a new session.
func TestDevLinksLifecycle(t *testing.T) {
	t.Parallel()
	dir, home, state := t.TempDir(), t.TempDir(), t.TempDir()
	for _, p := range []string{"shared", "other"} {
		if err := os.Mkdir(filepath.Join(home, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	project := mustProjectID(t, dir)
	stale := liveRefusedFile(state, project)
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, stale, "write from-an-earlier-session\n")
	writeTestFile(t, filepath.Join(dir, "sbox.yaml"), "mounts: [{host: ~/shared, guest: /home/runner/shared, live: true}]\n")
	var started []string
	host := devHost{stateDir: state, userConfig: allowAll(t), home: home, startLink: func(project string, l liveLink) (*liveRelay, error) {
		started = append(started, l.guest)
		f, err := os.OpenFile(liveRefusedFile(state, project), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
		if err == nil {
			_, _ = f.WriteString("write " + l.guest + "/a.txt\n") // as the server does when the guest is refused
			_ = f.Close()
		}
		return &liveRelay{port: 4242, token: []byte("token")}, err
	}}
	var session string
	vm := fakeVM{answer: func(args []string) string {
		if args[0] == "start" {
			session = args[1]
			return "READY " + session + "\n"
		}
		return ""
	}}
	var stdout, stderr bytes.Buffer
	if code := devMain(context.Background(), []string{dir}, strings.NewReader(""), &stdout, &stderr, vm.run, host); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	notes := stderr.String()
	for _, want := range []string{"BLOCKED live    /home/runner/shared/a.txt  read-only in a live session (write ×1)", "links    ~/shared → /home/runner/shared (read-only)"} {
		if !strings.Contains(notes, want) {
			t.Errorf("stderr lacks %q:\n%s", want, notes)
		}
	}
	if strings.Contains(notes, "from-an-earlier-session") {
		t.Errorf("an earlier session's refusals are in this one's summary:\n%s", notes)
	}
	if strings.Contains(notes, "mounts   ") {
		t.Errorf("a link is listed as a copy:\n%s", notes)
	}

	// Attached again, the server is started again if it stopped, with no warning.
	running := fakeVM{answer: func(args []string) string {
		if args[0] == "ps" {
			return session + " running 1700000000 " + project + "\n"
		}
		return ""
	}}
	started = nil
	stderr.Reset()
	if code := devMain(context.Background(), []string{dir}, strings.NewReader(""), &stdout, &stderr, running.run, host); code != exitOK || !slices.Equal(started, []string{"/home/runner/shared"}) {
		t.Errorf("reattach: exit %d, startLink calls %q\n%s", code, started, stderr.String())
	}
	if strings.Contains(stderr.String(), "config changed") {
		t.Errorf("reattach warned of a change:\n%s", stderr.String())
	}

	// A new link waits for a new session, as sbox dev says.
	writeTestFile(t, filepath.Join(dir, "sbox.yaml"), "mounts: [{host: ~/shared, guest: /home/runner/shared, live: true}, {host: ~/other, guest: /home/runner/other, live: true}]\n")
	stderr.Reset()
	if code := devMain(context.Background(), []string{dir}, strings.NewReader(""), &stdout, &stderr, running.run, host); code != exitOK {
		t.Errorf("reattach after a change: exit %d\n%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "mounts.live") || !strings.Contains(stderr.String(), "sbox dev -restart") {
		t.Errorf("a changed link isn't called out:\n%s", stderr.String())
	}
	for _, c := range running.calls {
		if strings.Contains(strings.Join(c, " "), "rm -rf") {
			t.Errorf("a changed link was copied in: %q", c)
		}
	}
}

// TestPreflightLinks: copies and links are listed apart, with a link's mode.
func TestPreflightLinks(t *testing.T) {
	t.Parallel()
	var c config
	c.Mounts = []mount{{Host: "~/c", Guest: "/home/runner/c"}, {Host: "~/r", Guest: "/home/runner/r", Live: true}, {Host: "~/w", Guest: "/home/runner/w", Mode: "rw", Live: true}}
	got := strings.Join(devPreflight(c, nil, nil), "\n")
	for _, want := range []string{"mounts   ~/c → /home/runner/c\n", "links    ~/r → /home/runner/r (read-only), ~/w → /home/runner/w (read-write)"} {
		if !strings.Contains(got, want) {
			t.Errorf("preflight lacks %q:\n%s", want, got)
		}
	}
}

// TestDevFileLinks: a link to a file serves its folder, the real one when the path
// is a symlink, and says which name. hide wins over a link, so one to a hidden name
// stops the session rather than show a missing file.
func TestDevFileLinks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, "dotfiles"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(home, "dotfiles", "tool.conf"), "x\n")
	writeTestFile(t, filepath.Join(home, ".env"), "SECRET\n")
	if err := os.Symlink(filepath.Join(home, "dotfiles", "tool.conf"), filepath.Join(home, ".toolrc")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "sbox.yaml"), "mounts: [{host: ~/.toolrc, guest: /home/runner/.toolrc, live: true}]\n")
	host := devHost{stateDir: t.TempDir(), userConfig: allowAll(t), home: home}
	_, job, err := host.devConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(job.links) != 1 {
		t.Fatalf("links = %+v", job.links)
	}
	l := job.links[0]
	if l.src != filepath.Join(home, "dotfiles") || l.file != "tool.conf" || l.guest != "/home/runner/.toolrc" || !l.readOnly || l.id != linkID(l.src, "tool.conf", true) {
		t.Errorf("the link: %+v", l)
	}
	l.port, l.guestPort = 5001, 20490
	if got, want := l.driveLine(), l.id+"\t20490\t5001\tro\tfile\ttool.conf\t/home/runner/.toolrc\n"; got != want {
		t.Errorf("driveLine = %q, want %q", got, want)
	}

	writeTestFile(t, filepath.Join(dir, "sbox.yaml"), "hide: [.env]\nmounts: [{host: ~/.env, guest: /home/runner/.env, live: true}]\n")
	if _, _, err := host.devConfig(dir); err == nil || !strings.Contains(err.Error(), "hide") {
		t.Errorf("a link to a hidden name: %v", err)
	}
	name := "bad\tname"
	writeTestFile(t, filepath.Join(home, name), "x\n")
	writeTestFile(t, filepath.Join(dir, "sbox.yaml"), "mounts: [{host: ~/link-to-bad, guest: /home/runner/b, live: true}]\n")
	if err := os.Symlink(filepath.Join(home, name), filepath.Join(home, "link-to-bad")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := host.devConfig(dir); err == nil || !strings.Contains(err.Error(), "control character") {
		t.Errorf("a file whose real name has a tab: %v", err)
	}
}

// TestDevReadWriteFileLink: a read-write link to a file brings its copy loop onto
// the guest's drive, and only then. Detaching runs one comparison, so a change made
// just before it reaches the Mac now and not at the next boot. A name the server
// refuses anyway is refused up front.
func TestDevReadWriteFileLink(t *testing.T) {
	t.Parallel()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(home, "notes.md"), "x\n")
	dev := func(t *testing.T, mounts string) (map[string]string, [][]string, string, int) {
		t.Helper()
		dir := t.TempDir()
		writeTestFile(t, filepath.Join(dir, "sbox.yaml"), mounts)
		host := devHost{stateDir: t.TempDir(), userConfig: allowAll(t), home: home, startLink: func(string, liveLink) (*liveRelay, error) {
			return &liveRelay{port: 5001, token: []byte("token")}, nil
		}}
		vm := fakeVM{answer: func(args []string) string {
			if args[0] == "start" {
				return "READY " + args[1] + "\n"
			}
			return ""
		}}
		var stdout, stderr bytes.Buffer
		code := devMain(context.Background(), []string{dir}, strings.NewReader(""), &stdout, &stderr, vm.run, host)
		if len(vm.stdins) < 2 {
			return nil, vm.calls, stderr.String(), code
		}
		return tarFiles(t, vm.stdins[1]), vm.calls, stderr.String(), code
	}
	once := func(calls [][]string) bool {
		return slices.ContainsFunc(calls, func(c []string) bool { return strings.Contains(strings.Join(c, " "), "links.py --once") })
	}

	files, calls, notes, code := dev(t, "mounts: [{host: ~/notes.md, guest: /home/runner/notes.md, live: true, mode: rw}]\n")
	if code != exitOK {
		t.Fatalf("exit %d\n%s", code, notes)
	}
	if files["sbox/links.py"] != _linksScript {
		t.Error("the copy loop isn't on the drive")
	}
	if !strings.Contains(files["sbox/links"], "\trw\tfile\tnotes.md\t/home/runner/notes.md\n") {
		t.Errorf("sbox/links = %q", files["sbox/links"])
	}
	if !once(calls) {
		t.Errorf("no comparison at detach: %q", calls)
	}

	files, calls, notes, code = dev(t, "mounts: [{host: ~/notes.md, guest: /home/runner/notes.md, live: true}]\n")
	if code != exitOK {
		t.Fatalf("exit %d\n%s", code, notes)
	}
	if _, ok := files["sbox/links.py"]; ok || once(calls) {
		t.Errorf("a read-only link: links.py on the drive %v, comparison at detach %v", ok, once(calls))
	}

	for _, name := range []string{".envrc", "HEAD", "head", ".VSCODE"} {
		writeTestFile(t, filepath.Join(home, name), "x\n")
		if _, _, notes, code := dev(t, "mounts: [{host: ~/"+name+", guest: /home/runner/f, live: true, mode: rw}]\n"); code == exitOK || !strings.Contains(notes, "refuses") {
			t.Errorf("a read-write link to %s: exit %d\n%s", name, code, notes)
		}
		_ = os.Remove(filepath.Join(home, name))
	}
}
