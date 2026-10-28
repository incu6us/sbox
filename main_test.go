package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunDispatchesSessionCommands(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	run(nil, &stdout, &stderr)
	for _, cmd := range []string{"dev", "start", "attach", "ps", "stop", "pause", "resume", "log", "clean", "sync", "setup-env", "init", "config", "watch"} {
		if !strings.Contains(stderr.String(), "\n  "+cmd+" ") {
			t.Errorf("usage lacks %s:\n%s", cmd, stderr.String())
		}
	}

	// Arguments that fail validation reach each command without the VM.
	tests := []struct {
		args    []string
		wantErr string
	}{
		{[]string{"start"}, "usage: sbox start"},
		{[]string{"ps", "extra"}, "usage: sbox ps"},
		{[]string{"stop"}, "usage: sbox stop"},
		{[]string{"pause"}, "usage: sbox pause"},
		{[]string{"resume"}, "usage: sbox resume"},
		{[]string{"log"}, "usage: sbox log"},
		{[]string{"attach"}, "usage: sbox attach"},
		{[]string{"dev"}, "usage: sbox dev"},
		{[]string{"clean", "extra"}, "usage: sbox clean"},
		{[]string{"sync"}, "usage: sbox sync"},
		{[]string{"setup-env"}, "usage: sbox setup-env"},
		{[]string{"init", "a", "b"}, "usage: sbox init"},
		{[]string{"config", "a", "b"}, "usage: sbox config"},
		{[]string{"watch", "a", "b"}, "usage: sbox watch"},
		{[]string{"log", "-x", "a"}, "usage: sbox log"},
	}
	for _, tt := range tests {
		var stdout, stderr bytes.Buffer
		if code := run(tt.args, &stdout, &stderr); code != exitUsage || !strings.Contains(stderr.String(), tt.wantErr) {
			t.Errorf("sbox %v: exit %d, stderr %q; want exit %d with %q", tt.args, code, stderr.String(), exitUsage, tt.wantErr)
		}
	}
}
