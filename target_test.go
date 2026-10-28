package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseTarget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		arg  string
		want target
		ok   bool
	}{
		{"https://github.com/o/r", target{owner: "o", repo: "r"}, true},
		{"https://github.com/o/r.git", target{owner: "o", repo: "r"}, true},
		{"https://github.com/o/r/", target{owner: "o", repo: "r"}, true},
		{"https://github.com/o/r/tree/v1.2", target{owner: "o", repo: "r", ref: "v1.2"}, true},
		{"https://github.com/o/r/tree/main/plugins/x", target{owner: "o", repo: "r", ref: "main", path: "plugins/x"}, true},
		{"github.com/o/r", target{owner: "o", repo: "r"}, true},
		{"github.com/o/r@v2", target{owner: "o", repo: "r", ref: "v2"}, true},
		{"github.com/o/r@v2/sub/dir", target{owner: "o", repo: "r", ref: "v2", path: "sub/dir"}, true},
		{"github.com/o/r/sub", target{owner: "o", repo: "r", path: "sub"}, true},
		{"o/r@v1", target{}, false},
		{"./github.com/o/r", target{}, false},
		{"https://gitlab.com/o/r", target{}, false},
		{"https://github.com/o", target{}, false},
		{"github.com/o/../r", target{}, false},
		{"github.com/o/r@a..b", target{}, false},
		{"github.com/o/r/tree/main/../../x", target{}, false},
		{"https://github.com/o/r/blob/main/README.md", target{}, false},
	}
	for _, tt := range tests {
		got, ok := parseTarget(tt.arg)
		if ok != tt.ok || got != tt.want {
			t.Errorf("parseTarget(%q) = %+v, %v; want %+v, %v", tt.arg, got, ok, tt.want, tt.ok)
		}
	}
	if s := (target{owner: "o", repo: "r", path: "x"}).display("0123456789abcdef0123456789abcdef01234567"); s != "github.com/o/r@0123456789ab/x" {
		t.Errorf("display = %q", s)
	}
}

// githubTarball is a codeload-like archive: a pax global header naming the
// commit, then everything under one top folder.
func githubTarball(t *testing.T, commit string, files map[string]string, extra ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": commit}}))
	must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "r-main/", Mode: 0o755}))
	for name, body := range files {
		must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "r-main/" + name, Mode: 0o644, Size: int64(len(body))}))
		_, err := tw.Write([]byte(body))
		must(err)
	}
	for _, h := range extra {
		must(tw.WriteHeader(h))
	}
	must(tw.Close())
	must(gz.Close())
	return buf.Bytes()
}

func TestFetchTarget(t *testing.T) {
	t.Parallel()
	const sha = "0123456789abcdef0123456789abcdef01234567"
	good := githubTarball(t, sha, map[string]string{"README.md": "hi\n", "plugins/x/plugin.json": "{}\n"})
	evil := githubTarball(t, sha, nil, &tar.Header{Typeflag: tar.TypeReg, Name: "r-main/../../escape", Mode: 0o644})
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/o/r/tar.gz/HEAD", "/o/r/tar.gz/main", "/o/r/tar.gz/" + sha:
			_, _ = w.Write(good)
		case "/o/evil/tar.gz/HEAD":
			_, _ = w.Write(evil)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	fetch := func(cache string, tg target, max int64) (string, string, error) {
		return fetchTarget(context.Background(), srv.Client(), srv.URL, tg, cache, max, false)
	}

	cache := t.TempDir()
	dir, commit, err := fetch(cache, target{owner: "o", repo: "r"}, 1<<20)
	if err != nil || commit != sha {
		t.Fatalf("fetch: %v, commit %q", err, commit)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "README.md")); string(data) != "hi\n" || !strings.HasPrefix(dir, cache) {
		t.Errorf("fetched %s: README %q; want the repo's files, under the cache", dir, data)
	}
	if dir, _, err := fetch(cache, target{owner: "o", repo: "r", ref: "main", path: "plugins/x"}, 1<<20); err != nil || filepath.Base(dir) != "x" {
		t.Errorf("a folder in the repo: %q, %v", dir, err)
	}

	hits.Store(0)
	for range 2 {
		if _, _, err := fetch(cache, target{owner: "o", repo: "r", ref: sha}, 1<<20); err != nil {
			t.Fatal(err)
		}
	}
	if hits.Load() != 1 {
		t.Errorf("a commit ref was downloaded %d times, want once (it can't move)", hits.Load())
	}
	if _, _, err := fetchTarget(context.Background(), srv.Client(), srv.URL, target{owner: "o", repo: "r", ref: sha}, cache, 1<<20, true); err != nil || hits.Load() != 2 {
		t.Errorf("-refetch: %v, %d downloads; want the commit fetched again", err, hits.Load())
	}

	// Entries unused for 30 days go on the next fetch; one just used stays.
	month := time.Now().Add(-31 * 24 * time.Hour)
	stale := filepath.Join(cache, "old", "repo", sha)
	if err := os.MkdirAll(filepath.Join(stale, "tree"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{stale, filepath.Join(cache, "o", "r", sha)} {
		if err := os.Chtimes(p, month, month); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := fetch(cache, target{owner: "o", repo: "r", ref: sha}, 1<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("an entry unused for a month is still cached (%v)", err)
	}
	if _, err := os.Stat(filepath.Join(cache, "o", "r", sha, "tree", "README.md")); err != nil {
		t.Errorf("the entry just used went too: %v", err)
	}

	for _, tt := range []struct {
		name    string
		tg      target
		max     int64
		wantErr string
	}{
		{"missing ref", target{owner: "o", repo: "r", ref: "nope"}, 1 << 20, "nope"},
		{"escaping entry", target{owner: "o", repo: "evil"}, 1 << 20, "outside"},
		{"too big", target{owner: "o", repo: "r"}, 100, "larger than"},
		{"missing folder", target{owner: "o", repo: "r", path: "none"}, 1 << 20, "none"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if _, _, err := fetch(filepath.Join(root, "cache"), tt.tg, tt.max); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want one naming %q", err, tt.wantErr)
			}
			if _, err := os.Stat(filepath.Join(root, "escape")); err == nil {
				t.Error("an entry escaped the cache")
			}
		})
	}
}

