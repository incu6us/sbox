package main

import (
	"archive/tar"
	"bufio"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
)

//go:embed scripts/guest-init.sh
var _guestInit string

const (
	_defaultTimeout = 60 * time.Second
	_maxRunBytes    = 256 << 20 // ponytail: whole skill in one tar; raise if real skills need more

	exitTimeout = 124 // like timeout(1)
	exitInfra   = 125 // like docker run: sbox itself failed
)

// runResult is what one microVM run produced.
type runResult struct {
	started  bool
	exitCode int // the command's, valid when started && !timedOut
	timedOut bool
}

// runMain implements `sbox run [-timeout d] [-v] <dir> -- <command...>`.
// It exits with the command's status, 124 on timeout, 125 when sbox fails.
func runMain(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("sbox run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	timeout := flags.Duration("timeout", _defaultTimeout, "stop the microVM after this long")
	verbose := flags.Bool("v", false, "also show kernel and Firecracker output")
	flags.Usage = func() {
		_, _ = fmt.Fprint(stderr, "usage: sbox run [flags] <dir> -- <command> [args...]\n\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return exitInfra
	}
	rest := flags.Args()
	if len(rest) < 3 || rest[1] != "--" || *timeout < time.Second {
		flags.Usage()
		return exitInfra
	}

	start := time.Now()
	res, err := runVM(ctx, rest[0], rest[2:], *timeout, *verbose, stdout)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "sbox:", err)
		return exitInfra
	}
	elapsed := time.Since(start).Round(100 * time.Millisecond)
	switch {
	case res.timedOut:
		_, _ = fmt.Fprintf(stderr, "sbox: timed out after %s\n", *timeout)
		return exitTimeout
	case !res.started:
		_, _ = fmt.Fprintln(stderr, "sbox: the microVM stopped before the command started (-v shows why)")
		return exitInfra
	}
	_, _ = fmt.Fprintf(stderr, "sbox: exit %d in %s\n", res.exitCode, elapsed)
	return res.exitCode
}

// runVM sends the directory and command into a fresh microVM and streams
// the guest's output to out.
func runVM(ctx context.Context, dir string, command []string, timeout time.Duration, verbose bool, out io.Writer) (runResult, error) {
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return runResult{}, fmt.Errorf("%s: not a directory", dir)
	}
	id, err := randomHex(8)
	if err != nil {
		return runResult{}, err
	}
	nonce, err := randomHex(16)
	if err != nil {
		return runResult{}, err
	}

	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeRunTar(pw, dir, command, nonce)) }()

	// The VM side enforces the timeout; the host one only catches a hung VM.
	ctx, cancel := context.WithTimeout(ctx, timeout+30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "limactl", "shell", "--workdir", "/", _vmName,
		"sudo", _runScriptPath, id, strconv.Itoa(int(timeout.Seconds())))
	cmd.Stdin = pr
	cmd.Stderr = out
	console, err := cmd.StdoutPipe()
	if err != nil {
		return runResult{}, err
	}
	if err := cmd.Start(); err != nil {
		return runResult{}, fmt.Errorf("limactl: %w", err)
	}

	res := streamConsole(console, out, verbose, nonce)
	err = cmd.Wait()
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr) && exitErr.ExitCode() == exitTimeout:
		res.timedOut = true
	case err != nil:
		return res, fmt.Errorf("sbox-run: %w", err)
	}
	return res, nil
}

// streamConsole prints the guest's output and picks up the init's markers,
// which only count with the run's nonce. Kernel and Firecracker lines are
// noise unless verbose.
func streamConsole(r io.Reader, out io.Writer, verbose bool, nonce string) runResult {
	var (
		res     runResult
		blank   bool // held back: the init prints one before its exit marker
		start   = "SBOX-" + nonce + " START"
		exitPfx = "SBOX-" + nonce + " EXIT "
	)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if blank && !strings.HasPrefix(line, exitPfx) {
			_, _ = fmt.Fprintln(out, "│")
		}
		blank = false
		switch {
		case line == start:
			res.started = true
		case strings.HasPrefix(line, exitPfx):
			if code, err := strconv.Atoi(strings.TrimPrefix(line, exitPfx)); err == nil {
				res.exitCode = code
			}
		case res.started && line == "":
			blank = true
		case res.started && !isNoise(line):
			_, _ = fmt.Fprintln(out, "│ "+printable(line))
		case verbose:
			_, _ = fmt.Fprintln(out, "· "+printable(line))
		}
	}
	return res
}

// isNoise spots Firecracker's own log lines and kernel messages.
func isNoise(line string) bool {
	return strings.HasPrefix(line, "[") || (len(line) > 4 && line[4] == '-' && strings.Contains(line, "[") &&
		strings.HasPrefix(line, "20"))
}

// printable escapes control characters, so guest output can't drive the
// user's terminal.
func printable(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '\t' || unicode.IsPrint(r) {
			b.WriteRune(r)
		} else {
			fmt.Fprintf(&b, `\x%02x`, r)
		}
	}
	return b.String()
}

// writeRunTar packs what the guest gets: skill/ (the directory, symlinks
// kept as links), sbox/init.sh and sbox/nonce for root only, and
// sbox/cmd.sh for the runner.
func writeRunTar(w io.Writer, dir string, command []string, nonce string) error {
	tw := tar.NewWriter(w)
	total := int64(0)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		} else if !info.Mode().IsRegular() && !info.IsDir() {
			return nil // sockets, devices, pipes
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(filepath.Join("skill", rel))
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if total += info.Size(); total > _maxRunBytes {
			return fmt.Errorf("%s: larger than %d MiB", dir, _maxRunBytes>>20)
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return err
	}

	cmdScript := "#!/bin/sh\nexec " + shellJoin(command) + "\n"
	for _, f := range []struct {
		name, body string
		mode       int64
	}{
		{"sbox/init.sh", _guestInit, 0o700},
		{"sbox/nonce", nonce, 0o400},
		{"sbox/cmd.sh", cmdScript, 0o755},
	} {
		hdr := &tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := io.WriteString(tw, f.body); err != nil {
			return err
		}
	}
	return tw.Close()
}

// shellJoin quotes each argument for sh, so the command runs as given.
func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", errors.New("no randomness available")
	}
	return hex.EncodeToString(b), nil
}
