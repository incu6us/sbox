package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoOf(t *testing.T) {
	t.Parallel()
	for url, want := range map[string]string{
		"https://github.com/Org/Repo.git":     "github.com/org/repo",
		"https://github.com/org/repo":         "github.com/org/repo",
		"git@github.com:org/repo.git":         "github.com/org/repo",
		"ssh://git@github.com/org/repo.git":   "github.com/org/repo",
		"https://user@gitlab.example/a/b.git": "gitlab.example/a/b",
		"/srv/git/repo.git":                   "",
		"https://github.com/org":              "",
	} {
		if got := repoOf(url); got != want {
			t.Errorf("repoOf(%q) = %q, want %q", url, got, want)
		}
	}
}

// gitRepo makes a repository whose origin reads url but pushes to a local
// bare repository (insteadOf), with main tracking origin/main.
func gitRepo(t *testing.T, url string) (dir, bare string) {
	t.Helper()
	dir, bare = t.TempDir(), t.TempDir()
	git := func(d string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", d}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(bare, "init", "-q", "--bare", "-b", "main")
	git(dir, "init", "-q", "-b", "main")
	git(dir, "config", "user.name", "t")
	git(dir, "config", "user.email", "t@t")
	git(dir, "config", "commit.gpgsign", "false")
	git(dir, "remote", "add", "origin", url)
	git(dir, "config", "url."+bare+".insteadOf", url)
	writeTestFile(t, filepath.Join(dir, "a.txt"), "v1\n")
	git(dir, "add", "-A")
	git(dir, "commit", "-qm", "init")
	git(dir, "push", "-q", "-u", "origin", "main")
	return dir, bare
}

func TestPushTarget(t *testing.T) {
	t.Parallel()
	dir, _ := gitRepo(t, "https://github.com/org/repo.git")
	if repo, err := pushTarget(dir, []string{"github.com/org/repo"}); err != nil || repo != "github.com/org/repo" {
		t.Errorf("listed: %q, %v", repo, err)
	}
	for list, want := range map[string]string{"github.com/org/other": "not in push", "": "push: [github.com/org/repo]"} {
		var push []string
		if list != "" {
			push = []string{list}
		}
		if _, err := pushTarget(dir, push); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("push %q: %v, want %q", list, err, want)
		}
	}
	if _, err := pushTarget(t.TempDir(), []string{"github.com/org/repo"}); err == nil {
		t.Error("not a repository: no error")
	}
}

func TestCommitAndPush(t *testing.T) {
	t.Parallel()
	dir, bare := gitRepo(t, "https://github.com/org/repo.git")
	writeTestFile(t, filepath.Join(dir, "a.txt"), "v2\n")
	writeTestFile(t, filepath.Join(dir, "new.txt"), "new\n")
	writeTestFile(t, filepath.Join(dir, "mine.txt"), "not the run's\n")
	var out strings.Builder
	if err := commitAndPush(dir, []string{"a.txt", "new.txt"}, "sbox run: fix", []string{"github.com/org/repo"}, &out); err != nil {
		t.Fatalf("commitAndPush: %v\n%s", err, out.String())
	}
	log, _ := exec.Command("git", "-C", bare, "log", "-1", "--name-only", "--format=%s").Output()
	if got := string(log); !strings.Contains(got, "sbox run: fix") || !strings.Contains(got, "a.txt") || !strings.Contains(got, "new.txt") || strings.Contains(got, "mine.txt") {
		t.Errorf("pushed commit:\n%s\nwant the run's two files only", got)
	}
}
