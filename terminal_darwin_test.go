package main

import (
	"bytes"
	"os"
	"syscall"
	"testing"
	"unsafe"
)

// openTerminal opens a pseudo-terminal's slave side, what a shell's stdin
// is, or returns nil when the system has none to give.
func openTerminal(t *testing.T) *os.File {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil
	}
	t.Cleanup(func() { _ = master.Close() })
	var name [128]byte
	for _, req := range []struct {
		op  uintptr
		arg uintptr
	}{{syscall.TIOCPTYGRANT, 0}, {syscall.TIOCPTYUNLK, 0}, {syscall.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name))}} {
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), req.op, req.arg); errno != 0 {
			t.Fatalf("ioctl %#x: %v", req.op, errno)
		}
	}
	slave, err := os.OpenFile(string(name[:bytes.IndexByte(name[:], 0)]), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	return slave
}
