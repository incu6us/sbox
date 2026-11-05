//go:build darwin

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// TestLiveAcceptChecksTheToken: a connection speaks NFS only after the
// session's token; anything else is closed unanswered.
func TestLiveAcceptChecksTheToken(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	token := bytes.Repeat([]byte("k"), 32)
	srv := newNFSServer(testFolder(t), bytes.Repeat([]byte{1}, 32))
	go func() { _ = liveAccept(ln, srv, token) }()

	var null xdrWriter
	for _, v := range []uint32{7, 0, 2, _nfsProgram, 4, 0, 0, 0, 0, 0} {
		null.u32(v)
	}
	record := append(binary.BigEndian.AppendUint32(nil, 0x80000000|uint32(len(null.b))), null.b...)
	try := func(prefix []byte) []byte {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		_, _ = c.Write(append(prefix, record...))
		got, _ := io.ReadAll(io.LimitReader(c, 8))
		return got
	}
	if got := try(token); len(got) != 8 || binary.BigEndian.Uint32(got[4:]) != 7 {
		t.Errorf("with the token: %x, want the NULL reply", got)
	}
	wrong := bytes.Repeat([]byte("x"), 32)
	if got := try(wrong); len(got) != 0 {
		t.Errorf("with a wrong token: %x, want the connection closed", got)
	}
}

// TestLiveStateIsKept: the key, token and port are made once per project
// and kept, so a server started again answers the same handles and relay.
func TestLiveStateIsKept(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	a, err := liveState(liveFolder(state, "0123456789abcdef", false))
	if err != nil {
		t.Fatal(err)
	}
	b, err := liveState(liveFolder(state, "0123456789abcdef", false))
	if err != nil {
		t.Fatal(err)
	}
	if a.port != b.port || !bytes.Equal(a.token, b.token) || !bytes.Equal(a.key, b.key) || len(a.token) != 32 || len(a.key) != 32 {
		t.Errorf("first %+v, second %+v; want the same port, token and key, 32 bytes each", a, b)
	}
	if a.port < 1024 || a.port > 65535 {
		t.Errorf("port %d", a.port)
	}
	for _, f := range []string{"key", "token"} {
		if fi, err := os.Stat(filepath.Join(state, "live", "0123456789abcdef", f)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v, mode %v; want 0600", f, err, fi.Mode())
		}
	}
	if c, err := liveState(liveFolder(state, "fedcba9876543210", false)); err != nil || bytes.Equal(c.token, a.token) {
		t.Errorf("another project: %v, same token %v", err, bytes.Equal(c.token, a.token))
	}
}

// TestLiveStateByMode: a read-only server has its own key, token and port, so
// one left running from a read-write session is never the one it reaches.
func TestLiveStateByMode(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	rw, err := liveState(liveFolder(state, "0123456789abcdef", false))
	if err != nil {
		t.Fatal(err)
	}
	ro, err := liveState(liveFolder(state, "0123456789abcdef", true))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(rw.token, ro.token) || bytes.Equal(rw.key, ro.key) || rw.port == ro.port {
		t.Errorf("read-write and read-only share state: %+v, %+v", rw, ro)
	}
}

// TestServeLiveReadOnly: the running server refuses a change when read-only,
// and a refusal at the root is logged so that a BLOCKED line can name it.
func TestServeLiveReadOnly(t *testing.T) {
	t.Parallel()
	state, dir := t.TempDir(), testFolder(t)
	ln, err := serveLive(state, "0123456789abcdef", dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	if got := liveServeArgs("0123456789abcdef", dir, true); !slices.Equal(got, []string{"live-serve", "-ro", "0123456789abcdef", dir}) {
		t.Errorf("read-only args: %q", got)
	}
	if got := liveServeArgs("0123456789abcdef", dir, false); !slices.Equal(got, []string{"live-serve", "0123456789abcdef", dir}) {
		t.Errorf("read-write args: %q", got)
	}
	if got := refusedLine("setattr", ""); got != "setattr .\n" {
		t.Errorf("a refusal at the root: %q", got)
	}
	file := liveRefusedFile(state, "0123456789abcdef")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, file, refusedLine("setattr", "")+refusedLine("write", "README.md"))
	want := []string{"BLOCKED live    .  read-only in a live session (setattr ×1)", "BLOCKED live    README.md  read-only in a live session (write ×1)"}
	if got := liveRefusals(state, "0123456789abcdef"); !slices.Equal(got, want) {
		t.Errorf("liveRefusals = %q, want %q", got, want)
	}
}

// TestLiveServeMainUsage: the flag doesn't take the place of the arguments.
func TestLiveServeMainUsage(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{}, {"-ro"}, {"-ro", "project"}, {"-nope", "project", "dir"}} {
		if code := liveServeMain(context.Background(), args, io.Discard, t.TempDir(), nil); code != exitUsage {
			t.Errorf("%q: exit %d, want usage", args, code)
		}
	}
}

// TestServeLiveFolderInAnotherCase: APFS takes ~/WORK for ~/work, and every
// handle must still resolve inside the folder.
func TestServeLiveFolderInAnotherCase(t *testing.T) {
	t.Parallel()
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(real, "Project")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "README.md"), "hello\n")
	root, err := liveRoot(filepath.Join(real, "PROJECT"))
	if err != nil {
		t.Fatal(err)
	}
	if root != dir {
		t.Fatalf("liveRoot: %q, want %q", root, dir)
	}
	s := newNFSServer(root, bytes.Repeat([]byte{1}, 32))
	if status := fhStatus(t, s, lookupHandle(t, s, "README.md"), func(*compoundArgs) {}); status != _nfsOK {
		t.Errorf("a handle under the folder: %d", status)
	}
}
