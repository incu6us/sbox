package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// hookScript is the sh script that runs a project's dev.setup and
// dev.start steps in the guest, as the runner, from the project folder,
// each under timeout. Setup runs when force is set or when the list's
// hash differs from the one its last successful run stamped on the home
// (the project's kept drive). A failed step stops the script, before any
// start step and without the stamp. A start step ending in & runs
// detached, its output appended to ~/.sbox/start.log; one still running
// after 30 s gets a line saying the & may be missing. With trace, setup
// steps run under strace into /tmp/sbox-trace/setup (start steps don't: a
// traced server would hold its step open).
func hookScript(setup, start []string, timeout time.Duration, force, trace bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, `cd "$HOME/work" || exit 1
mkdir -p "$HOME/.sbox"
step() {
	echo "sbox: $1: $2" >&2
	w=
	[ "$1" = start ] && { (sleep 30 </dev/null >/dev/null 2>&1; echo "sbox: start: still running after 30s: $2 (end a server's line with & to run it in the background)" >&2) & w=$!; }
	timeout %d $tracer sh -c "$2" </dev/null
	s=$?
	[ -n "$w" ] && kill $w 2>/dev/null
	[ $s -eq 0 ] && return
	[ $s -eq 124 ] && echo "sbox: $1 timed out after %v: $2" >&2 || echo "sbox: $1 failed (exit $s): $2" >&2
	exit $s
}
`, int(timeout.Seconds()), timeout)
	if len(setup) > 0 {
		sum := sha256.Sum256([]byte(strings.Join(setup, "\n")))
		stamp := hex.EncodeToString(sum[:8])
		cond := fmt.Sprintf(`[ "$(cat "$HOME/.sbox/setup.sha" 2>/dev/null)" != %s ]`, stamp)
		if force {
			cond = "true"
		}
		fmt.Fprintf(&b, "if %s; then\n\trm -f \"$HOME/.sbox/setup.sha\"\n", cond)
		if trace {
			fmt.Fprintf(&b, "\trm -rf /tmp/sbox-trace/setup && mkdir -p /tmp/sbox-trace/setup\n\ttracer='strace -ff -qq -s 200 -e trace=%s -o /tmp/sbox-trace/setup/t --'\n", _traced)
		}
		for _, s := range setup {
			fmt.Fprintf(&b, "\tstep setup %s\n", shellJoin([]string{s}))
		}
		fmt.Fprintf(&b, "\techo %s >\"$HOME/.sbox/setup.sha\"\nfi\ntracer=\n", stamp)
	}
	for _, s := range start {
		s = strings.TrimSpace(s)
		if bg, ok := strings.CutSuffix(s, "&"); ok && !strings.HasSuffix(bg, "&") {
			q := shellJoin([]string{strings.TrimSpace(bg)})
			fmt.Fprintf(&b, "echo \"sbox: start: \"%s\" (in the background, output in ~/.sbox/start.log)\" >&2\n", q)
			fmt.Fprintf(&b, "nohup sh -c %s >>\"$HOME/.sbox/start.log\" 2>&1 </dev/null &\n", q)
			continue
		}
		fmt.Fprintf(&b, "step start %s\n", shellJoin([]string{s}))
	}
	return b.String()
}
