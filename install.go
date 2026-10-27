package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// _maxLimaBytes caps the Lima download (the 2.2.1 archive is 38 MB).
const _maxLimaBytes = 200 << 20

// installLima downloads the pinned Lima archive, checks its SHA-256 before
// unpacking anything, and installs it under the home folder, where
// findLimactl looks first. Nothing is left behind on failure.
func installLima(ctx context.Context, client *http.Client, a artifact, home string) error {
	dir := filepath.Join(home, _limaDir)
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", a.url, resp.Status)
	}

	tmp, err := os.CreateTemp(parent, "lima-*.tar.gz")
	if err != nil {
		return err
	}
	defer func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, _maxLimaBytes+1))
	switch {
	case err != nil:
		return fmt.Errorf("download %s: %w", a.url, err)
	case n > _maxLimaBytes:
		return fmt.Errorf("download %s: larger than %d MiB", a.url, _maxLimaBytes>>20)
	case hex.EncodeToString(h.Sum(nil)) != a.sha256:
		return fmt.Errorf("checksum mismatch for %s", a.url)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}

	staging, err := os.MkdirTemp(parent, "lima-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }() // gone already once renamed
	if err := extractTarGz(tmp, staging); err != nil {
		return fmt.Errorf("unpack %s: %w", a.url, err)
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.Rename(staging, dir)
}

// extractTarGz unpacks a .tar.gz into dest. Entries whose path, or whose
// symlink target, leaves dest are refused, and so are entries written
// through a symlink the archive created, since a chain of links that each
// look inside can still lead out. Only files, folders and symlinks are
// kept, and of the mode bits only whether a file is executable.
func extractTarGz(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()
	return extractTar(gz, dest, false)
}

// extractTar is extractTarGz for a plain tar. With filesOnly (a tar from a
// guest) anything but files and folders is refused.
func extractTar(r io.Reader, dest string, filesOnly bool) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	links := make(map[string]bool) // symlinks created, relative to dest
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		rel, err := insidePath(hdr.Name)
		if err != nil {
			return err
		}
		for dir := path.Dir(rel); dir != "."; dir = path.Dir(dir) {
			if links[dir] {
				return fmt.Errorf("%s: written through a link", hdr.Name)
			}
		}
		if filesOnly && hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeDir {
			return fmt.Errorf("%s: not a regular file", hdr.Name)
		}
		target := filepath.Join(dest, filepath.FromSlash(rel))
		switch hdr.Typeflag {
		case tar.TypeDir:
			err = os.MkdirAll(target, 0o755)
		case tar.TypeReg:
			err = writeFile(target, tr, hdr.Mode&0o111 != 0)
		case tar.TypeSymlink:
			if path.IsAbs(hdr.Linkname) {
				return fmt.Errorf("%s: link to %s, outside the folder", hdr.Name, hdr.Linkname)
			}
			if _, err := insidePath(path.Join(path.Dir(rel), hdr.Linkname)); err != nil {
				return fmt.Errorf("%s: link to %s, outside the folder", hdr.Name, hdr.Linkname)
			}
			if err = os.MkdirAll(filepath.Dir(target), 0o755); err == nil {
				err = os.Symlink(hdr.Linkname, target)
			}
			links[rel] = true
		}
		if err != nil {
			return err
		}
	}
}

// insidePath cleans an archive path and refuses one that leaves the root.
func insidePath(name string) (string, error) {
	clean := path.Clean(name)
	if path.IsAbs(name) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%s: outside the folder", name)
	}
	return clean, nil
}

func writeFile(target string, r io.Reader, executable bool) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if executable {
		mode = 0o755
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