func TestResolveTarget(t *testing.T) {
	t.Parallel()
	const sha = "0123456789abcdef0123456789abcdef01234567"
	var asked []target
	fetch := func(tg target) (string, string, error) {
		asked = append(asked, tg)
		if tg.repo == "missing" {
			return "", "", os.ErrNotExist
		}
		return "/cache/o/r/HEAD/tree", sha, nil
	}
	if dir, name, where, err := resolveTarget("./local", fetch); dir != "./local" || name != "./local" || where != (targetJSON{Path: "./local"}) || err != nil || len(asked) != 0 {
		t.Errorf("a local path: %q, %q, %+v, %v, fetched %v", dir, name, where, err, asked)
	}
	if dir, name, where, err := resolveTarget("https://github.com/o/r/tree/main/plugins/x", fetch); dir != "/cache/o/r/HEAD/tree" || name != "github.com/o/r@0123456789ab/plugins/x" || err != nil ||
		where != (targetJSON{URL: "https://github.com/o/r", Commit: sha, Path: "plugins/x"}) {
		t.Errorf("a URL: %q, %q, %+v, %v", dir, name, where, err)
	}
	if _, _, _, err := resolveTarget("github.com/o/missing", fetch); err == nil {
		t.Error("a failed fetch: no error")
	}
}

func TestDevMainFromGitHub(t *testing.T) {
	t.Parallel()
	const sha = "0123456789abcdef0123456789abcdef01234567"
	src, cwd := t.TempDir(), t.TempDir()
	writeTestFile(t, filepath.Join(src, "README.md"), "from github\n")
	fetch := func(tg target) (string, string, error) { return src, sha, nil }
	vm := fakeVM{answer: func(args []string) string {
		if args[0] == "start" {
			return "READY " + args[1] + "\n"
		}
		return ""
	}}
	var stdout, stderr bytes.Buffer
	host := devHost{stateDir: t.TempDir(), cwd: cwd, fetch: fetch}
	if code := devMain(context.Background(), []string{"https://github.com/o/myrepo"}, strings.NewReader(""), &stdout, &stderr, vm.run, host); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	local := filepath.Join(cwd, "myrepo")
	if data, _ := os.ReadFile(filepath.Join(local, "README.md")); string(data) != "from github\n" {
		t.Errorf("./myrepo/README.md = %q, want the repository's", data)
	}
	if vm.calls[1][2] != mustProjectID(t, local) || !strings.Contains(stderr.String(), "github.com/o/myrepo@0123456789ab") {
		t.Errorf("calls %q, stderr %q; want a session on ./myrepo and the commit named", vm.calls, stderr.String())
	}

	again := fakeVM{}
	stderr.Reset()
	if code := devMain(context.Background(), []string{"github.com/o/myrepo"}, strings.NewReader(""), &stdout, &stderr, again.run, host); code != exitUsage ||
		len(again.calls) != 0 || !strings.Contains(stderr.String(), "sbox dev "+local) {
		t.Errorf("folder exists: exit %d, calls %q, stderr %q; want a pointer at the folder", code, again.calls, stderr.String())
	}
}
