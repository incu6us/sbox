//go:build darwin

package main

import (
	"cmp"
	"context"
	"crypto/subtle"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The Mac side of a live mount (docs/live-mount.md): sbox live-serve runs
// the NFS server for one project, on the Mac's loopback, in the background
// until the project has no session. Its key, token and port are kept in
// the state folder, so a server started again answers the same handles
// and the same relay.

type liveFiles struct {
	key, token []byte
	port       int
}

// liveFolder is where a project's server keeps its state. A read-only server
// has its own, port and token included, so one left running from a read-write
// session is never the one a read-only session's relay reaches.
func liveFolder(stateDir, project string, readOnly bool) string {
	if readOnly {
		return filepath.Join(stateDir, "live", project, "ro")
	}
	return filepath.Join(stateDir, "live", project)
}

// liveState reads the key, token and port in a server's folder, making each
// the first time.
func liveState(dir string) (liveFiles, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return liveFiles{}, err
	}
	var st liveFiles
	var err error
	if st.key, err = readOrMake(filepath.Join(dir, "key"), func() ([]byte, error) { return randomBytes(32) }); err != nil {
		return liveFiles{}, err
	}
	if st.token, err = readOrMake(filepath.Join(dir, "token"), func() ([]byte, error) { return randomBytes(32) }); err != nil {
		return liveFiles{}, err
	}
	port, err := readOrMake(filepath.Join(dir, "port"), func() ([]byte, error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		defer func() { _ = ln.Close() }()
		return []byte(strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)), nil
	})
	if err != nil {
		return liveFiles{}, err
	}
	if st.port, err = strconv.Atoi(string(port)); err != nil {
		return liveFiles{}, fmt.Errorf("%s: %w", filepath.Join(dir, "port"), err)
	}
	return st, nil
}

// liveAccept serves srv on each connection from ln that starts with token,
// closing the rest unanswered, until ln is closed.
// liveRoot is dir as the file system names it: APFS takes ~/WORK for ~/work,
// and handles resolve to its own spelling.
func liveRoot(dir string) (string, error) {
	root, err := filepath.Abs(dir)
	if err == nil {
		root, err = filepath.EvalSymlinks(root)
	}
	if err != nil {
		return "", err
	}
	f, _, err := lstatHandle(root)
	if err != nil {
		return "", err
	}
	return fsgetpath(f.dev, f.ino)
}

func liveAccept(ln net.Listener, srv *nfsServer, token []byte) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer func() { _ = c.Close() }()
			_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
			got := make([]byte, len(token))
			if _, err := io.ReadFull(c, got); err != nil || subtle.ConstantTimeCompare(got, token) != 1 {
				return
			}
			_ = c.SetReadDeadline(time.Time{})
			_ = srv.serveConn(c, c)
		}()
	}
}

// startLiveServer finds project's server running, or starts sbox
// live-serve in the background, and returns where the relay goes.
func startLiveServer(stateDir, project, dir string, readOnly bool) (*liveRelay, error) {
	return startServer(liveFolder(stateDir, project, readOnly), liveServeArgs(project, dir, readOnly))
}

// startLinkServer does so for one of project's links.
func startLinkServer(stateDir, project string, l liveLink) (*liveRelay, error) {
	return startServer(l.spec(project, stateDir).folder, l.serveArgs(project))
}

