package main

import (
	"bufio"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// echoServer accepts connections on 127.0.0.1 and echoes each line back.
func echoServer(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				line, _ := bufio.NewReader(c).ReadString('\n')
				_, _ = c.Write([]byte(line))
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// proxyHelper starts scripts/sbox-proxy.py with args until the test ends.
func proxyHelper(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("python3", append([]string{"scripts/sbox-proxy.py"}, args...)...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

// roundTrip dials until the helper is up, sends a line and returns the answer.
func roundTrip(t *testing.T, network, addr string) string {
	t.Helper()
	var c net.Conn
	var err error
	for i := 0; i < 50; i++ {
		if c, err = net.Dial(network, addr); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(c).ReadString('\n')
	return line
}

func TestProxyExposeAndRelay(t *testing.T) {
	t.Parallel()
	dir, err := os.MkdirTemp("", "sx") // unix socket paths are short on macOS
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	port := echoServer(t)

	// expose: a unix socket whose clients reach a TCP port.
	proxyHelper(t, "expose", sock, "127.0.0.1", strconv.Itoa(port))
	if got := roundTrip(t, "unix", sock); got != "ping\n" {
		t.Errorf("through expose: %q, want the echo", got)
	}
	// relay then expose: TCP to the socket to TCP, as a forwarded port is.
	front := freePort(t)
	proxyHelper(t, "relay", "127.0.0.1", strconv.Itoa(front), sock)
	if got := roundTrip(t, "tcp", "127.0.0.1:"+strconv.Itoa(front)); got != "ping\n" {
		t.Errorf("through relay and expose: %q, want the echo", got)
	}
}

func TestPythonSelftests(t *testing.T) {
	t.Parallel()
	for _, script := range []string{"scripts/sbox-proxy.py", "scripts/sbox-dns.py"} {
		out, err := exec.Command("python3", script, "--selftest").CombinedOutput()
		if err != nil || string(out) != "ok\n" {
			t.Errorf("%s --selftest: %v\n%s", script, err, out)
		}
	}
}
