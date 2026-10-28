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
	_, slave := openPTY(t)
	return slave
}

// openPTY opens a pseudo-terminal pair: the master, which a terminal
// emulator holds, and the slave, a program's terminal. Both nil when the
// system has none to give.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, nil
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
	slave, err = os.OpenFile(string(name[:bytes.IndexByte(name[:], 0)]), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	return master, slave
}

// setWinsize sets the terminal's size, as a terminal emulator does when its
// window changes; the kernel signals the terminal's foreground programs.
func setWinsize(t *testing.T, f *os.File, rows, cols uint16) {
	t.Helper()
	ws := [4]uint16{rows, cols, 0, 0}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws))); errno != 0 {
		t.Fatal(errno)
	}
}
