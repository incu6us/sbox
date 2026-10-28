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
		return c[0] == "events" || (c[0] == "attach" && slices.Equal(c[2:], []string{"--", "ss", "-Hltn"}))
	})
}

func (f *fakeVM) run(_ context.Context, in io.Reader, out, errOut io.Writer, args ...string) error {
	f.calls = append(f.calls, args)
	var data []byte
	if in != nil {
		var err error
		if data, err = io.ReadAll(in); err != nil {
			return err
		}
		f.stdin = data
	}
	f.stdins = append(f.stdins, data)
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
}

func (g *fakeGuest) wait(d time.Duration) {
	if !g.paused || g.ignorePause {
		g.ticks += int(d / time.Second)
	}
}

func (g *fakeGuest) run(_ context.Context, in io.Reader, out, _ io.Writer, args ...string) error {
	if in != nil {
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
	good := &fakeGuest{}
	if bad := failing(sessionProbe(context.Background(), good.run, good.wait)); len(bad) != 0 || !good.stopped {
		t.Errorf("a well-behaved session: failing %q, stopped %v; want none failing and the session stopped", bad, good.stopped)
	}
	late := &fakeGuest{late: true}
	if bad := failing(sessionProbe(context.Background(), late.run, late.wait)); len(bad) != 0 {
		t.Errorf("a guest whose first tick comes after the first read: failing %q, want none", bad)
	}
	silent := &fakeGuest{silent: true}
	if bad := failing(sessionProbe(context.Background(), silent.run, silent.wait)); len(bad) != 3 {
		t.Errorf("a session with no counter output: failing %q, want the three counter checks", bad)
	}
	broken := &fakeGuest{ignorePause: true}
	if bad := failing(sessionProbe(context.Background(), broken.run, broken.wait)); !slices.Equal(bad, []string{"session: pause freezes the guest"}) || !broken.stopped {
		t.Errorf("a session that ignores pause: failing %q, stopped %v; want only the pause check failing", bad, broken.stopped)
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
	vm := fakeVM{}
	var stdout, stderr bytes.Buffer
	if code := sessionMain(context.Background(), "clean", nil, &stdout, &stderr, vm.run); code != exitOK ||
		!slices.EqualFunc(vm.calls, [][]string{{"clean"}}, slices.Equal) || !strings.Contains(stdout.String(), "removed") {
		t.Errorf("sbox clean: exit %d, calls %q, stdout %q", code, vm.calls, stdout.String())
	}
	if code := sessionMain(context.Background(), "clean", []string{"x"}, &stdout, &stderr, vm.run); code != exitUsage {
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
	want, _ := readManifest(dir)
	if err != nil || !maps.Equal(base, want) {
		t.Errorf("base after a new drive: %v, %v; want the folder's manifest %v", base, err, want)
	}

	kept := t.TempDir()
	run(kept, answer(""))
	if _, err := loadBase(kept, project); err == nil {
		t.Error("a kept drive wrote a base it can't know")
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
	keep := func(string) bool { return true }
	stop := func(string) bool { return false }

	tests := []struct {
		name  string
		args  []string
		ps    string
		keep  func(string) bool
		want  [][]string // "NEW" stands for the new session's id
		notes string     // on stderr
	}{
		{name: "running", args: []string{dir}, ps: live + " running " + started + " " + project + "\n",
			want: [][]string{{"ps"}, {"attach", live}, {"stop", live}}, notes: "attached to session " + live + ", running 2h0m"},
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
	for answer, want := range map[string]bool{"\n": true, "y\n": true, "n\n": false, "No\n": false, "": true} {
		var out bytes.Buffer
		if got := askKeep(strings.NewReader(answer), &out)("0123abcd"); got != want || !strings.Contains(out.String(), "keep session 0123abcd running? [Y/n]") {
			t.Errorf("answer %q: keep %v, asked %q; want %v", answer, got, out.String(), want)
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
	script := hookScript([]string{"npm ci"}, []string{"npm run dev &"}, 10*time.Minute, false)
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
	if env := tarFiles(t, vm.stdins[1])["sbox/env"]; env != "A=1 2\nNODE_ENV=development\n" {
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
	script := hookScript([]string{"npm ci"}, nil, 10*time.Minute, true)
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
		none.calls[1][0] != "start" || none.calls[3][0] != "stop" || tarFiles(t, none.stdins[1])["sbox/env"] != "A=b\n" {
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
	if code := devMain(context.Background(), []string{dir}, strings.NewReader(""), &stdout, &stderr, vm.run, devHost{stateDir: t.TempDir(), lookupEnv: lookup}); code != exitOK {
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
	if code := devMain(context.Background(), []string{dir}, strings.NewReader(""), &stdout, &stderr, none.run, devHost{stateDir: t.TempDir(), lookupEnv: lookup}); code != exitUsage ||
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
		}
		return ""
	}}
	var stdout, stderr bytes.Buffer
	if code := devMain(context.Background(), []string{dir, "--", "claude"}, strings.NewReader(""), &stdout, &stderr, vm.run, devHost{stateDir: t.TempDir()}); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	id := vm.calls[1][1]
	want := [][]string{{"ps"}, {"start", id, mustProjectID(t, dir)}, {"attach", id, "--", "claude"}, {"events", id}, {"attach", id, "--", "ss", "-Hltn"}, {"stop", id}}
	if !slices.EqualFunc(vm.calls, want, slices.Equal) {
		t.Errorf("calls %q, want %q", vm.calls, want)
	}
	out := stderr.String()
	for _, want := range []string{"egress   api.anthropic.com:443", "ports    3000", "session 0s", "BLOCKED egress  telemetry.example:443", "BLOCKED port    5173"} {
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
	if code := devMain(context.Background(), []string{dir}, strings.NewReader(""), &stdout, &stderr, vm.run, devHost{stateDir: t.TempDir(), home: home}); code != exitOK {
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
	if code := devMain(context.Background(), []string{dir}, strings.NewReader(""), &stdout, &stderr, none.run, devHost{stateDir: t.TempDir(), home: home}); code != exitUsage ||
		len(none.calls) != 0 || !strings.Contains(stderr.String(), "~/.gitconfig") {
		t.Errorf("missing mount: exit %d, calls %q, stderr %q; want usage before any VM call", code, none.calls, stderr.String())
	}
}