// startServer finds the server whose state is in sdir running, or starts it
// with args.
func startServer(sdir string, args []string) (*liveRelay, error) {
	st, err := liveState(sdir)
	if err != nil {
		return nil, err
	}
	relay := &liveRelay{port: st.port, token: st.token}
	if pid, err := os.ReadFile(filepath.Join(sdir, "pid")); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(pid))); err == nil && syscall.Kill(n, 0) == nil && answers(st.port) {
			return relay, nil
		}
	}

	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	logFile, err := os.OpenFile(filepath.Join(sdir, "log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() { _ = logFile.Close() }()
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // outlives the terminal, like the session
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	for range 50 {
		select {
		case <-exited:
			return nil, fmt.Errorf("sbox live-serve exited (%s)", filepath.Join(sdir, "log"))
		case <-time.After(100 * time.Millisecond):
		}
		if answers(st.port) {
			return relay, nil
		}
	}
	return nil, fmt.Errorf("sbox live-serve didn't start listening (%s)", filepath.Join(sdir, "log"))
}

// liveServeArgs is the command line of project's server.
func liveServeArgs(project, dir string, readOnly bool) []string {
	if readOnly {
		return []string{"live-serve", "-ro", project, dir}
	}
	return []string{"live-serve", project, dir}
}

// serveArgs is the command line of the link's server. ponytail: a process per
// link, so a ps into the VM per link every 30 s; one process with many listeners
// if that shows up in limactl's load.
func (l liveLink) serveArgs(project string) []string {
	args := []string{"live-serve", "-link", l.id, "-guest", l.guest}
	if l.readOnly {
		args = append(args, "-ro")
	}
	return append(args, project, l.src)
}

// spec is what the link's server serves and where it keeps its state.
func (l liveLink) spec(project, stateDir string) liveSpec {
	return liveSpec{folder: filepath.Join(stateDir, "live", project, "links", l.id), dir: l.src, guest: l.guest, readOnly: l.readOnly}
}

func answers(port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// refusedLine is one refusal in a project's log; the root's path is ".".
func refusedLine(op, rel string) string { return op + " " + cmp.Or(rel, ".") + "\n" }

// liveSpec is one server of project's: its state folder and what it serves.
// guest is a link's path in the guest, which its refusals are logged under.
type liveSpec struct {
	folder, dir, guest string
	readOnly           bool
}

// liveServerFor makes the server spec asks for, with its state, as project's
// lists and log say.
func liveServerFor(stateDir, project string, spec liveSpec) (*nfsServer, liveFiles, error) {
	root, err := liveRoot(spec.dir)
	if err != nil {
		return nil, liveFiles{}, err
	}
	st, err := liveState(spec.folder)
	if err != nil {
		return nil, liveFiles{}, err
	}
	srv := newNFSServer(root, st.key)
	srv.readOnly = spec.readOnly
	srv.protectFile = liveProtectFile(stateDir, project)
	srv.hideFile = liveHideFile(stateDir, project)
	refused := liveRefusedFile(stateDir, project)
	srv.refused = func(op, rel string) {
		if spec.guest != "" {
			rel = path.Join(spec.guest, rel)
		}
		if f, err := os.OpenFile(refused, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600); err == nil {
			_, _ = f.WriteString(refusedLine(op, rel))
			_ = f.Close()
		}
	}
	return srv, st, nil
}

// serveLive starts dir's server on project's port in this process and
// returns its listener; closing it stops the server.
func serveLive(stateDir, project, dir string, readOnly bool) (net.Listener, error) {
	return serveSpec(stateDir, project, liveSpec{folder: liveFolder(stateDir, project, readOnly), dir: dir, readOnly: readOnly})
}

// serveSpec starts spec's server in this process and returns its listener.
func serveSpec(stateDir, project string, spec liveSpec) (net.Listener, error) {
	srv, st, err := liveServerFor(stateDir, project, spec)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", st.port))
	if err != nil {
		return nil, err
	}
	go func() { _ = liveAccept(ln, srv, st.token) }()
	return ln, nil
}

// liveServeMain implements the hidden `sbox live-serve [-ro] <project> <dir>`
// and, for a link, `sbox live-serve -link <id> -guest <path> [-ro] <project>
// <dir>`: the NFS server for dir, until project has had no session for two
// checks in a row.
func liveServeMain(ctx context.Context, args []string, stderr io.Writer, stateDir string, vm sboxRunFunc) int {
	flags := flag.NewFlagSet("sbox live-serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	readOnly := flags.Bool("ro", false, "refuse every change")
	link := flags.String("link", "", "the link's `id`")
	guest := flags.String("guest", "", "the link's `path` in the guest")
	if flags.Parse(args) != nil || flags.NArg() != 2 || (*link == "") != (*guest == "") {
		_, _ = fmt.Fprintln(stderr, "usage: sbox live-serve [-link <id> -guest <path>] [-ro] <project> <dir>")
		return exitUsage
	}
	args = flags.Args()
	spec := liveSpec{folder: liveFolder(stateDir, args[0], *readOnly), dir: args[1], readOnly: *readOnly}
	if *link != "" {
		if *link != linkID(args[1], "", *readOnly) {
			_, _ = fmt.Fprintf(stderr, "sbox live-serve: %s is not link %s\n", args[1], *link)
			return exitUsage
		}
		spec = liveLink{id: *link, src: args[1], guest: *guest, readOnly: *readOnly}.spec(args[0], stateDir)
	} else if id, err := projectID(args[1]); err != nil || id != args[0] {
		_, _ = fmt.Fprintf(stderr, "sbox live-serve: %s is not project %s's folder\n", args[1], args[0])
		return exitUsage
	}
	ln, err := serveSpec(stateDir, args[0], spec)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox live-serve:", err)
		return exitFail
	}
	defer func() { _ = ln.Close() }()
	pidFile := filepath.Join(spec.folder, "pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox live-serve:", err)
		return exitFail
	}
	defer func() { _ = os.Remove(pidFile) }()
	_, _ = fmt.Fprintf(stderr, "sbox live-serve: %s on %s\n", args[1], ln.Addr())

	misses := 0
	for {
		select {
		case <-ctx.Done():
			return exitOK
		case <-time.After(30 * time.Second):
		}
		var out strings.Builder
		if vm(ctx, nil, &out, io.Discard, "ps") != nil {
			continue // the VM unreachable says nothing about the session
		}
		misses++
		for _, s := range parseSessions(out.String()) {
			if s.Project == args[0] && s.State != "exited" {
				misses = 0
			}
		}
		if misses >= 2 {
			_, _ = fmt.Fprintln(stderr, "sbox live-serve: no session on the project; stopping")
			return exitOK
		}
	}
}
