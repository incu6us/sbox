package main

import (
	"os"
	"testing"
)

// openTerminal: not wired up on Linux, where sbox's host side doesn't run.
func openTerminal(*testing.T) *os.File { return nil }
