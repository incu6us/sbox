package main

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"slices"
	"strings"
)

// _remote is a git remote URL: https, ssh, or scp-like (user@host:path),
// down to host, owner and repository.
var _remote = regexp.MustCompile(`^(?:(?:https?|ssh)://)?(?:[^@/]+@)?([a-z0-9.-]+\.[a-z]+)[:/]([a-z0-9_.-]+)/([a-z0-9_.-]+?)(?:\.git)?/?$`)

// repoOf is a remote URL as host/owner/repo, lowercase, or "".
func repoOf(url string) string {
	m := _remote.FindStringSubmatch(strings.ToLower(url))
	if m == nil {
		return ""
	}
	return m[1] + "/" + m[2] + "/" + m[3]
}

// git runs git in dir, on the Mac's own repository with the user's own
// settings and credentials; never on a .git that came from a guest.
func git(dir string, args ...string) (string, error) {
	var out, errOut bytes.Buffer
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(errOut.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// pushTarget is the repository the current branch pushes to, as its
// upstream remote's URL reads in the config (not rewritten), checked
// against the config's push list. It catches a wrong folder or remote;
// what the guest may push to is the token's scope.
func pushTarget(dir string, push []string) (string, error) {
	upstream, err := git(dir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	if err != nil {
		return "", fmt.Errorf("%s: the current branch has no upstream to push to (git push -u first): %w", dir, err)
	}
	remote, _, _ := strings.Cut(upstream, "/")
	url, err := git(dir, "config", "--get", "remote."+remote+".pushurl")
	if err != nil {
		if url, err = git(dir, "config", "--get", "remote."+remote+".url"); err != nil {
			return "", err
		}
	}
	repo := repoOf(url)
	switch {
	case repo == "":
		return "", fmt.Errorf("remote %s: %q isn't host/owner/repo", remote, url)
	case len(push) == 0:
		return "", fmt.Errorf("-push needs the repository in the config: push: [%s]", repo)
	case !slices.Contains(push, repo):
		return "", fmt.Errorf("%s pushes to %s, which is not in push: %s", dir, repo, strings.Join(push, ", "))
	}
	return repo, nil
}

// commitAndPush commits paths alone (whatever else is staged stays so)
// with message, checks the remote again, and pushes.
func commitAndPush(dir string, paths []string, message string, push []string, out io.Writer) error {
	repo, err := pushTarget(dir, push)
	if err != nil {
		return err
	}
	for _, args := range [][]string{
		append([]string{"add", "-A", "--"}, paths...),
		append([]string{"commit", "-q", "-m", message, "--"}, paths...),
		{"push", "-q"},
	} {
		if _, err := git(dir, args...); err != nil {
			return err
		}
	}
	_, _ = fmt.Fprintf(out, "sbox: committed %d %s and pushed to %s\n", len(paths), plural(len(paths), "file", "files"), repo)
	return nil
}
