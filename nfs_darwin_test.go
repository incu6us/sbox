//go:build darwin

package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// compoundArgs builds a COMPOUND's operations: add writes the op number and
// returns the writer for its arguments.
type compoundArgs struct {
	w xdrWriter
	n uint32
}

func (c *compoundArgs) add(op uint32) *xdrWriter {
	c.n++
	c.w.u32(op)
	return &c.w
}

// rpc sends one call to s and returns the reply's accept status and the
// reader positioned at the procedure's results.
func rpc(t *testing.T, s *nfsServer, proc uint32, args []byte) (uint32, *xdrReader) {
	t.Helper()
	var w xdrWriter
	w.u32(0x1234) // xid
	w.u32(0)      // CALL
	w.u32(2)      // RPC version
	w.u32(_nfsProgram)
	w.u32(4)
	w.u32(proc)
	w.u32(1) // AUTH_SYS, ignored
	w.opaque([]byte("cred"))
	w.u32(0) // verifier AUTH_NONE
	w.opaque(nil)
	w.b = append(w.b, args...)

	r := &xdrReader{b: s.call(w.b)}
	if xid, msg, stat := r.u32(), r.u32(), r.u32(); xid != 0x1234 || msg != 1 || stat != 0 {
		t.Fatalf("reply header: xid %x, type %d, reply_stat %d", xid, msg, stat)
	}
	r.u32() // verifier flavor
	r.opaque(400)
	return r.u32(), r
}

// compound runs c on s and returns COMPOUND's status and a reader at the
// first result (the op number already checked against want ops in order).
func compound(t *testing.T, s *nfsServer, minor uint32, c *compoundArgs) (uint32, *xdrReader) {
	t.Helper()
	var w xdrWriter
	w.opaque([]byte("tag"))
	w.u32(minor)
	w.u32(c.n)
	w.b = append(w.b, c.w.b...)
	accept, r := rpc(t, s, 1, w.b)
	if accept != 0 {
		t.Fatalf("COMPOUND accept_stat %d", accept)
	}
	status := r.u32()
	if tag := r.opaque(1024); string(tag) != "tag" {
		t.Fatalf("tag %q", tag)
	}
	r.u32() // results count
	return status, r
}

// result reads one op result header and checks its op number.
func result(t *testing.T, r *xdrReader, op uint32) uint32 {
	t.Helper()
	if got := r.u32(); got != op {
		t.Fatalf("result for op %d, want %d", got, op)
	}
	return r.u32()
}

