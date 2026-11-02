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
