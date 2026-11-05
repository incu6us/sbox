//go:build !darwin

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// Live mounts are served by macOS's fsgetpath(2); sbox runs on macOS.
var errLiveMacOnly = errors.New("live mounts need macOS")

func startLiveServer(string, string, string, bool) (*liveRelay, error) { return nil, errLiveMacOnly }

func liveServeMain(_ context.Context, _ []string, stderr io.Writer, _ string, _ sboxRunFunc) int {
	_, _ = fmt.Fprintln(stderr, "sbox live-serve:", errLiveMacOnly)
	return exitFail
}