func testFolder(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func inodeOf(fi os.FileInfo) uint64 { return fi.Sys().(*syscall.Stat_t).Ino }

func TestNFSNull(t *testing.T) {
	t.Parallel()
	s := newNFSServer(testFolder(t), bytes.Repeat([]byte{1}, 32))
	if accept, r := rpc(t, s, 0, nil); accept != 0 || len(r.b) != 0 {
		t.Errorf("NULL: accept %d, %d bytes left", accept, len(r.b))
	}
	if accept, _ := rpc(t, s, 9, nil); accept != 3 {
		t.Errorf("procedure 9: accept %d, want PROC_UNAVAIL", accept)
	}
}

func TestNFSRootAttributes(t *testing.T) {
	t.Parallel()
	dir := testFolder(t)
	s := newNFSServer(dir, bytes.Repeat([]byte{1}, 32))

	var c compoundArgs
	c.add(_opPutRootFH)
	c.add(_opGetFH)
	c.add(_opGetAttr).bitmap(attrMask(_attrType, _attrSize, _attrFileID, _attrMode, _attrOwner))
	status, r := compound(t, s, 0, &c)
	if status != _nfsOK {
		t.Fatalf("status %d", status)
	}
	result(t, r, _opPutRootFH)
	result(t, r, _opGetFH)
	if fh := r.opaque(128); len(fh) == 0 {
		t.Error("empty root handle")
	}
	result(t, r, _opGetAttr)
	mask := r.bitmap()
	vals := &xdrReader{b: r.opaque(1 << 20)}
	if !slices.Equal(mask, attrMask(_attrType, _attrSize, _attrFileID, _attrMode, _attrOwner)) {
		t.Errorf("attribute mask %v", mask)
	}
	var st os.FileInfo
	st, _ = os.Lstat(dir)
	if typ := vals.u32(); typ != _nf4Dir {
		t.Errorf("type %d, want directory", typ)
	}
	vals.u64() // size
	if id := vals.u64(); id != inodeOf(st) {
		t.Errorf("fileid %d, want the inode %d", id, inodeOf(st))
	}
	if mode := vals.u32(); mode != uint32(st.Mode().Perm()) {
		t.Errorf("mode %o, want %o", mode, st.Mode().Perm())
	}
	if owner := vals.str(64); owner != "1000" {
		t.Errorf("owner %q, want the guest's runner, 1000", owner)
	}
}

func TestNFSBadCompound(t *testing.T) {
	t.Parallel()
	s := newNFSServer(testFolder(t), bytes.Repeat([]byte{1}, 32))

	var c compoundArgs
	c.add(_opPutRootFH)
	if status, _ := compound(t, s, 1, &c); status != _nfs4errMinorVersMismatch {
		t.Errorf("minor version 1: %d, want MINOR_VERS_MISMATCH", status)
	}

	c = compoundArgs{}
	c.add(_opGetFH)
	status, r := compound(t, s, 0, &c)
	if status != _nfs4errNoFileHandle || result(t, r, _opGetFH) != _nfs4errNoFileHandle {
		t.Errorf("GETFH with no handle: %d, want NOFILEHANDLE", status)
	}

	c = compoundArgs{}
	c.add(_opPutRootFH)
	c.add(9999)
	c.add(_opGetFH)
	status, r = compound(t, s, 0, &c)
	result(t, r, _opPutRootFH)
	if status != _nfs4errOpIllegal || result(t, r, _opIllegal) != _nfs4errOpIllegal || len(r.b) != 0 {
		t.Errorf("unknown op: %d, want OP_ILLEGAL and nothing after it", status)
	}
}

// lookupHandle walks names from the root and returns the last handle.
func lookupHandle(t *testing.T, s *nfsServer, names ...string) []byte {
	t.Helper()
	var c compoundArgs
	c.add(_opPutRootFH)
	for _, n := range names {
		c.add(_opLookup).str(n)
	}
	c.add(_opGetFH)
	status, r := compound(t, s, 0, &c)
	if status != _nfsOK {
		t.Fatalf("lookup %v: %d", names, status)
	}
	result(t, r, _opPutRootFH)
	for range names {
		result(t, r, _opLookup)
	}
	result(t, r, _opGetFH)
	return r.opaque(128)
}

// fhStatus runs PUTFH h and then ops (no arguments) and returns the status.
func fhStatus(t *testing.T, s *nfsServer, h []byte, add func(c *compoundArgs)) uint32 {
	t.Helper()
	var c compoundArgs
	c.add(_opPutFH).opaque(h)
	add(&c)
	status, _ := compound(t, s, 0, &c)
	return status
}

func TestNFSLookup(t *testing.T) {
	t.Parallel()
	dir := testFolder(t)
	outside := t.TempDir()
	for _, d := range []string{"app", "app/deep"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("no"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "out")); err != nil {
		t.Fatal(err)
	}
	s := newNFSServer(dir, bytes.Repeat([]byte{1}, 32))

	deep := lookupHandle(t, s, "app", "deep")
	if status := fhStatus(t, s, deep, func(c *compoundArgs) { c.add(_opLookupP); c.add(_opLookup).str("deep") }); status != _nfsOK {
		t.Errorf("PUTFH app/deep, LOOKUPP, LOOKUP deep: %d", status)
	}
	if status := fhStatus(t, s, lookupHandle(t, s), func(c *compoundArgs) { c.add(_opLookupP) }); status != _nfs4errNoEnt {
		t.Errorf("LOOKUPP at the root: %d, want NOENT", status)
	}

	link := lookupHandle(t, s, "out")
	var c compoundArgs
	c.add(_opPutFH).opaque(link)
	c.add(_opGetAttr).bitmap(attrMask(_attrType))
	status, r := compound(t, s, 0, &c)
	result(t, r, _opPutFH)
	result(t, r, _opGetAttr)
	r.bitmap()
	if typ := (&xdrReader{b: r.opaque(64)}).u32(); status != _nfsOK || typ != _nf4Lnk {
		t.Errorf("the symlink: status %d, type %d, want a link, not what it points at", status, typ)
	}

	forged := slices.Clone(deep)
	forged[len(forged)-1] ^= 1
	theirs := newNFSServer(outside, bytes.Repeat([]byte{2}, 32))
	secret, _, err := lstatHandle(filepath.Join(outside, "secret"))
	if err != nil {
		t.Fatal(err)
	}
	minted, _, _ := lstatHandle(filepath.Join(outside, "secret"))
	for _, tt := range []struct {
		name string
		h    []byte
		want uint32
	}{
		{"a forged handle", forged, _nfs4errBadHandle},
		{"another server's handle", theirs.handle(secret), _nfs4errBadHandle},
		{"a short handle", deep[:5], _nfs4errBadHandle},
		{"a file outside the folder", s.handle(minted), _nfs4errStale},
	} {
		if status := fhStatus(t, s, tt.h, func(*compoundArgs) {}); status != tt.want {
			t.Errorf("%s: %d, want %d", tt.name, status, tt.want)
		}
	}

	readme := lookupHandle(t, s, "README.md")
	for _, tt := range []struct {
		name string
		h    []byte
		arg  string
		want uint32
	}{
		{"missing", deep, "nope", _nfs4errNoEnt},
		{"dot dot", deep, "..", _nfs4errBadName},
		{"dot", deep, ".", _nfs4errBadName},
		{"a slash", lookupHandle(t, s), "app/deep", _nfs4errBadName},
		{"empty", deep, "", _nfs4errInval},
		{"too long", deep, strings.Repeat("a", 256), _nfs4errNameTooLong},
		{"in a file", readme, "x", _nfs4errNotDir},
		{"through the symlink", link, "secret", _nfs4errSymlink},
	} {
		if status := fhStatus(t, s, tt.h, func(c *compoundArgs) { c.add(_opLookup).str(tt.arg) }); status != tt.want {
			t.Errorf("LOOKUP %s: %d, want %d", tt.name, status, tt.want)
		}
	}
}

func TestNFSClientID(t *testing.T) {
	t.Parallel()
	s := newNFSServer(testFolder(t), bytes.Repeat([]byte{1}, 32))

	var c compoundArgs
	w := c.add(_opSetClientID)
	w.b = append(w.b, "verifier"...)
	w.opaque([]byte("linux guest"))
	w.u32(0x40000000) // callback program
	w.str("tcp")
	w.str("172.16.0.2.3.4")
	w.u32(1) // callback ident
	status, r := compound(t, s, 0, &c)
	if status != _nfsOK || result(t, r, _opSetClientID) != _nfsOK {
		t.Fatalf("SETCLIENTID: %d", status)
	}
	id, confirm := r.u64(), r.take(8)

	run := func(op uint32, id uint64, verf []byte) uint32 {
		var c compoundArgs
		w := c.add(op)
		w.u64(id)
		w.b = append(w.b, verf...)
		status, _ := compound(t, s, 0, &c)
		return status
	}
	if status := run(_opSetClientIDConfirm, id, []byte("wrongone")); status != _nfs4errStaleClientID {
		t.Errorf("confirm with the wrong verifier: %d, want STALE_CLIENTID", status)
	}
	if status := run(_opSetClientIDConfirm, id, confirm); status != _nfsOK {
		t.Errorf("confirm: %d", status)
	}
	if status := run(_opRenew, id, nil); status != _nfsOK {
		t.Errorf("RENEW: %d", status)
	}
	if status := run(_opRenew, id+100, nil); status != _nfs4errStaleClientID {
		t.Errorf("RENEW an unknown client: %d, want STALE_CLIENTID", status)
	}
}

func TestNFSAttributes(t *testing.T) {
	t.Parallel()
	dir := testFolder(t)
	s := newNFSServer(dir, bytes.Repeat([]byte{1}, 32))
	fh := lookupHandle(t, s, "README.md")

	getattr := func(attrs ...int) ([]uint32, *xdrReader) {
		var c compoundArgs
		c.add(_opPutFH).opaque(fh)
		c.add(_opGetAttr).bitmap(attrMask(attrs...))
		status, r := compound(t, s, 0, &c)
		result(t, r, _opPutFH)
		if status != _nfsOK || result(t, r, _opGetAttr) != _nfsOK {
			t.Fatalf("GETATTR: %d", status)
		}
		return r.bitmap(), &xdrReader{b: r.opaque(1 << 20)}
	}

	mask, v := getattr(_attrSupportedAttrs, _attrType, _attrFHExpireType, _attrChange, _attrSize,
		_attrLinkSupport, _attrSymlinkSupport, _attrNamedAttr, _attrFSID, _attrUniqueHandles,
		_attrLeaseTime, _attrRdattrError, 12 /* acl: not supported */, _attrFileHandle, _attrFileID,
		_attrMaxFileSize, _attrMaxName, _attrMaxRead, _attrMaxWrite, _attrMode, _attrNumLinks,
		_attrOwner, _attrOwnerGroup, _attrRawDev, _attrSpaceUsed, _attrTimeAccess, _attrTimeDelta,
		_attrTimeMetadata, _attrTimeModify, _attrMountedOnFileID)
	if hasAttr(mask, 12) {
		t.Error("the reply claims acl")
	}
	supported := v.bitmap()
	for _, a := range []int{_attrType, _attrChange, _attrFileHandle, _attrTimeModify, _attrMountedOnFileID} {
		if !hasAttr(supported, a) || !hasAttr(mask, a) {
			t.Errorf("attribute %d missing from supported_attrs or the reply", a)
		}
	}
	fi, _ := os.Lstat(filepath.Join(dir, "README.md"))
	typ, expire, change, size := v.u32(), v.u32(), v.u64(), v.u64()
	links, symlinks, named := v.u32(), v.u32(), v.u32()
	fsidMajor, fsidMinor := v.u64(), v.u64()
	unique, lease, rdErr := v.u32(), v.u32(), v.u32()
	handle := v.opaque(128)
	fileid, maxSize, maxName, maxRead, maxWrite := v.u64(), v.u64(), v.u32(), v.u64(), v.u64()
	mode, nlink, owner, group := v.u32(), v.u32(), v.str(64), v.str(64)
	v.u32()
	v.u32() // rawdev
	v.u64() // space used
	v.u64()
	v.u32() // atime
	deltaSec, deltaNsec := v.u64(), v.u32()
	v.u64()
	v.u32() // ctime
	mtimeSec, mtimeNsec := v.u64(), v.u32()
	mounted := v.u64()
	for _, c := range []struct {
		name      string
		got, want uint64
	}{
		{"type", uint64(typ), _nf4Reg},
		{"fh_expire_type (persistent)", uint64(expire), 0},
		{"size", size, 6},
		{"link_support", uint64(links), 1},
		{"symlink_support", uint64(symlinks), 1},
		{"named_attr", uint64(named), 0},
		{"unique_handles", uint64(unique), 1},
		{"lease_time", uint64(lease), 90},
		{"rdattr_error", uint64(rdErr), _nfsOK},
		{"fileid", fileid, inodeOf(fi)},
		{"maxname", uint64(maxName), 255},
		{"maxread", maxRead, 1 << 20},
		{"maxwrite", maxWrite, 1 << 20},
		{"mode", uint64(mode), 0o644},
		{"numlinks", uint64(nlink), 1},
		{"time_delta", deltaSec*1e9 + uint64(deltaNsec), 1},
		{"time_modify", mtimeSec*1e9 + uint64(mtimeNsec), uint64(fi.ModTime().UnixNano())},
		{"mounted_on_fileid", mounted, inodeOf(fi)},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
	if change == 0 || fsidMajor == 0 && fsidMinor == 0 || maxSize == 0 || !bytes.Equal(handle, fh) || owner != "1000" || group != "1000" || v.err || len(v.b) != 0 {
		t.Errorf("change %d, fsid %d.%d, maxfilesize %d, handle %x (want %x), owner %q:%q, decode error %v, %d bytes left",
			change, fsidMajor, fsidMinor, maxSize, handle, fh, owner, group, v.err, len(v.b))
	}

	// The folder's file system, from statfs, and the fixed answers.
	_, v = getattr(_attrCaseInsensitive, _attrCasePreserving, _attrFilesTotal, _attrMaxLink, _attrSpaceTotal)
	if ci, cp, files, maxLink, space := v.u32(), v.u32(), v.u64(), v.u32(), v.u64(); ci != 0 || cp != 1 || files == 0 || maxLink == 0 || space == 0 {
		t.Errorf("case_insensitive %d, case_preserving %d, files_total %d, maxlink %d, space_total %d", ci, cp, files, maxLink, space)
	}

	// change follows the file: a write on the Mac moves it.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello again\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, v = getattr(_attrChange)
	if v.u64() == change {
		t.Error("change didn't move after a write")
	}
}

func TestNFSAccess(t *testing.T) {
	t.Parallel()
	dir := testFolder(t)
	s := newNFSServer(dir, bytes.Repeat([]byte{1}, 32))
	for _, tt := range []struct {
		name      string
		h         []byte
		ask, want uint32
	}{
		{"a 0644 file", lookupHandle(t, s, "README.md"), _accessRead | _accessModify | _accessExecute, _accessRead | _accessModify},
		{"the folder", lookupHandle(t, s), _accessRead | _accessLookup | _accessModify, _accessRead | _accessLookup | _accessModify},
	} {
		var c compoundArgs
		c.add(_opPutFH).opaque(tt.h)
		c.add(_opAccess).u32(tt.ask)
		status, r := compound(t, s, 0, &c)
		result(t, r, _opPutFH)
		if status != _nfsOK || result(t, r, _opAccess) != _nfsOK {
			t.Fatalf("%s: ACCESS %d", tt.name, status)
		}
		if supported, granted := r.u32(), r.u32(); supported != tt.ask || granted != tt.want {
			t.Errorf("%s: supported %b, granted %b, want %b and %b", tt.name, supported, granted, tt.ask, tt.want)
		}
	}
}

// readdir lists dir from cookie with maxcount, returning names, cookies and eof.
func readdir(t *testing.T, s *nfsServer, h []byte, cookie uint64, maxcount uint32) (uint32, []string, []uint64, bool) {
	t.Helper()
	var c compoundArgs
	c.add(_opPutFH).opaque(h)
	w := c.add(_opReadDir)
	w.u64(cookie)
	w.b = append(w.b, make([]byte, 8)...)
	w.u32(maxcount)
	w.u32(maxcount)
	w.bitmap(attrMask(_attrType, _attrFileID))
	status, r := compound(t, s, 0, &c)
	result(t, r, _opPutFH)
	if result(t, r, _opReadDir); status != _nfsOK {
		return status, nil, nil, false
	}
	r.take(8) // cookie verifier
	var names []string
	var cookies []uint64
	for r.u32() == 1 {
		cookies = append(cookies, r.u64())
		names = append(names, r.str(255))
		r.bitmap()
		r.opaque(1024)
	}
	return status, names, cookies, r.u32() == 1
}

func TestNFSReadDir(t *testing.T) {
	t.Parallel()
	dir := testFolder(t)
	var want []string
	for i := range 30 {
		name := fmt.Sprintf("file-%02d", i)
		want = append(want, name)
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want = append(want, "README.md")
	slices.Sort(want)
	s := newNFSServer(dir, bytes.Repeat([]byte{1}, 32))
	root := lookupHandle(t, s)

	status, names, _, eof := readdir(t, s, root, 0, 1<<16)
	if status != _nfsOK || !slices.Equal(names, want) || !eof {
		t.Errorf("one call: %d, %v (eof %v), want %v without . and ..", status, names, eof, want)
	}

	var all []string
	cookie := uint64(0)
	for calls := 0; ; calls++ {
		if calls > 40 {
			t.Fatal("paging never reached eof")
		}
		status, names, cookies, eof := readdir(t, s, root, cookie, 300)
		if status != _nfsOK || len(names) == 0 && !eof {
			t.Fatalf("page after cookie %d: %d, %v", cookie, status, names)
		}
		for _, c := range cookies {
			if c <= 2 {
				t.Errorf("cookie %d: 0, 1 and 2 are reserved", c)
			}
		}
		all = append(all, names...)
		if eof {
			break
		}
		cookie = cookies[len(cookies)-1]
	}
	if !slices.Equal(all, want) {
		t.Errorf("paged: %v, want %v", all, want)
	}
	if status, _, _, _ := readdir(t, s, root, 0, 20); status != _nfs4errTooSmall {
		t.Errorf("maxcount 20: %d, want TOOSMALL", status)
	}
	if status, _, _, _ := readdir(t, s, lookupHandle(t, s, "README.md"), 0, 1<<16); status != _nfs4errNotDir {
		t.Errorf("READDIR a file: %d, want NOTDIR", status)
	}
}

func TestNFSReadLink(t *testing.T) {
	t.Parallel()
	dir := testFolder(t)
	if err := os.Symlink("/Users/someone/.ssh", filepath.Join(dir, "keys")); err != nil {
		t.Fatal(err)
	}
	s := newNFSServer(dir, bytes.Repeat([]byte{1}, 32))

	var c compoundArgs
	c.add(_opPutFH).opaque(lookupHandle(t, s, "keys"))
	c.add(_opReadLink)
	status, r := compound(t, s, 0, &c)
	result(t, r, _opPutFH)
	if status != _nfsOK || result(t, r, _opReadLink) != _nfsOK || r.str(1024) != "/Users/someone/.ssh" {
		t.Errorf("READLINK: %d, want the link's own text", status)
	}
	if status := fhStatus(t, s, lookupHandle(t, s, "README.md"), func(c *compoundArgs) { c.add(_opReadLink) }); status != _nfs4errInval {
		t.Errorf("READLINK a file: %d, want INVAL", status)
	}
}

// open adds an OPEN of name for reading, no create.
func openRead(c *compoundArgs, name string) {
	w := c.add(_opOpen)
	w.u32(1) // seqid
	w.u32(1) // share_access READ
	w.u32(0) // share_deny NONE
	w.u64(7) // clientid
	w.opaque([]byte("owner"))
	w.u32(0) // NOCREATE
	w.u32(0) // CLAIM_NULL
	w.str(name)
}

func TestNFSOpenRead(t *testing.T) {
	t.Parallel()
	dir := testFolder(t)
	if err := os.Mkdir(filepath.Join(dir, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("README.md", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	s := newNFSServer(dir, bytes.Repeat([]byte{1}, 32))
	root := lookupHandle(t, s)

	var c compoundArgs
	c.add(_opPutFH).opaque(root)
	openRead(&c, "README.md")
	c.add(_opGetFH)
	status, r := compound(t, s, 0, &c)
	result(t, r, _opPutFH)
	if status != _nfsOK || result(t, r, _opOpen) != _nfsOK {
		t.Fatalf("OPEN: %d", status)
	}
	seqid, other := r.u32(), r.take(12)
	r.u32()
	r.u64()
	r.u64() // change_info
	if rflags := r.u32(); rflags&_openResultConfirm != 0 {
		t.Error("OPEN asks for OPEN_CONFIRM")
	}
	r.bitmap()
	if deleg := r.u32(); deleg != 0 {
		t.Errorf("delegation %d, want none", deleg)
	}
	result(t, r, _opGetFH)
	if fh := r.opaque(128); !bytes.Equal(fh, lookupHandle(t, s, "README.md")) {
		t.Error("the current handle after OPEN isn't the file")
	}

	read := func(offset uint64, count uint32) (uint32, bool, string) {
		var c compoundArgs
		c.add(_opPutFH).opaque(lookupHandle(t, s, "README.md"))
		w := c.add(_opRead)
		w.u32(seqid)
		w.b = append(w.b, other...)
		w.u64(offset)
		w.u32(count)
		status, r := compound(t, s, 0, &c)
		result(t, r, _opPutFH)
		if result(t, r, _opRead); status != _nfsOK {
			return status, false, ""
		}
		eof := r.u32() == 1
		return status, eof, string(r.opaque(1 << 20))
	}
	if status, eof, data := read(0, 1024); status != _nfsOK || !eof || data != "hello\n" {
		t.Errorf("READ all: %d %v %q", status, eof, data)
	}
	if status, eof, data := read(1, 3); status != _nfsOK || eof || data != "ell" {
		t.Errorf("READ 1..4: %d %v %q", status, eof, data)
	}
	if status, eof, data := read(100, 3); status != _nfsOK || !eof || data != "" {
		t.Errorf("READ past the end: %d %v %q", status, eof, data)
	}

	c = compoundArgs{}
	c.add(_opPutFH).opaque(lookupHandle(t, s, "README.md"))
	w := c.add(_opClose)
	w.u32(2)
	w.u32(seqid)
	w.b = append(w.b, other...)
	if status, _ := compound(t, s, 0, &c); status != _nfsOK {
		t.Errorf("CLOSE: %d", status)
	}

	for _, tt := range []struct {
		name string
		want uint32
	}{{"app", _nfs4errIsDir}, {"link", _nfs4errSymlink}, {"missing", _nfs4errNoEnt}} {
		c := compoundArgs{}
		c.add(_opPutFH).opaque(root)
		openRead(&c, tt.name)
		if status, _ := compound(t, s, 0, &c); status != tt.want {
			t.Errorf("OPEN %s: %d, want %d", tt.name, status, tt.want)
		}
	}
}

func TestNFSSavedHandleAndSecinfo(t *testing.T) {
	t.Parallel()
	s := newNFSServer(testFolder(t), bytes.Repeat([]byte{1}, 32))

	var c compoundArgs
	c.add(_opPutRootFH)
	c.add(_opSaveFH)
	c.add(_opLookup).str("README.md")
	c.add(_opRestoreFH)
	c.add(_opGetFH)
	status, r := compound(t, s, 0, &c)
	for _, op := range []uint32{_opPutRootFH, _opSaveFH, _opLookup, _opRestoreFH, _opGetFH} {
		result(t, r, op)
	}
	if fh := r.opaque(128); status != _nfsOK || !bytes.Equal(fh, lookupHandle(t, s)) {
		t.Errorf("SAVEFH, LOOKUP, RESTOREFH: %d, not back at the root", status)
	}

	c = compoundArgs{}
	c.add(_opPutRootFH)
	c.add(_opRestoreFH)
	if status, _ := compound(t, s, 0, &c); status != _nfs4errRestoreFH {
		t.Errorf("RESTOREFH with nothing saved: %d", status)
	}

	c = compoundArgs{}
	c.add(_opPutRootFH)
	c.add(_opSecInfo).str("README.md")
	status, r = compound(t, s, 0, &c)
	result(t, r, _opPutRootFH)
	if status != _nfsOK || result(t, r, _opSecInfo) != _nfsOK || r.u32() != 1 || r.u32() != 1 {
		t.Errorf("SECINFO: %d, want one flavor, AUTH_SYS", status)
	}
}

// TestNFSServeConn: calls arrive as RPC records, possibly in fragments, and
// each reply goes back as one record.
func TestNFSServeConn(t *testing.T) {
	t.Parallel()
	s := newNFSServer(testFolder(t), bytes.Repeat([]byte{1}, 32))
	null := func(xid uint32) []byte {
		var w xdrWriter
		for _, v := range []uint32{xid, 0, 2, _nfsProgram, 4, 0, 0, 0, 0, 0} {
			w.u32(v)
		}
		return w.b
	}
	var in bytes.Buffer
	in.Write(binary.BigEndian.AppendUint32(nil, 0x80000000|uint32(len(null(1)))))
	in.Write(null(1))
	second := null(2)
	in.Write(binary.BigEndian.AppendUint32(nil, 12)) // a first fragment
	in.Write(second[:12])
	in.Write(binary.BigEndian.AppendUint32(nil, 0x80000000|uint32(len(second)-12)))
	in.Write(second[12:])

	var out bytes.Buffer
	if err := s.serveConn(&in, &out); err != nil {
		t.Fatalf("serveConn: %v", err)
	}
	for _, xid := range []uint32{1, 2} {
		r := &xdrReader{b: out.Bytes()}
		mark := r.u32()
		reply := r.take(int(mark &^ 0x80000000))
		if mark&0x80000000 == 0 || binary.BigEndian.Uint32(reply) != xid {
			t.Fatalf("reply %d: mark %x, xid %d", xid, mark, binary.BigEndian.Uint32(reply))
		}
		out.Next(4 + len(reply))
	}

	huge := bytes.NewBuffer(binary.BigEndian.AppendUint32(nil, 0x80000000|(_maxRecord+1)))
	if err := s.serveConn(huge, &out); err == nil {
		t.Error("a record over the limit was read")
	}
}

// modeAttrs is a fattr4 setting mode.
func modeAttrs(w *xdrWriter, mode uint32) {
	w.bitmap(attrMask(_attrMode))
	var v xdrWriter
	v.u32(mode)
	w.opaque(v.b)
}

// openWrite adds an OPEN of name for writing: how is -1 for no create,
// else UNCHECKED (0), GUARDED (1) or EXCLUSIVE (2, with verf).
func openWrite(c *compoundArgs, name string, how int, verf string) {
	w := c.add(_opOpen)
	w.u32(1)
	w.u32(3) // share_access BOTH
	w.u32(0)
	w.u64(7)
	w.opaque([]byte("owner"))
	if how < 0 {
		w.u32(0)
	} else {
		w.u32(1)
		w.u32(uint32(how))
		if how == 2 {
			w.b = append(w.b, verf...)
		} else {
			modeAttrs(w, 0o640)
		}
	}
	w.u32(0) // CLAIM_NULL
	w.str(name)
}

// write adds a WRITE of data at offset with an anonymous stateid.
func write(c *compoundArgs, offset uint64, data string, stable uint32) {
	w := c.add(_opWrite)
	w.b = append(w.b, make([]byte, 16)...)
	w.u64(offset)
	w.u32(stable)
	w.opaque([]byte(data))
}

// run is compound's status, failing the test on anything but want.
func runWant(t *testing.T, s *nfsServer, name string, want uint32, c *compoundArgs) *xdrReader {
	t.Helper()
	status, r := compound(t, s, 0, c)
	if status != want {
		t.Errorf("%s: %d, want %d", name, status, want)
	}
	return r
}

func TestNFSWrites(t *testing.T) {
	t.Parallel()
	dir := testFolder(t)
	if err := os.Mkdir(filepath.Join(dir, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := newNFSServer(dir, bytes.Repeat([]byte{1}, 32))
	root := lookupHandle(t, s)
	read := func(p string) string { b, _ := os.ReadFile(filepath.Join(dir, p)); return string(b) }

	var c compoundArgs
	c.add(_opPutFH).opaque(root)
	openWrite(&c, "new.txt", 0, "")
	write(&c, 0, "made in the guest\n", 2)
	r := runWant(t, s, "OPEN create, WRITE", _nfsOK, &c)
	if got := read("new.txt"); got != "made in the guest\n" {
		t.Errorf("new.txt on the Mac: %q", got)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "new.txt")); fi == nil || fi.Mode().Perm() != 0o640 {
		t.Errorf("new.txt mode %v, want the create's 0640", fi)
	}
	result(t, r, _opPutFH)
	result(t, r, _opOpen)
	r.take(16 + 4 + 16 + 4) // stateid, change_info, rflags
	if attrset := r.bitmap(); !hasAttr(attrset, _attrMode) {
		t.Error("OPEN's attrset lacks mode")
	}
	r.u32() // delegation
	result(t, r, _opWrite)
	if count, committed := r.u32(), r.u32(); count != 18 || committed != 2 {
		t.Errorf("WRITE: count %d, committed %d", count, committed)
	}
	verf := r.take(8)

	c = compoundArgs{}
	c.add(_opPutFH).opaque(lookupHandle(t, s, "new.txt"))
	c.add(_opCommit).u64(0)
	c.w.u32(0)
	r = runWant(t, s, "COMMIT", _nfsOK, &c)
	result(t, r, _opPutFH)
	if result(t, r, _opCommit); !bytes.Equal(r.take(8), verf) {
		t.Error("COMMIT's verifier isn't WRITE's")
	}

	c = compoundArgs{}
	c.add(_opPutFH).opaque(root)
	openWrite(&c, "new.txt", 1, "")
	runWant(t, s, "GUARDED on an existing file", _nfs4errExist, &c)
	for _, tt := range []struct {
		verf string
		want uint32
	}{{"exclusv1", _nfsOK}, {"exclusv1", _nfsOK}, {"exclusv2", _nfs4errExist}} {
		c = compoundArgs{}
		c.add(_opPutFH).opaque(root)
		openWrite(&c, "excl.txt", 2, tt.verf)
		runWant(t, s, "EXCLUSIVE "+tt.verf, tt.want, &c)
	}

	c = compoundArgs{}
	c.add(_opPutFH).opaque(lookupHandle(t, s, "new.txt"))
	w := c.add(_opSetAttr)
	w.b = append(w.b, make([]byte, 16)...)
	w.bitmap(attrMask(_attrSize, _attrMode))
	var v xdrWriter
	v.u64(4)
	v.u32(0o600)
	w.opaque(v.b)
	runWant(t, s, "SETATTR size and mode", _nfsOK, &c)
	if fi, _ := os.Stat(filepath.Join(dir, "new.txt")); read("new.txt") != "made" || fi.Mode().Perm() != 0o600 {
		t.Errorf("after SETATTR: %q, %v", read("new.txt"), fi.Mode())
	}

	c = compoundArgs{}
	c.add(_opPutFH).opaque(root)
	w = c.add(_opCreate)
	w.u32(_nf4Dir)
	w.str("made")
	modeAttrs(w, 0o750)
	c.add(_opPutFH).opaque(root)
	w = c.add(_opCreate)
	w.u32(_nf4Lnk)
	w.str("app/x")
	w.str("rel")
	modeAttrs(w, 0o777)
	runWant(t, s, "CREATE a directory and a relative symlink", _nfsOK, &c)
	if fi, err := os.Stat(filepath.Join(dir, "made")); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o750 {
		t.Errorf("made: %v %v", fi, err)
	}
	if target, err := os.Readlink(filepath.Join(dir, "rel")); err != nil || target != "app/x" {
		t.Errorf("rel -> %q, %v", target, err)
	}

	c = compoundArgs{}
	c.add(_opPutFH).opaque(root)
	c.add(_opSaveFH)
	c.add(_opPutFH).opaque(lookupHandle(t, s, "app"))
	w = c.add(_opRename)
	w.str("new.txt")
	w.str("moved.txt")
	runWant(t, s, "RENAME into app", _nfsOK, &c)
	if read("app/moved.txt") != "made" || read("new.txt") != "" {
		t.Errorf("after RENAME: app/moved.txt %q", read("app/moved.txt"))
	}

	c = compoundArgs{}
	c.add(_opPutFH).opaque(lookupHandle(t, s, "app", "moved.txt"))
	c.add(_opSaveFH)
	c.add(_opPutFH).opaque(root)
	c.add(_opLink).str("hard.txt")
	runWant(t, s, "LINK", _nfsOK, &c)
	if read("hard.txt") != "made" {
		t.Errorf("hard.txt %q", read("hard.txt"))
	}

	for _, name := range []string{"hard.txt", "made"} {
		c = compoundArgs{}
		c.add(_opPutFH).opaque(root)
		c.add(_opRemove).str(name)
		runWant(t, s, "REMOVE "+name, _nfsOK, &c)
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s still there: %v", name, err)
		}
	}
}

// TestNFSProtected: every way to change a protected path is refused, by
// path, case-insensitively, and the Mac's files stay as they were.
func TestNFSProtected(t *testing.T) {
	t.Parallel()
	dir := testFolder(t)
	for _, d := range []string{".git", ".git/hooks", "sub", "sub/.git", "config"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for p, body := range map[string]string{".git/config": "[core]\n", ".git/HEAD": "ref\n", "sub/.git/config": "[sub]\n",
		".vscode": "", "config/prod.env": "SECRET=1\n", "config/dev.env": "x\n"} {
		if p == ".vscode" {
			if err := os.Mkdir(filepath.Join(dir, p), 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := newNFSServer(dir, bytes.Repeat([]byte{1}, 32))
	s.protect = []string{"config/prod.env"}
	root := lookupHandle(t, s)
	git := lookupHandle(t, s, ".git")

	setattr := func(h []byte) func(c *compoundArgs) {
		return func(c *compoundArgs) {
			c.add(_opPutFH).opaque(h)
			w := c.add(_opSetAttr)
			w.b = append(w.b, make([]byte, 16)...)
			modeAttrs(w, 0o777)
		}
	}
	create := func(h []byte, typ uint32, name, link string) func(c *compoundArgs) {
		return func(c *compoundArgs) {
			c.add(_opPutFH).opaque(h)
			w := c.add(_opCreate)
			w.u32(typ)
			if typ == _nf4Lnk {
				w.str(link)
			}
			w.str(name)
			modeAttrs(w, 0o755)
		}
	}
	rename := func(from []byte, old string, to []byte, name string) func(c *compoundArgs) {
		return func(c *compoundArgs) {
			c.add(_opPutFH).opaque(from)
			c.add(_opSaveFH)
			c.add(_opPutFH).opaque(to)
			w := c.add(_opRename)
			w.str(old)
			w.str(name)
		}
	}
	for _, tt := range []struct {
		name  string
		build func(c *compoundArgs)
	}{
		{"open .git/config for writing", func(c *compoundArgs) { c.add(_opPutFH).opaque(git); openWrite(c, "config", -1, "") }},
		{"create a hook", func(c *compoundArgs) {
			c.add(_opPutFH).opaque(lookupHandle(t, s, ".git", "hooks"))
			openWrite(c, "pre-commit", 0, "")
		}},
		{"write .git/HEAD", func(c *compoundArgs) { c.add(_opPutFH).opaque(lookupHandle(t, s, ".git", "HEAD")); write(c, 0, "x", 2) }},
		{"chmod .git/HEAD", setattr(lookupHandle(t, s, ".git", "HEAD"))},
		{"remove .git/HEAD", func(c *compoundArgs) { c.add(_opPutFH).opaque(git); c.add(_opRemove).str("HEAD") }},
		{"rename .git away", rename(root, ".git", root, "git-old")},
		{"rename onto .git/HEAD", rename(root, "README.md", git, "HEAD")},
		{"create .ENVRC", func(c *compoundArgs) { c.add(_opPutFH).opaque(root); openWrite(c, ".ENVRC", 0, "") }},
		{"mkdir in .vscode", create(lookupHandle(t, s, ".vscode"), _nf4Dir, "x", "")},
		{"mkdir .Idea", create(root, _nf4Dir, ".Idea", "")},
		{"a nested repo's config", func(c *compoundArgs) {
			c.add(_opPutFH).opaque(lookupHandle(t, s, "sub", ".git"))
			openWrite(c, "config", -1, "")
		}},
		{"link .git/config elsewhere", func(c *compoundArgs) {
			c.add(_opPutFH).opaque(lookupHandle(t, s, ".git", "config"))
			c.add(_opSaveFH)
			c.add(_opPutFH).opaque(root)
			c.add(_opLink).str("notes")
		}},
		{"a protect path", func(c *compoundArgs) {
			c.add(_opPutFH).opaque(lookupHandle(t, s, "config"))
			openWrite(c, "prod.env", -1, "")
		}},
		{"a protect path, other case", func(c *compoundArgs) {
			c.add(_opPutFH).opaque(lookupHandle(t, s, "config"))
			openWrite(c, "PROD.env", -1, "")
		}},
		{"rename the folder holding a protect path", rename(root, "config", root, "cfg")},
		{"a symlink to an absolute path", create(root, _nf4Lnk, "zshrc", "/Users/you/.zshrc")},
		{"a symlink out of the folder", create(lookupHandle(t, s, "sub"), _nf4Lnk, "up", "../../x")},
		{"a symlink into .git", create(root, _nf4Lnk, "notes", "sub/../.git/hooks/pre-commit")},
	} {
		var c compoundArgs
		tt.build(&c)
		if status, _ := compound(t, s, 0, &c); status != _nfs4errAccess {
			t.Errorf("%s: %d, want ACCESS", tt.name, status)
		}
	}
	for p, want := range map[string]string{".git/config": "[core]\n", ".git/HEAD": "ref\n", "sub/.git/config": "[sub]\n", "config/prod.env": "SECRET=1\n"} {
		if got, _ := os.ReadFile(filepath.Join(dir, p)); string(got) != want {
			t.Errorf("%s changed: %q", p, got)
		}
	}
	for _, p := range []string{".git/hooks/pre-commit", "git-old", ".envrc", ".Idea", "notes", "zshrc", "sub/up", "cfg"} {
		if _, err := os.Lstat(filepath.Join(dir, p)); !os.IsNotExist(err) {
			t.Errorf("%s exists: %v", p, err)
		}
	}

	// Next to them, writes go through, and ACCESS tells the client first.
	var c compoundArgs
	c.add(_opPutFH).opaque(lookupHandle(t, s, "config"))
	openWrite(&c, "dev.env", -1, "")
	runWant(t, s, "config/dev.env, not protected", _nfsOK, &c)
	c = compoundArgs{}
	c.add(_opPutFH).opaque(git)
	c.add(_opAccess).u32(_accessRead | _accessModify | _accessExtend | _accessDelete | _accessLookup)
	r := runWant(t, s, "ACCESS .git", _nfsOK, &c)
	result(t, r, _opPutFH)
	result(t, r, _opAccess)
	if r.u32(); r.u32() != _accessRead|_accessLookup {
		t.Error("ACCESS on .git grants more than reading")
	}
}

// TestNFSIgnoresTheCallersUID: credentials claiming root change nothing:
// the server acts as the Mac user and refuses by path.
func TestNFSIgnoresTheCallersUID(t *testing.T) {
	t.Parallel()
	dir := testFolder(t)
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := newNFSServer(dir, bytes.Repeat([]byte{1}, 32))
	var args xdrWriter
	args.opaque(nil) // tag
	args.u32(0)
	args.u32(2)
	args.u32(_opPutRootFH)
	args.u32(_opOpen)
	args.u32(1)
	args.u32(3)
	args.u32(0)
	args.u64(7)
	args.opaque([]byte("owner"))
	args.u32(1)
	args.u32(0)
	modeAttrs(&args, 0o644)
	args.u32(0)
	args.str(".git")

	var root xdrWriter // AUTH_SYS: stamp, machine name, uid 0, gid 0, no groups
	root.u32(0)
	root.str("guest")
	root.u32(0)
	root.u32(0)
	root.u32(0)
	var w xdrWriter
	for _, v := range []uint32{9, 0, 2, _nfsProgram, 4, 1, 1} {
		w.u32(v)
	}
	w.opaque(root.b)
	w.u32(0)
	w.opaque(nil)
	w.b = append(w.b, args.b...)
	r := &xdrReader{b: s.call(w.b)}
	r.take(12)
	r.u32()
	r.opaque(400)
	if accept, status := r.u32(), r.u32(); accept != 0 || status != _nfs4errAccess {
		t.Errorf("OPEN .git as uid 0: accept %d, status %d, want ACCESS", accept, status)
	}
}

func TestNFSLocks(t *testing.T) {
	t.Parallel()
	dir := testFolder(t)
	s := newNFSServer(dir, bytes.Repeat([]byte{1}, 32))
	fh := lookupHandle(t, s, "README.md")

	lock := func(owner string, typ uint32, offset, length uint64) (uint32, []byte) {
		var c compoundArgs
		c.add(_opPutFH).opaque(fh)
		w := c.add(_opLock)
		w.u32(typ)
		w.u32(0) // not a reclaim
		w.u64(offset)
		w.u64(length)
		w.u32(1) // a new lock owner
		w.u32(1) // open seqid
		w.b = append(w.b, make([]byte, 16)...)
		w.u32(1) // lock seqid
		w.u64(7)
		w.opaque([]byte(owner))
		status, r := compound(t, s, 0, &c)
		result(t, r, _opPutFH)
		result(t, r, _opLock)
		if status != _nfsOK {
			return status, nil
		}
		return status, r.take(16)
	}
	test := func(owner string, offset, length uint64) uint32 {
		var c compoundArgs
		c.add(_opPutFH).opaque(fh)
		w := c.add(_opLockT)
		w.u32(2)
		w.u64(offset)
		w.u64(length)
		w.u64(7)
		w.opaque([]byte(owner))
		status, _ := compound(t, s, 0, &c)
		return status
	}
	unlock := func(stateid []byte, offset, length uint64) uint32 {
		var c compoundArgs
		c.add(_opPutFH).opaque(fh)
		w := c.add(_opLockU)
		w.u32(2)
		w.u32(2)
		w.b = append(w.b, stateid...)
		w.u64(offset)
		w.u64(length)
		status, _ := compound(t, s, 0, &c)
		return status
	}

	statusA, a := lock("a", 2, 0, 10)
	if statusA != _nfsOK {
		t.Fatalf("a's write lock: %d", statusA)
	}
	if status := test("b", 5, 1); status != _nfs4errDenied {
		t.Errorf("b tests 5..6 under a's lock: %d, want DENIED", status)
	}
	if status, _ := lock("b", 2, 5, ^uint64(0)); status != _nfs4errDenied {
		t.Errorf("b locks 5..end: %d, want DENIED", status)
	}
	if status, _ := lock("b", 1, 10, 10); status != _nfsOK {
		t.Errorf("b locks 10..20, next to a's: %d", status)
	}
	if status := test("a", 0, 10); status != _nfsOK {
		t.Errorf("a tests its own range: %d", status)
	}
	if status := unlock(a, 0, 10); status != _nfsOK {
		t.Errorf("a unlocks: %d", status)
	}
	if status, _ := lock("b", 2, 0, 10); status != _nfsOK {
		t.Errorf("b locks 0..10 after a let go: %d", status)
	}
}

// TestNFSProtectFile: a server given a protect file reads it again when it
// changes, so sbox.yaml's protect applies to a running server.
func TestNFSProtectFile(t *testing.T) {
	t.Parallel()
	dir := testFolder(t)
	for _, f := range []string{"a.env", "b.env"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	list := filepath.Join(t.TempDir(), "protect")
	if err := os.WriteFile(list, []byte("a.env\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newNFSServer(dir, bytes.Repeat([]byte{1}, 32))
	s.protectFile = list
	open := func(name string) uint32 {
		var c compoundArgs
		c.add(_opPutRootFH)
		openWrite(&c, name, -1, "")
		status, _ := compound(t, s, 0, &c)
		return status
	}
	if a, b := open("a.env"), open("b.env"); a != _nfs4errAccess || b != _nfsOK {
		t.Errorf("a.env %d, b.env %d; want a.env refused", a, b)
	}
	if err := os.WriteFile(list, []byte("b.env\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(list, time.Now().Add(time.Second), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if a, b := open("a.env"), open("b.env"); a != _nfsOK || b != _nfs4errAccess {
		t.Errorf("after the change: a.env %d, b.env %d; want b.env refused", a, b)
	}
}

// TestNFSProtectedAliases: names APFS takes for a protected one are refused
// like it. strings.ToLower leaves ſ alone; APFS folds it to s.
func TestNFSProtectedAliases(t *testing.T) {
	t.Parallel()
	dir := testFolder(t)
	for _, d := range []string{".vscode", ".husky", "config"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(dir, ".vscode", "tasks.json"), "{}\n")
	writeTestFile(t, filepath.Join(dir, "config", "aws.env"), "x\n")
	s := newNFSServer(dir, bytes.Repeat([]byte{1}, 32))
	s.protect = []string{"config/aws.env"}
	for _, tt := range []struct{ name, folder, file string }{
		{"long s", ".vſcode", "tasks.json"},
		{"long s in .husky", ".huſky", "pre-commit"},
		{"Kelvin sign", ".husKy", "pre-commit"},
		{"long s in a protect path", "config", "awſ.env"},
	} {
		var c compoundArgs
		c.add(_opPutRootFH)
		c.add(_opLookup).str(tt.folder)
		openWrite(&c, tt.file, 0, "")
		if status, _ := compound(t, s, 0, &c); status != _nfs4errAccess {
			t.Errorf("%s: %d, want ACCESS", tt.name, status)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(dir, ".vscode", "tasks.json")); string(got) != "{}\n" {
		t.Errorf("tasks.json: %q", got)
	}
	if _, err := os.Lstat(filepath.Join(dir, ".husky", "pre-commit")); !os.IsNotExist(err) {
		t.Errorf("a hook exists: %v", err)
	}
}

// TestNFSNoPlantedRepository: objects/, refs/, config and a HEAD file make
// a repository git on the Mac finds, and its config runs code at the next
// git status there. No file may be named HEAD; a folder may (components/Head).
func TestNFSNoPlantedRepository(t *testing.T) {
	t.Parallel()
	dir := testFolder(t)
	if err := os.Mkdir(filepath.Join(dir, "components"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "components", "ref"), "ref: refs/heads/main\n")
	writeTestFile(t, filepath.Join(dir, "HEAD"), "on the Mac\n")
	s := newNFSServer(dir, bytes.Repeat([]byte{1}, 32))
	root, components := lookupHandle(t, s), lookupHandle(t, s, "components")
	make := func(typ uint32, name, link string) func(c *compoundArgs) {
		return func(c *compoundArgs) {
			c.add(_opPutFH).opaque(components)
			w := c.add(_opCreate)
			w.u32(typ)
			if typ == _nf4Lnk {
				w.str(link)
			}
			w.str(name)
			modeAttrs(w, 0o755)
		}
	}
	for _, tt := range []struct {
		name  string
		build func(c *compoundArgs)
	}{
		{"open HEAD", func(c *compoundArgs) { c.add(_opPutFH).opaque(components); openWrite(c, "HEAD", 0, "") }},
		{"open head", func(c *compoundArgs) { c.add(_opPutFH).opaque(components); openWrite(c, "head", 0, "") }},
		{"a symlink HEAD", make(_nf4Lnk, "HEAD", "ref")},
		{"a FIFO HEAD", make(_nf4FIFO, "HEAD", "")},
		{"rename a file to HEAD", func(c *compoundArgs) {
			c.add(_opPutFH).opaque(components)
			c.add(_opSaveFH)
			w := c.add(_opRename)
			w.str("ref")
			w.str("HEAD")
		}},
		{"link a file to HEAD", func(c *compoundArgs) {
			c.add(_opPutFH).opaque(lookupHandle(t, s, "components", "ref"))
			c.add(_opSaveFH)
			c.add(_opPutFH).opaque(components)
			c.add(_opLink).str("HEAD")
		}},
		{"write the Mac's HEAD", func(c *compoundArgs) { c.add(_opPutFH).opaque(lookupHandle(t, s, "HEAD")); write(c, 0, "ref:", 2) }},
		{"open the Mac's HEAD for writing", func(c *compoundArgs) { c.add(_opPutFH).opaque(root); openWrite(c, "HEAD", -1, "") }},
	} {
		var c compoundArgs
		tt.build(&c)
		if status, _ := compound(t, s, 0, &c); status != _nfs4errAccess {
			t.Errorf("%s: %d, want ACCESS", tt.name, status)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "components")); len(entries) != 1 {
		t.Errorf("components holds %v, want only ref", entries)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "HEAD")); string(got) != "on the Mac\n" {
		t.Errorf("the Mac's HEAD: %q", got)
	}

	// A folder of that name, and files in it, are ordinary.
	var c compoundArgs
	make(_nf4Dir, "Head", "")(&c)
	runWant(t, s, "mkdir Head", _nfsOK, &c)
	c = compoundArgs{}
	c.add(_opPutFH).opaque(lookupHandle(t, s, "components", "Head"))
	openWrite(&c, "index.tsx", 0, "")
	runWant(t, s, "a file in Head", _nfsOK, &c)
	c = compoundArgs{}
	c.add(_opPutFH).opaque(components)
	c.add(_opSaveFH)
	w := c.add(_opRename)
	w.str("Head")
	w.str("HEAD")
	runWant(t, s, "rename the folder Head to HEAD", _nfsOK, &c)
}

// TestNFSSymlinksStayInside: however a symlink is built or moved, a Mac
// tool following it stays in the folder. Each target checked alone looks
// fine; up/.. is the folder's parent once up is a link to the folder.
func TestNFSSymlinksStayInside(t *testing.T) {
	t.Parallel()
	dir := testFolder(t)
	for _, d := range []string{"a", "a/b", "a/b/c", "low", "low/deep"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s := newNFSServer(dir, bytes.Repeat([]byte{1}, 32))
	link := func(in []byte, name, target string) uint32 {
		var c compoundArgs
		c.add(_opPutFH).opaque(in)
		w := c.add(_opCreate)
		w.u32(_nf4Lnk)
		w.str(target)
		w.str(name)
		modeAttrs(w, 0o755)
		status, _ := compound(t, s, 0, &c)
		return status
	}
	rename := func(from []byte, old string, to []byte, name string) uint32 {
		var c compoundArgs
		c.add(_opPutFH).opaque(from)
		c.add(_opSaveFH)
		c.add(_opPutFH).opaque(to)
		w := c.add(_opRename)
		w.str(old)
		w.str(name)
		status, _ := compound(t, s, 0, &c)
		return status
	}
	root, deep, c := lookupHandle(t, s), lookupHandle(t, s, "low", "deep"), lookupHandle(t, s, "a", "b", "c")

	for _, tt := range []struct {
		name, target string
		want         uint32
	}{
		{"up", "..", _nfsOK},
		{"past-a-link", "up/..", _nfs4errAccess},
		{"past-a-folder", "../up/../..", _nfs4errAccess},
		{"dotted", "./up/./..", _nfs4errAccess},
		{"inward", "../up", _nfsOK},
		{"plain", "../README.md", _nfsOK},
	} {
		if got := link(lookupHandle(t, s, "low"), tt.name, tt.target); got != tt.want {
			t.Errorf("low/%s -> %s: %d, want %d", tt.name, tt.target, got, tt.want)
		}
	}

	// Moving the link, or a folder holding it, closer to the root.
	if got := link(c, "far", "../../../README.md"); got != _nfsOK {
		t.Fatalf("a/b/c/far: %d", got)
	}
	if got := rename(c, "far", root, "far"); got != _nfs4errAccess {
		t.Errorf("moving far to the root: %d, want ACCESS", got)
	}
	if got := rename(lookupHandle(t, s, "a", "b"), "c", root, "c"); got != _nfs4errAccess {
		t.Errorf("moving the folder holding far to the root: %d, want ACCESS", got)
	}
	if got := rename(root, "a", root, "a2"); got != _nfsOK {
		t.Errorf("a move keeping its depth: %d", got)
	}
	if got := rename(lookupHandle(t, s, "a2", "b"), "c", deep, "c"); got != _nfsOK {
		t.Errorf("a move leaving the link inside: %d", got)
	}
}

// TestNFSProtectFileWithASpace: a protect path may hold a space.
func TestNFSProtectFileWithASpace(t *testing.T) {
	t.Parallel()
	dir := testFolder(t)
	writeTestFile(t, filepath.Join(dir, "my secrets.env"), "x\n")
	list := filepath.Join(t.TempDir(), "protect")
	writeTestFile(t, list, "my secrets.env\n")
	s := newNFSServer(dir, bytes.Repeat([]byte{1}, 32))
	s.protectFile = list
	var c compoundArgs
	c.add(_opPutRootFH)
	openWrite(&c, "my secrets.env", -1, "")
	if status, _ := compound(t, s, 0, &c); status != _nfs4errAccess {
		t.Errorf("my secrets.env: %d, want ACCESS", status)
	}
}
