package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// target is a folder in a GitHub repository at a ref, from a URL.
type target struct {
	owner, repo string
	ref         string // branch, tag or commit; the default branch when empty
	path        string // a folder inside the repository, or ""
}

var (
	_ghName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	_sha    = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// parseTarget reads https://github.com/o/r[/tree/<ref>[/<path>]] or
// github.com/o/r[@ref][/path]. Anything else is not a target (a local
// path, as before).
func parseTarget(arg string) (target, bool) {
	rest, long := strings.CutPrefix(arg, "https://github.com/")
	if !long {
		var ok bool
		if rest, ok = strings.CutPrefix(arg, "github.com/"); !ok {
			return target{}, false
		}
	}
	parts := strings.Split(strings.TrimSuffix(rest, "/"), "/")
	if len(parts) < 2 {
		return target{}, false
	}
	t := target{owner: parts[0], repo: strings.TrimSuffix(parts[1], ".git")}
	more := parts[2:]
	if long {
		if len(more) > 0 {
			if more[0] != "tree" || len(more) < 2 {
				return target{}, false
			}
			t.ref, more = more[1], more[2:]
		}
	} else {
		t.repo, t.ref, _ = strings.Cut(t.repo, "@")
	}
	t.path = strings.Join(more, "/")
	for _, name := range []string{t.owner, t.repo} {
		if !_ghName.MatchString(name) || name == "." || name == ".." {
			return target{}, false
		}
	}
	if t.ref != "" && (!_ghName.MatchString(t.ref) || strings.Contains(t.ref, "..")) {
		return target{}, false
	}
	for _, seg := range more {
		if seg == "" || seg == "." || seg == ".." {
			return target{}, false
		}
	}
	return t, true
}

// display names the target at a commit, for headers and messages.
func (t target) display(commit string) string {
	s := fmt.Sprintf("github.com/%s/%s@%s", t.owner, t.repo, commit[:min(12, len(commit))])
	if t.path != "" {
		s += "/" + t.path
	}
	return s
}

// _codeload serves any ref of a public repository as a tarball.
const _codeload = "https://codeload.github.com"

// fetchTarget downloads the target's tarball from base (codeload) into
// cacheRoot/<owner>/<repo>/<ref>/ and returns its folder and the commit.
// No git runs: the tarball is extracted with the safe extractor, so no
// hooks, filters or submodules. A commit ref is reused from the cache;
// branches and tags move, so they are fetched again. max caps the
// download.
func fetchTarget(ctx context.Context, client *http.Client, base string, t target, cacheRoot string, max int64) (string, string, error) {
	ref := t.ref
	if ref == "" {
		ref = "HEAD"
	}
	dest := filepath.Join(cacheRoot, t.owner, t.repo, ref)
	name := fmt.Sprintf("github.com/%s/%s", t.owner, t.repo)
	commit, err := os.ReadFile(filepath.Join(dest, "commit"))
	if err != nil || !_sha.MatchString(ref) {
		if commit, err = download(ctx, client, base, t, ref, dest, max); err != nil {
			return "", "", fmt.Errorf("%s: %w", name, err)
		}
	}
	dir := filepath.Join(dest, "tree", filepath.FromSlash(t.path))
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", "", fmt.Errorf("%s@%s: no folder %q", name, ref, t.path)
	}
	return dir, string(commit), nil
}

// download fetches and unpacks one tarball into dest/tree, with the
// commit its pax header names in dest/commit.
func download(ctx context.Context, client *http.Client, base string, t target, ref, dest string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/%s/%s/tar.gz/%s", base, t.owner, t.repo, ref), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("no ref %q (or the repository is private)", ref)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("download: %s", resp.Status)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".download-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }()
	n, err := io.Copy(tmp, io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if n > max {
		return nil, fmt.Errorf("download larger than %d bytes", max)
	}

	commit, err := tarballCommit(tmp)
	if err != nil {
		return nil, err
	}
	staging, err := os.MkdirTemp(filepath.Dir(dest), ".extract-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if err := extractTarGz(tmp, staging); err != nil {
		return nil, err
	}
	top, err := os.ReadDir(staging)
	if err != nil || len(top) != 1 || !top[0].IsDir() {
		return nil, errors.New("the tarball has no single top folder")
	}
	if err := os.RemoveAll(dest); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return nil, err
	}
	if err := os.Rename(filepath.Join(staging, top[0].Name()), filepath.Join(dest, "tree")); err != nil {
		return nil, err
	}
	return commit, os.WriteFile(filepath.Join(dest, "commit"), commit, 0o644)
}

// tarballCommit reads the commit from a GitHub tarball's pax global header.
func tarballCommit(f *os.File) ([]byte, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	hdr, err := tar.NewReader(gz).Next()
	if err != nil {
		return nil, err
	}
	if c := hdr.PAXRecords["comment"]; hdr.Typeflag == tar.TypeXGlobalHeader && _sha.MatchString(c) {
		return []byte(c), nil
	}
	return nil, errors.New("the tarball names no commit")
}

// fetchFunc fetches a target and returns its folder and commit. Tests
// pass a fake.
type fetchFunc func(target) (dir, commit string, err error)

// githubFetch is the real fetchFunc: codeload, the user's cache folder,
// the run tar's size cap.
func githubFetch(ctx context.Context) fetchFunc {
	return func(t target) (string, string, error) {
		cache, err := os.UserCacheDir()
		if err != nil {
			return "", "", err
		}
		client := &http.Client{Timeout: 5 * time.Minute}
		return fetchTarget(ctx, client, _codeload, t, filepath.Join(cache, "sbox", "targets"), _maxRunBytes)
	}
}

// resolveTarget turns a run or scan argument into a local folder and the
// name to show for it: a GitHub URL is fetched, anything else is a path.
func resolveTarget(arg string, fetch fetchFunc) (dir, name string, err error) {
	t, ok := parseTarget(arg)
	if !ok {
		return arg, arg, nil
	}
	dir, commit, err := fetch(t)
	if err != nil {
		return "", "", err
	}
	return dir, t.display(commit), nil
}
