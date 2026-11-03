//go:build darwin

package main

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
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
	a, err := liveState(state, "0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	b, err := liveState(state, "0123456789abcdef")
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
	if c, err := liveState(state, "fedcba9876543210"); err != nil || bytes.Equal(c.token, a.token) {
		t.Errorf("another project: %v, same token %v", err, bytes.Equal(c.token, a.token))
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
