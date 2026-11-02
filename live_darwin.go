//go:build darwin

package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
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

// liveState reads project's key, token and port, making each the first
// time.
func liveState(stateDir, project string) (liveFiles, error) {
	dir := filepath.Join(stateDir, "live", project)
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

func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := rand.Read(b)
	return b, err
}

// readOrMake returns p's content, writing make's (0600) when p is missing.
func readOrMake(p string, make func() ([]byte, error)) ([]byte, error) {
	b, err := os.ReadFile(p)
	if !errors.Is(err, fs.ErrNotExist) {
		return b, err
	}
	if b, err = make(); err != nil {
		return nil, err
	}
	return b, os.WriteFile(p, b, 0o600)
}

// liveAccept serves srv on each connection from ln that starts with token,
// closing the rest unanswered, until ln is closed.
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
func startLiveServer(stateDir, project, dir string) (*liveRelay, error) {
	st, err := liveState(stateDir, project)
	if err != nil {
		return nil, err
	}
	relay := &liveRelay{port: st.port, token: st.token}
	sdir := filepath.Join(stateDir, "live", project)
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
	cmd := exec.Command(exe, "live-serve", project, dir)
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

func answers(port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// serveLive starts dir's server on project's port in this process and
// returns its listener; closing it stops the server.
func serveLive(stateDir, project, dir string) (net.Listener, error) {
	root, err := filepath.Abs(dir)
	if err == nil {
		root, err = filepath.EvalSymlinks(root)
	}
	if err != nil {
		return nil, err
	}
	st, err := liveState(stateDir, project)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", st.port))
	if err != nil {
		return nil, err
	}
	go func() { _ = liveAccept(ln, newNFSServer(root, st.key), st.token) }()
	return ln, nil
}

// liveServeMain implements the hidden `sbox live-serve <project> <dir>`:
// the NFS server for dir, until project has had no session for two
// checks in a row.
func liveServeMain(ctx context.Context, args []string, stderr io.Writer, stateDir string, vm sboxRunFunc) int {
	if len(args) != 2 {
		_, _ = fmt.Fprintln(stderr, "usage: sbox live-serve <project> <dir>")
		return exitUsage
	}
	if id, err := projectID(args[1]); err != nil || id != args[0] {
		_, _ = fmt.Fprintf(stderr, "sbox live-serve: %s is not project %s's folder\n", args[1], args[0])
		return exitUsage
	}
	ln, err := serveLive(stateDir, args[0], args[1])
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox live-serve:", err)
		return exitFail
	}
	defer func() { _ = ln.Close() }()
	pidFile := filepath.Join(stateDir, "live", args[0], "pid")
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
