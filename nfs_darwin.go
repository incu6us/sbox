//go:build darwin

package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// An NFSv4.0 server for one folder, the project of a live session
// (docs/live-mount.md). It acts as the Mac user running sbox. File handles
// are the file's device and inode with a MAC, resolved back to a path with
// fsgetpath(2), so they survive renames and a restart of the server, and
// a handle it didn't mint is refused. Every path is checked to be inside
// the folder, and no symlink on the Mac is ever followed.

const (
	_nfsProgram = 100003

	_nfsOK                    = 0
	_nfs4errNoEnt             = 2
	_nfs4errIO                = 5
	_nfs4errAccess            = 13
	_nfs4errExist             = 17
	_nfs4errXDev              = 18
	_nfs4errNotDir            = 20
	_nfs4errIsDir             = 21
	_nfs4errInval             = 22
	_nfs4errNoSpc             = 28
	_nfs4errROFS              = 30
	_nfs4errNameTooLong       = 63
	_nfs4errNotEmpty          = 66
	_nfs4errStale             = 70
	_nfs4errBadHandle         = 10001
	_nfs4errBadCookie         = 10003
	_nfs4errNotSupp           = 10004
	_nfs4errTooSmall          = 10005
	_nfs4errServerFault       = 10006
	_nfs4errBadType           = 10007
	_nfs4errDenied            = 10010
	_nfs4errNoFileHandle      = 10020
	_nfs4errMinorVersMismatch = 10021
	_nfs4errStaleClientID     = 10022
	_nfs4errBadStateID        = 10025
	_nfs4errSymlink           = 10029
	_nfs4errRestoreFH         = 10030
	_nfs4errAttrNotSupp       = 10032
	_nfs4errBadXDR            = 10036
	_nfs4errBadName           = 10041
	_nfs4errOpIllegal         = 10044

	_opAccess             = 3
	_opClose              = 4
	_opCommit             = 5
	_opCreate             = 6
	_opDelegPurge         = 7
	_opDelegReturn        = 8
	_opGetAttr            = 9
	_opGetFH              = 10
	_opLink               = 11
	_opLock               = 12
	_opLockT              = 13
	_opLockU              = 14
	_opLookup             = 15
	_opLookupP            = 16
	_opOpen               = 18
	_opOpenConfirm        = 20
	_opOpenDowngrade      = 21
	_opPutFH              = 22
	_opPutPubFH           = 23
	_opPutRootFH          = 24
	_opRead               = 25
	_opReadDir            = 26
	_opReadLink           = 27
	_opRemove             = 28
	_opRename             = 29
	_opRenew              = 30
	_opRestoreFH          = 31
	_opSaveFH             = 32
	_opSecInfo            = 33
	_opSetAttr            = 34
	_opSetClientID        = 35
	_opSetClientIDConfirm = 36
	_opWrite              = 38
	_opReleaseLockOwner   = 39
	_opIllegal            = 10044

	_nf4Reg  = 1
	_nf4Dir  = 2
	_nf4Lnk  = 5
	_nf4FIFO = 7

	_accessRead    = 0x01
	_accessLookup  = 0x02
	_accessModify  = 0x04
	_accessExtend  = 0x08
	_accessDelete  = 0x10
	_accessExecute = 0x20

	_openResultConfirm       = 2
	_openResultLocktypePOSIX = 4

	_attrSupportedAttrs  = 0
	_attrType            = 1
	_attrFHExpireType    = 2
	_attrChange          = 3
	_attrSize            = 4
	_attrLinkSupport     = 5
	_attrSymlinkSupport  = 6
	_attrNamedAttr       = 7
	_attrFSID            = 8
	_attrUniqueHandles   = 9
	_attrLeaseTime       = 10
	_attrRdattrError     = 11
	_attrCanSetTime      = 15
	_attrCaseInsensitive = 16
	_attrCasePreserving  = 17
	_attrChownRestricted = 18
	_attrFileHandle      = 19
	_attrFileID          = 20
	_attrFilesAvail      = 21
	_attrFilesFree       = 22
	_attrFilesTotal      = 23
	_attrHomogeneous     = 26
	_attrMaxFileSize     = 27
	_attrMaxLink         = 28
	_attrMaxName         = 29
	_attrMaxRead         = 30
	_attrMaxWrite        = 31
	_attrMode            = 33
	_attrNoTrunc         = 34
	_attrNumLinks        = 35
	_attrOwner           = 36
	_attrOwnerGroup      = 37
	_attrRawDev          = 41
	_attrSpaceAvail      = 42
	_attrSpaceFree       = 43
	_attrSpaceTotal      = 44
	_attrSpaceUsed       = 45
	_attrTimeAccess      = 47
	_attrTimeAccessSet   = 48
	_attrTimeDelta       = 51
	_attrTimeMetadata    = 52
	_attrTimeModify      = 53
	_attrTimeModifySet   = 54
	_attrMountedOnFileID = 55

	// _guestOwner is who the guest sees owning every file: its runner.
	_guestOwner = "1000"
	_maxIO      = 1 << 20
	_leaseTime  = 90
)

// xdrReader decodes XDR. A short read sets err and returns zeros, so a
// decoder reads on and checks err once.
type xdrReader struct {
	b   []byte
	err bool
}

func (r *xdrReader) take(n int) []byte {
	if r.err || n < 0 || len(r.b) < n {
		r.err = true
		return make([]byte, max(n, 0))
	}
	p := r.b[:n]
	r.b = r.b[n:]
	return p
}

func (r *xdrReader) u32() uint32 { return binary.BigEndian.Uint32(r.take(4)) }
func (r *xdrReader) u64() uint64 { return binary.BigEndian.Uint64(r.take(8)) }

// opaque reads a variable-length opaque of at most limit bytes.
func (r *xdrReader) opaque(limit int) []byte {
	n := int(r.u32())
	if n > limit {
		r.err = true
		return nil
	}
	p := r.take(n)
	r.take((4 - n%4) % 4)
	return p
}

func (r *xdrReader) str(limit int) string { return string(r.opaque(limit)) }

func (r *xdrReader) bitmap() []uint32 {
	n := int(r.u32())
	if n > 8 {
		r.err = true
		return nil
	}
	words := make([]uint32, n)
	for i := range words {
		words[i] = r.u32()
	}
	return words
}

type xdrWriter struct{ b []byte }

func (w *xdrWriter) u32(v uint32) { w.b = binary.BigEndian.AppendUint32(w.b, v) }
func (w *xdrWriter) u64(v uint64) { w.b = binary.BigEndian.AppendUint64(w.b, v) }

func (w *xdrWriter) opaque(p []byte) {
	w.u32(uint32(len(p)))
	w.b = append(w.b, p...)
	w.b = append(w.b, make([]byte, (4-len(p)%4)%4)...)
}

func (w *xdrWriter) str(s string) { w.opaque([]byte(s)) }

func (w *xdrWriter) bitmap(words []uint32) {
	w.u32(uint32(len(words)))
	for _, v := range words {
		w.u32(v)
	}
}

// attrMask is the bitmap with the given attribute numbers set.
func attrMask(attrs ...int) []uint32 {
	var words []uint32
	for _, a := range attrs {
		for len(words) <= a/32 {
			words = append(words, 0)
		}
		words[a/32] |= 1 << (a % 32)
	}
	return words
}

func hasAttr(mask []uint32, a int) bool { return a/32 < len(mask) && mask[a/32]&(1<<(a%32)) != 0 }

// nfsServer serves root, which must be a real path (no symlinks in it).
type nfsServer struct {
	root string
	key  []byte // the handle MAC's key
	mu   sync.Mutex

	// Client ids and open stateids. ponytail: no seqid replay checks and no
	// share reservations; one client, the guest, and its own kernel orders
	// its calls.
	clients  map[uint64][8]byte // client id → its confirm verifier
	nextID   uint64
	stateids uint64

	// protect are paths from the root the guest may not change, besides
	// _protectedNames at any depth; read from protectFile, when set, again
	// whenever it changes.
	protect      []string
	protectFile  string
	protectStamp time.Time
	// hide are rules for what the guest can't see or change, folded; read
	// from hideFile as protect is from protectFile.
	hide      ignoreRules
	hideFile  string
	hideStamp time.Time
	// refused, when set, hears each refused change: the operation and the
	// path from the root.
	refused func(op, rel string)
	verf    [8]byte // WRITE's verifier: new each start, so the client resends unsynced writes

	locks      map[[2]uint64][]lockRange // by device and inode
	lockOwners map[uint64]lockOwner      // by the lock stateid's id
}

type lockOwner struct {
	client uint64
	owner  string
}

type lockRange struct {
	owner      lockOwner
	start, end uint64 // end exclusive; ^0 to the end of the file
	write      bool
}

func newNFSServer(root string, key []byte) *nfsServer {
	s := &nfsServer{root: root, key: key, clients: map[uint64][8]byte{}, nextID: uint64(time.Now().Unix()) << 32,
		locks: map[[2]uint64][]lockRange{}, lockOwners: map[uint64]lockOwner{}}
	_, _ = rand.Read(s.verf[:])
	return s
}

// reloadProtect reads protectFile again when it changed. A file that
// can't be read keeps the last list.
func (s *nfsServer) reloadProtect() {
	if list, ok := reloadList(s.protectFile, &s.protectStamp); ok {
		s.protect = list
	}
	if list, ok := reloadList(s.hideFile, &s.hideStamp); ok {
		for i := range list {
			list[i] = foldName(list[i])
		}
		s.hide = list
	}
}

// reloadList reads file's lines when it changed since stamp. A file that
// can't be read keeps the last list.
func reloadList(file string, stamp *time.Time) ([]string, bool) {
	if file == "" {
		return nil, false
	}
	fi, err := os.Stat(file)
	if err != nil || fi.ModTime().Equal(*stamp) {
		return nil, false
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, false
	}
	*stamp = fi.ModTime()
	return slices.DeleteFunc(strings.Split(string(data), "\n"), func(q string) bool { return q == "" }), true
}

// rel is p from the root, "" for the root itself.
func (s *nfsServer) rel(p string) string {
	if p == s.root {
		return ""
	}
	return strings.TrimPrefix(p, s.root+"/")
}

// foldName is name as hide's rules compare it, the case folded as APFS does.
func foldName(name string) string { return strings.ReplaceAll(strings.ToLower(name), "\u017f", "s") }

// hideMatches reports whether a hide rule matches rel or a folder it is in.
func (s *nfsServer) hideMatches(rel string) bool {
	for rel = foldName(rel); rel != "." && rel != "/"; rel = path.Dir(rel) {
		if s.hide.skip(rel) {
			return true
		}
	}
	return false
}

// hidden reports whether the guest may not see p. A path is looked at as
// asked for and as the file system names it, so any spelling APFS takes
// for a hidden file (the other form of an é) is that file. ponytail: a hard
// link the Mac made under another name is that name's.
func (s *nfsServer) hidden(p string) bool {
	if len(s.hide) == 0 || p == s.root {
		return false
	}
	if s.hideMatches(s.rel(p)) {
		return true
	}
	f, _, err := lstatHandle(p)
	if err != nil {
		return false
	}
	real, err := fsgetpath(f.dev, f.ino)
	return err == nil && s.hideMatches(s.rel(real))
}

// missing answers the guest's looking for the hidden path p, which is there
// or not: the same to the guest, and only one that's there is reported.
func (s *nfsServer) missing(p string) uint32 {
	var sys syscall.Stat_t
	if s.refused != nil && syscall.Lstat(p, &sys) == nil {
		s.refused("hide", s.rel(p))
	}
	return _nfs4errNoEnt
}

// unhides reports whether moving src to dst would bring a hidden path at
// or under src into view. ponytail: walks the folder under the one lock,
// as linksStayInside does.
func (s *nfsServer) unhides(src, dst string) bool {
	if len(s.hide) == 0 {
		return false
	}
	found := false
	_ = filepath.WalkDir(src, func(p string, _ fs.DirEntry, err error) error {
		if err == nil && s.hidden(p) && !s.hidden(dst+strings.TrimPrefix(p, src)) {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// protected reports whether p may not be changed: a hidden path, a
// protected name at any depth, a protect path, or anything under them. Names compare as APFS
// does, ſ and K included. ponytail: EqualFold, no Unicode normalisation; a
// non-ASCII protect path must be written as the file's own name is.
func (s *nfsServer) protected(p string) bool {
	rel := s.rel(p)
	if rel == "" {
		return false
	}
	if s.hidden(p) {
		return true
	}
	parts := strings.Split(rel, "/")
	for _, c := range parts {
		if slices.ContainsFunc(_protectedNames, func(n string) bool { return strings.EqualFold(c, n) }) {
			return true
		}
	}
	return slices.ContainsFunc(s.protect, func(q string) bool { return startsWithFold(parts, strings.Split(q, "/")) })
}

// protectedFile is protected, or p is, or would be, a file named HEAD: with
// objects/ and refs/ beside it, that is a repository git on the Mac finds,
// and its config runs code at the next git status there. A folder of that
// name is ordinary, and git doesn't take one for a HEAD.
func (s *nfsServer) protectedFile(p string) bool {
	if s.protected(p) {
		return true
	}
	if !strings.EqualFold(filepath.Base(p), "HEAD") {
		return false
	}
	var sys syscall.Stat_t
	return syscall.Lstat(p, &sys) != nil || fileType(sys.Mode) != _nf4Dir
}

// startsWithFold reports whether parts begins with prefix's components.
func startsWithFold(parts, prefix []string) bool {
	if len(prefix) > len(parts) {
		return false
	}
	for i, c := range prefix {
		if !strings.EqualFold(c, parts[i]) {
			return false
		}
	}
	return true
}

// holdsProtected reports whether a protect path lies under the folder p:
// moving or removing p would move it out of reach.
func (s *nfsServer) holdsProtected(p string) bool {
	rel := s.rel(p)
	parts := strings.Split(rel, "/")
	return slices.ContainsFunc(s.protect, func(q string) bool {
		path := strings.Split(q, "/")
		return rel == "" || len(path) > len(parts) && startsWithFold(path, parts)
	})
}

// refuse answers a refused change to p.
func (s *nfsServer) refuse(op, p string) uint32 {
	if s.refused != nil {
		if s.hidden(p) {
			op = "hide"
		}
		s.refused(op, s.rel(p))
	}
	return _nfs4errAccess
}

// symlinkTarget reports whether a new symlink in dir may point at target:
// relative, inside the folder, and not into a protected path. Tools on
// the Mac follow symlinks even though this server doesn't. A .. is only
// allowed first: after a name it is lexically harmless but, when that name
// is a link (now or later), it climbs from wherever the link led.
func (s *nfsServer) symlinkTarget(dir, target string) bool {
	if target == "" || filepath.IsAbs(target) {
		return false
	}
	named := false
	for _, c := range strings.Split(target, "/") {
		switch c {
		case "", ".":
		case "..":
			if named {
				return false
			}
		default:
			named = true
		}
	}
	p := filepath.Join(dir, target)
	return (p == s.root || strings.HasPrefix(p, s.root+"/")) && !s.protected(p)
}

// linksStayInside reports whether every relative symlink at or under src
// still passes symlinkTarget once src is at dst. Links the Mac made with
// absolute targets aren't the guest's and don't stop a move. ponytail:
// walks the moved folder under the server's one lock; moves of big folders
// are rare, and node_modules is on the guest's disk.
func (s *nfsServer) linksStayInside(src, dst string) bool {
	inside := true
	_ = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		target, err := os.Readlink(p)
		if err == nil && !filepath.IsAbs(target) && !s.symlinkTarget(filepath.Dir(dst+strings.TrimPrefix(p, src)), target) {
			inside = false
			return filepath.SkipAll
		}
		return nil
	})
	return inside
}

// fileHandle is a resolved handle: the file's identity and where it is now.
type fileHandle struct {
	dev  int32
	ino  uint64
	path string
}

const _handleLen = 1 + 4 + 8 + 16

func (s *nfsServer) mac(id []byte) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write(id)
	return m.Sum(nil)[:16]
}

// handle mints the wire handle for f.
func (s *nfsServer) handle(f fileHandle) []byte {
	id := make([]byte, 0, _handleLen)
	id = append(id, 1)
	id = binary.BigEndian.AppendUint32(id, uint32(f.dev))
	id = binary.BigEndian.AppendUint64(id, f.ino)
	return append(id, s.mac(id)...)
}

// resolve turns a wire handle back into a file inside root: BADHANDLE for
// one sbox didn't mint, STALE for a file gone or no longer inside.
func (s *nfsServer) resolve(h []byte) (fileHandle, uint32) {
	if len(h) != _handleLen || h[0] != 1 || !hmac.Equal(h[13:], s.mac(h[:13])) {
		return fileHandle{}, _nfs4errBadHandle
	}
	f := fileHandle{dev: int32(binary.BigEndian.Uint32(h[1:5])), ino: binary.BigEndian.Uint64(h[5:13])}
	p, err := fsgetpath(f.dev, f.ino)
	if err != nil || (p != s.root && !strings.HasPrefix(p, s.root+"/")) || s.hidden(p) {
		return fileHandle{}, _nfs4errStale
	}
	f.path = p
	return f, _nfsOK
}

// fsgetpath is macOS's fsgetpath(2): the path of the file (dev, ino).
func fsgetpath(dev int32, ino uint64) (string, error) {
	const sysFsgetpath = 427
	fsid := [2]int32{dev, 0}
	buf := make([]byte, 1024)
	n, _, errno := syscall.Syscall6(sysFsgetpath, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)),
		uintptr(unsafe.Pointer(&fsid)), uintptr(ino), 0, 0)
	if errno != 0 {
		return "", errno
	}
	return string(buf[:max(int(n)-1, 0)]), nil
}

// lstatHandle is the handle of the file at p, not following a symlink.
func lstatHandle(p string) (fileHandle, syscall.Stat_t, error) {
	var st syscall.Stat_t
	if err := syscall.Lstat(p, &st); err != nil {
		return fileHandle{}, st, err
	}
	return fileHandle{dev: st.Dev, ino: st.Ino, path: p}, st, nil
}

// _maxRecord caps one RPC record: a WRITE of _maxIO and its COMPOUND.
const _maxRecord = _maxIO + 64<<10

// serveConn answers RPC records from in on out until in ends (nil) or
// sends something malformed (an error). A record is fragments, each with a
// 4-byte mark: the last-fragment bit and the length.
func (s *nfsServer) serveConn(in io.Reader, out io.Writer) error {
	var mark [4]byte
	for {
		var msg []byte
		for last := false; !last; {
			if _, err := io.ReadFull(in, mark[:]); err != nil {
				if errors.Is(err, io.EOF) && len(msg) == 0 {
					return nil
				}
				return err
			}
			v := binary.BigEndian.Uint32(mark[:])
			last = v&0x80000000 != 0
			n := int(v &^ 0x80000000)
			if len(msg)+n > _maxRecord {
				return fmt.Errorf("nfs: a record over %d bytes", _maxRecord)
			}
			msg = append(msg, make([]byte, n)...)
			if _, err := io.ReadFull(in, msg[len(msg)-n:]); err != nil {
				return err
			}
		}
		reply := s.call(msg)
		if _, err := out.Write(append(binary.BigEndian.AppendUint32(nil, 0x80000000|uint32(len(reply))), reply...)); err != nil {
			return err
		}
	}
}

// call answers one RPC call message with the reply message.
func (s *nfsServer) call(msg []byte) []byte {
	r := &xdrReader{b: msg}
	xid, msgType, rpcVers, prog, vers, proc := r.u32(), r.u32(), r.u32(), r.u32(), r.u32(), r.u32()
	r.u32() // credential flavor: the server acts as the Mac user whatever it says
	r.opaque(400)
	r.u32() // verifier
	r.opaque(400)

	var w xdrWriter
	w.u32(xid)
	w.u32(1) // REPLY
	if r.err || msgType != 0 || rpcVers != 2 {
		w.u32(1) // MSG_DENIED
		w.u32(0) // RPC_MISMATCH
		w.u32(2)
		w.u32(2)
		return w.b
	}
	w.u32(0) // MSG_ACCEPTED
	w.u32(0) // verifier AUTH_NONE
	w.opaque(nil)
	switch {
	case prog != _nfsProgram:
		w.u32(1) // PROG_UNAVAIL
	case vers != 4:
		w.u32(2) // PROG_MISMATCH
		w.u32(4)
		w.u32(4)
	case proc == 0: // NULL
		w.u32(0)
	case proc == 1:
		w.u32(0)
		s.compound(r, &w)
	default:
		w.u32(3) // PROC_UNAVAIL
	}
	return w.b
}

// compoundState is what the operations of one COMPOUND share.
type compoundState struct {
	cur, saved *fileHandle
}

// dir is the current handle when it is a directory.
func (st *compoundState) dir() (*fileHandle, uint32) {
	if st.cur == nil {
		return nil, _nfs4errNoFileHandle
	}
	var sys syscall.Stat_t
	if err := syscall.Lstat(st.cur.path, &sys); err != nil {
		return nil, errnoStatus(err)
	}
	switch fileType(sys.Mode) {
	case _nf4Dir:
		return st.cur, _nfsOK
	case _nf4Lnk:
		return nil, _nfs4errSymlink
	}
	return nil, _nfs4errNotDir
}

// checkName is a component's status: one name, not a path, not . or ..
func checkName(name string) uint32 {
	switch {
	case name == "":
		return _nfs4errInval
	case len(name) > 255:
		return _nfs4errNameTooLong
	case name == "." || name == ".." || strings.ContainsAny(name, "/\x00"):
		return _nfs4errBadName
	}
	return _nfsOK
}

func errnoStatus(err error) uint32 {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return _nfs4errIO
	}
	switch errno {
	case syscall.ENOENT:
		return _nfs4errNoEnt
	case syscall.EACCES, syscall.EPERM:
		return _nfs4errAccess
	case syscall.EEXIST:
		return _nfs4errExist
	case syscall.EXDEV:
		return _nfs4errXDev
	case syscall.ENOTDIR:
		return _nfs4errNotDir
	case syscall.EISDIR:
		return _nfs4errIsDir
	case syscall.EINVAL:
		return _nfs4errInval
	case syscall.ENOSPC:
		return _nfs4errNoSpc
	case syscall.EROFS:
		return _nfs4errROFS
	case syscall.ENAMETOOLONG:
		return _nfs4errNameTooLong
	case syscall.ENOTEMPTY:
		return _nfs4errNotEmpty
	case syscall.ELOOP:
		return _nfs4errSymlink
	}
	return _nfs4errIO
}

func (s *nfsServer) compound(r *xdrReader, w *xdrWriter) {
	tag := r.opaque(1024)
	minor := r.u32()
	n := r.u32()
	if r.err {
		w.u32(_nfs4errBadXDR)
		w.opaque(tag)
		w.u32(0)
		return
	}
	if minor != 0 {
		w.u32(_nfs4errMinorVersMismatch)
		w.opaque(tag)
		w.u32(0)
		return
	}

	s.mu.Lock() // ponytail: one lock for every call; per-file locks if one client's parallel calls ever queue
	defer s.mu.Unlock()
	s.reloadProtect()
	var res xdrWriter
	var st compoundState
	status, count := uint32(_nfsOK), uint32(0)
	for i := uint32(0); i < n && status == _nfsOK; i++ {
		op := r.u32()
		if r.err {
			status = _nfs4errBadXDR
			break
		}
		count++
		res.u32(op)
		// An op sbox doesn't know answers as ILLEGAL, nothing written yet.
		if status = s.op(op, r, &res, &st); status == _nfs4errOpIllegal {
			res.b = res.b[:len(res.b)-4]
			res.u32(_opIllegal)
			res.u32(_nfs4errOpIllegal)
		}
	}
	w.u32(status)
	w.opaque(tag)
	w.u32(count)
	w.b = append(w.b, res.b...)
}

// op runs one operation: it writes its status and, when it succeeds, its
// results, and returns the status.
func (s *nfsServer) op(op uint32, r *xdrReader, w *xdrWriter, st *compoundState) uint32 {
	fail := func(status uint32) uint32 {
		w.u32(status)
		return status
	}
	switch op {
	case _opPutRootFH, _opPutPubFH:
		f, _, err := lstatHandle(s.root)
		if err != nil {
			return fail(_nfs4errServerFault)
		}
		st.cur = &f
		return fail(_nfsOK)
	case _opPutFH:
		h := r.opaque(128)
		if r.err {
			return fail(_nfs4errBadXDR)
		}
		f, status := s.resolve(h)
		if status != _nfsOK {
			return fail(status)
		}
		st.cur = &f
		return fail(_nfsOK)
	case _opLookup:
		name := r.str(1024)
		if r.err {
			return fail(_nfs4errBadXDR)
		}
		dir, status := st.dir()
		if status == _nfsOK {
			status = checkName(name)
		}
		if status != _nfsOK {
			return fail(status)
		}
		if p := dir.path + "/" + name; s.hidden(p) {
			return fail(s.missing(p))
		}
		f, _, err := lstatHandle(dir.path + "/" + name)
		if err != nil {
			return fail(errnoStatus(err))
		}
		st.cur = &f
		return fail(_nfsOK)
	case _opLookupP:
		dir, status := st.dir()
		if status != _nfsOK {
			return fail(status)
		}
		if dir.path == s.root || s.hidden(filepath.Dir(dir.path)) {
			return fail(_nfs4errNoEnt)
		}
		f, _, err := lstatHandle(filepath.Dir(dir.path))
		if err != nil {
			return fail(errnoStatus(err))
		}
		st.cur = &f
		return fail(_nfsOK)
	case _opGetFH:
		if st.cur == nil {
			return fail(_nfs4errNoFileHandle)
		}
		w.u32(_nfsOK)
		w.opaque(s.handle(*st.cur))
		return _nfsOK
	case _opGetAttr:
		mask := r.bitmap()
		if r.err {
			return fail(_nfs4errBadXDR)
		}
		if st.cur == nil {
			return fail(_nfs4errNoFileHandle)
		}
		var sys syscall.Stat_t
		if err := syscall.Lstat(st.cur.path, &sys); err != nil {
			return fail(_nfs4errStale)
		}
		w.u32(_nfsOK)
		s.attrs(w, mask, *st.cur, &sys)
		return _nfsOK
	case _opSaveFH:
		if st.cur == nil {
			return fail(_nfs4errNoFileHandle)
		}
		st.saved = st.cur
		return fail(_nfsOK)
	case _opRestoreFH:
		if st.saved == nil {
			return fail(_nfs4errRestoreFH)
		}
		st.cur = st.saved
		return fail(_nfsOK)
	case _opAccess:
		ask := r.u32()
		if r.err {
			return fail(_nfs4errBadXDR)
		}
		if st.cur == nil {
			return fail(_nfs4errNoFileHandle)
		}
		w.u32(_nfsOK)
		w.u32(ask & (_accessRead | _accessLookup | _accessModify | _accessExtend | _accessDelete | _accessExecute))
		got := access(st.cur.path, ask)
		if s.protectedFile(st.cur.path) {
			got &^= _accessModify | _accessExtend | _accessDelete
		}
		w.u32(got)
		return _nfsOK
	case _opReadLink:
		if st.cur == nil {
			return fail(_nfs4errNoFileHandle)
		}
		var sys syscall.Stat_t
		if err := syscall.Lstat(st.cur.path, &sys); err != nil {
			return fail(errnoStatus(err))
		}
		if fileType(sys.Mode) != _nf4Lnk {
			return fail(_nfs4errInval)
		}
		target, err := os.Readlink(st.cur.path)
		if err != nil {
			return fail(errnoStatus(err))
		}
		w.u32(_nfsOK)
		w.str(target)
		return _nfsOK
	case _opReadDir:
		return s.readdir(r, w, st)
	case _opOpen:
		return s.open(r, w, st)
	case _opRead:
		return s.read(r, w, st)
	case _opClose:
		r.u32() // seqid
		seqid, other := r.u32(), r.take(12)
		if r.err {
			return fail(_nfs4errBadXDR)
		}
		w.u32(_nfsOK)
		w.u32(seqid + 1)
		w.b = append(w.b, other...)
		return _nfsOK
	case _opSecInfo:
		name := r.str(1024)
		if r.err {
			return fail(_nfs4errBadXDR)
		}
		dir, status := st.dir()
		if status == _nfsOK {
			status = checkName(name)
		}
		if status != _nfsOK {
			return fail(status)
		}
		if p := dir.path + "/" + name; s.hidden(p) {
			return fail(s.missing(p))
		}
		if _, _, err := lstatHandle(dir.path + "/" + name); err != nil {
			return fail(errnoStatus(err))
		}
		st.cur = nil // SECINFO consumes the current handle
		w.u32(_nfsOK)
		w.u32(1)
		w.u32(1) // AUTH_SYS
		return _nfsOK
	case _opSetClientID:
		r.take(8) // the client's boot verifier
		r.opaque(1024)
		r.u32()
		r.str(64)
		r.str(64)
		r.u32()
		if r.err {
			return fail(_nfs4errBadXDR)
		}
		var confirm [8]byte
		_, _ = rand.Read(confirm[:])
		s.nextID++
		s.clients[s.nextID] = confirm
		w.u32(_nfsOK)
		w.u64(s.nextID)
		w.b = append(w.b, confirm[:]...)
		return _nfsOK
	case _opSetClientIDConfirm:
		id, verf := r.u64(), r.take(8)
		if r.err {
			return fail(_nfs4errBadXDR)
		}
		if confirm, ok := s.clients[id]; !ok || !bytes.Equal(confirm[:], verf) {
			return fail(_nfs4errStaleClientID)
		}
		return fail(_nfsOK)
	case _opRenew:
		id := r.u64()
		if r.err {
			return fail(_nfs4errBadXDR)
		}
		if _, ok := s.clients[id]; !ok {
			return fail(_nfs4errStaleClientID)
		}
		return fail(_nfsOK)
	case _opWrite:
		return s.write(r, w, st)
	case _opCommit:
		r.u64()
		r.u32()
		if r.err {
			return fail(_nfs4errBadXDR)
		}
		if st.cur == nil {
			return fail(_nfs4errNoFileHandle)
		}
		f, err := os.OpenFile(st.cur.path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return fail(errnoStatus(err))
		}
		err = f.Sync()
		_ = f.Close()
		if err != nil {
			return fail(errnoStatus(err))
		}
		w.u32(_nfsOK)
		w.b = append(w.b, s.verf[:]...)
		return _nfsOK
	case _opCreate:
		return s.create(r, w, st)
	case _opRemove:
		name := r.str(1024)
		if r.err {
			return fail(_nfs4errBadXDR)
		}
		dir, status := st.dir()
		if status == _nfsOK {
			status = checkName(name)
		}
		if status != _nfsOK {
			return fail(status)
		}
		p := dir.path + "/" + name
		if s.protected(p) || s.holdsProtected(p) {
			return fail(s.refuse("remove", p))
		}
		before := dirChange(dir.path)
		var sys syscall.Stat_t
		err := syscall.Lstat(p, &sys)
		if err == nil && fileType(sys.Mode) == _nf4Dir {
			err = syscall.Rmdir(p)
		} else if err == nil {
			err = syscall.Unlink(p)
		}
		if err != nil {
			return fail(errnoStatus(err))
		}
		w.u32(_nfsOK)
		changeInfo(w, before, dirChange(dir.path))
		return _nfsOK
	case _opRename:
		oldName, newName := r.str(1024), r.str(1024)
		if r.err {
			return fail(_nfs4errBadXDR)
		}
		to, status := st.dir()
		if status == _nfsOK && st.saved == nil {
			status = _nfs4errNoFileHandle
		}
		var from *fileHandle
		if status == _nfsOK {
			from, status = (&compoundState{cur: st.saved}).dir()
		}
		for _, n := range []string{oldName, newName} {
			if status == _nfsOK {
				status = checkName(n)
			}
		}
		if status != _nfsOK {
			return fail(status)
		}
		src, dst := from.path+"/"+oldName, to.path+"/"+newName
		for _, p := range []string{src, dst} {
			if s.protected(p) || s.holdsProtected(p) {
				return fail(s.refuse("rename", p))
			}
		}
		var moved syscall.Stat_t
		if err := syscall.Lstat(src, &moved); err == nil && fileType(moved.Mode) != _nf4Dir && s.protectedFile(dst) {
			return fail(s.refuse("rename", dst))
		}
		if !s.linksStayInside(src, dst) {
			return fail(s.refuse("rename", dst))
		}
		if s.unhides(src, dst) {
			return fail(s.refuse("rename", src))
		}
		srcBefore, dstBefore := dirChange(from.path), dirChange(to.path)
		if err := os.Rename(src, dst); err != nil {
			return fail(errnoStatus(err))
		}
		w.u32(_nfsOK)
		changeInfo(w, srcBefore, dirChange(from.path))
		changeInfo(w, dstBefore, dirChange(to.path))
		return _nfsOK
	case _opLink:
		name := r.str(1024)
		if r.err {
			return fail(_nfs4errBadXDR)
		}
		dir, status := st.dir()
		if status == _nfsOK && st.saved == nil {
			status = _nfs4errNoFileHandle
		}
		if status == _nfsOK {
			status = checkName(name)
		}
		if status != _nfsOK {
			return fail(status)
		}
		src, dst := st.saved.path, dir.path+"/"+name
		var sys syscall.Stat_t
		if err := syscall.Lstat(src, &sys); err != nil {
			return fail(errnoStatus(err))
		}
		switch fileType(sys.Mode) {
		case _nf4Dir:
			return fail(_nfs4errIsDir)
		case _nf4Lnk:
			return fail(_nfs4errNotSupp) // which link(2) makes of a symlink varies; no need for one
		}
		if s.protected(src) {
			return fail(s.refuse("link", src))
		}
		if s.protectedFile(dst) {
			return fail(s.refuse("link", dst))
		}
		before := dirChange(dir.path)
		if err := os.Link(src, dst); err != nil {
			return fail(errnoStatus(err))
		}
		w.u32(_nfsOK)
		changeInfo(w, before, dirChange(dir.path))
		return _nfsOK
	case _opSetAttr:
		r.take(16) // stateid
		mask := r.bitmap()
		vals := &xdrReader{b: r.opaque(1 << 16)}
		status := uint32(_nfsOK)
		switch {
		case r.err:
			status = _nfs4errBadXDR
		case st.cur == nil:
			status = _nfs4errNoFileHandle
		case s.protectedFile(st.cur.path):
			status = s.refuse("setattr", st.cur.path)
		}
		var set []int
		if status == _nfsOK {
			set, status = s.setattr(st.cur.path, mask, vals)
		}
		w.u32(status)
		w.bitmap(attrMask(set...)) // SETATTR's attrsset follows even a failure
		return status
	case _opLock:
		return s.lock(r, w, st)
	case _opLockT:
		typ, offset, length := r.u32(), r.u64(), r.u64()
		owner := lockOwner{r.u64(), string(r.opaque(1024))}
		if r.err {
			return fail(_nfs4errBadXDR)
		}
		if st.cur == nil {
			return fail(_nfs4errNoFileHandle)
		}
		want, status := newRange(owner, typ, offset, length)
		if status != _nfsOK {
			return fail(status)
		}
		if c, ok := s.conflict(*st.cur, want); ok {
			return denied(w, c)
		}
		return fail(_nfsOK)
	case _opLockU:
		typ := r.u32()
		seqid, id := r.u32(), r.take(16)[4:]
		offset, length := r.u64(), r.u64()
		if r.err {
			return fail(_nfs4errBadXDR)
		}
		if st.cur == nil {
			return fail(_nfs4errNoFileHandle)
		}
		owner, ok := s.lockOwners[binary.BigEndian.Uint64(id)]
		if !ok {
			return fail(_nfs4errBadStateID)
		}
		cut, status := newRange(owner, typ, offset, length)
		if status != _nfsOK {
			return fail(status)
		}
		s.unlock(*st.cur, cut)
		w.u32(_nfsOK)
		w.u32(seqid + 1)
		w.b = append(w.b, id...)
		return _nfsOK
	case _opOpenConfirm:
		_, other, seqid := r.u32(), r.take(12), r.u32()
		if r.err {
			return fail(_nfs4errBadXDR)
		}
		w.u32(_nfsOK)
		w.u32(seqid + 1)
		w.b = append(w.b, other...)
		return _nfsOK
	case _opOpenDowngrade:
		_, other := r.u32(), r.take(12)
		seqid := r.u32()
		r.u32()
		r.u32()
		if r.err {
			return fail(_nfs4errBadXDR)
		}
		w.u32(_nfsOK)
		w.u32(seqid + 1)
		w.b = append(w.b, other...)
		return _nfsOK
	case _opDelegReturn: // sbox never delegates
		r.take(16)
		return fail(_nfsOK)
	case _opDelegPurge:
		r.u64()
		return fail(_nfsOK)
	case _opReleaseLockOwner:
		r.u64()
		r.opaque(1024)
		return fail(_nfsOK)
	}
	if op >= _opAccess && op <= _opReleaseLockOwner { // known, not served: NVERIFY, OPENATTR, VERIFY
		return fail(_nfs4errNotSupp)
	}
	return _nfs4errOpIllegal
}

func dirChange(p string) uint64 {
	var st syscall.Stat_t
	if syscall.Lstat(p, &st) != nil {
		return 0
	}
	return changeOf(&st)
}

func changeInfo(w *xdrWriter, before, after uint64) {
	w.u32(0) // not atomic
	w.u64(before)
	w.u64(after)
}

// write is WRITE. A stable write is synced before the reply.
func (s *nfsServer) write(r *xdrReader, w *xdrWriter, st *compoundState) uint32 {
	fail := func(status uint32) uint32 {
		w.u32(status)
		return status
	}
	r.take(16) // stateid
	offset, stable := r.u64(), r.u32()
	data := r.opaque(_maxIO)
	if r.err {
		return fail(_nfs4errBadXDR)
	}
	if st.cur == nil {
		return fail(_nfs4errNoFileHandle)
	}
	if s.protectedFile(st.cur.path) {
		return fail(s.refuse("write", st.cur.path))
	}
	f, err := os.OpenFile(st.cur.path, os.O_WRONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fail(errnoStatus(err))
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteAt(data, int64(offset)); err != nil {
		return fail(errnoStatus(err))
	}
	if stable != 0 {
		if err := f.Sync(); err != nil {
			return fail(errnoStatus(err))
		}
		stable = 2 // FILE_SYNC
	}
	w.u32(_nfsOK)
	w.u32(uint32(len(data)))
	w.u32(stable)
	w.b = append(w.b, s.verf[:]...)
	return _nfsOK
}

// create is CREATE: a directory, a symlink or a FIFO. Regular files come
// from OPEN; devices and sockets aren't made.
func (s *nfsServer) create(r *xdrReader, w *xdrWriter, st *compoundState) uint32 {
	fail := func(status uint32) uint32 {
		w.u32(status)
		return status
	}
	typ := r.u32()
	var target string
	switch typ {
	case _nf4Lnk:
		target = r.str(4096)
	case 3, 4: // block and character devices
		r.u32()
		r.u32()
	}
	name := r.str(1024)
	mask := r.bitmap()
	vals := &xdrReader{b: r.opaque(1 << 16)}
	if r.err {
		return fail(_nfs4errBadXDR)
	}
	dir, status := st.dir()
	if status == _nfsOK {
		status = checkName(name)
	}
	if status != _nfsOK {
		return fail(status)
	}
	p := dir.path + "/" + name
	if s.protected(p) || typ != _nf4Dir && s.protectedFile(p) || typ == _nf4Lnk && !s.symlinkTarget(dir.path, target) {
		return fail(s.refuse("create", p))
	}
	mode := createMode(mask, vals, 0o755)
	before := dirChange(dir.path)
	var err error
	switch typ {
	case _nf4Dir:
		err = syscall.Mkdir(p, mode)
	case _nf4Lnk:
		err = syscall.Symlink(target, p)
	case _nf4FIFO:
		err = syscall.Mkfifo(p, mode)
	default:
		return fail(_nfs4errBadType)
	}
	if err != nil {
		return fail(errnoStatus(err))
	}
	var set []int
	if typ != _nf4Lnk && hasAttr(mask, _attrMode) {
		_ = os.Chmod(p, os.FileMode(mode)) // past the umask
		set = append(set, _attrMode)
	}
	f, _, err := lstatHandle(p)
	if err != nil {
		return fail(errnoStatus(err))
	}
	st.cur = &f
	w.u32(_nfsOK)
	changeInfo(w, before, dirChange(dir.path))
	w.bitmap(attrMask(set...))
	return _nfsOK
}

// createMode is the mode in a create's attributes, or def.
func createMode(mask []uint32, vals *xdrReader, def uint32) uint32 {
	if !hasAttr(mask, _attrMode) {
		return def
	}
	for a := 0; a < _attrMode; a++ { // skip the values before mode: only size can come first
		if hasAttr(mask, a) && a == _attrSize {
			vals.u64()
		}
	}
	return vals.u32() & 0o7777
}

// setattr applies a SETATTR's values to p and returns the attributes set.
// Ownership can't change (every file is the Mac user's) and is accepted
// as is.
func (s *nfsServer) setattr(p string, mask []uint32, vals *xdrReader) ([]int, uint32) {
	settable := attrMask(_attrSize, _attrMode, _attrOwner, _attrOwnerGroup, _attrTimeAccessSet, _attrTimeModifySet)
	for i, word := range mask {
		if i >= len(settable) && word != 0 || i < len(settable) && word&^settable[i] != 0 {
			return nil, _nfs4errAttrNotSupp
		}
	}
	var sys syscall.Stat_t
	if err := syscall.Lstat(p, &sys); err != nil {
		return nil, errnoStatus(err)
	}
	link := fileType(sys.Mode) == _nf4Lnk
	atime := time.Unix(sys.Atimespec.Sec, sys.Atimespec.Nsec)
	mtime := time.Unix(sys.Mtimespec.Sec, sys.Mtimespec.Nsec)
	settime := func() time.Time {
		if vals.u32() == 1 { // SET_TO_CLIENT_TIME
			return time.Unix(int64(vals.u64()), int64(vals.u32()))
		}
		return time.Now()
	}
	var set []int
	var times bool
	for a := 0; a < 32*len(mask); a++ {
		if !hasAttr(mask, a) {
			continue
		}
		var err error
		switch a {
		case _attrSize:
			size := vals.u64()
			if fileType(sys.Mode) != _nf4Reg {
				return set, _nfs4errInval
			}
			err = os.Truncate(p, int64(size))
		case _attrMode:
			mode := vals.u32()
			if !link {
				err = os.Chmod(p, os.FileMode(mode&0o777)|unixBits(mode))
			}
		case _attrOwner, _attrOwnerGroup:
			vals.str(64)
		case _attrTimeAccessSet:
			atime, times = settime(), true
		case _attrTimeModifySet:
			mtime, times = settime(), true
		}
		if vals.err {
			return set, _nfs4errBadXDR
		}
		if err != nil {
			return set, errnoStatus(err)
		}
		set = append(set, a)
	}
	if times && !link {
		if err := os.Chtimes(p, atime, mtime); err != nil {
			return nil, errnoStatus(err)
		}
	}
	return set, _nfsOK
}

// unixBits turns a mode's setuid, setgid and sticky bits into Go's.
func unixBits(mode uint32) os.FileMode {
	var m os.FileMode
	if mode&0o4000 != 0 {
		m |= os.ModeSetuid
	}
	if mode&0o2000 != 0 {
		m |= os.ModeSetgid
	}
	if mode&0o1000 != 0 {
		m |= os.ModeSticky
	}
	return m
}

// newRange is a lock request's range: INVAL for an empty one or one past
// the largest offset.
func newRange(owner lockOwner, typ uint32, offset, length uint64) (lockRange, uint32) {
	l := lockRange{owner: owner, start: offset, write: typ == 2 || typ == 4}
	switch {
	case length == 0:
		return l, _nfs4errInval
	case length == ^uint64(0):
		l.end = ^uint64(0)
	case offset+length < offset:
		return l, _nfs4errInval
	default:
		l.end = offset + length
	}
	return l, _nfsOK
}

// conflict is a lock on f that want can't share: another owner's,
// overlapping, and one of the two a write lock.
func (s *nfsServer) conflict(f fileHandle, want lockRange) (lockRange, bool) {
	for _, l := range s.locks[[2]uint64{uint64(f.dev), f.ino}] {
		if l.owner != want.owner && l.start < want.end && want.start < l.end && (l.write || want.write) {
			return l, true
		}
	}
	return lockRange{}, false
}

// unlock drops cut's owner's locks on f over cut's range, splitting a
// lock that sticks out on either side.
func (s *nfsServer) unlock(f fileHandle, cut lockRange) {
	key := [2]uint64{uint64(f.dev), f.ino}
	var kept []lockRange
	for _, l := range s.locks[key] {
		if l.owner != cut.owner || l.end <= cut.start || cut.end <= l.start {
			kept = append(kept, l)
			continue
		}
		if l.start < cut.start {
			kept = append(kept, lockRange{owner: l.owner, start: l.start, end: cut.start, write: l.write})
		}
		if cut.end < l.end {
			kept = append(kept, lockRange{owner: l.owner, start: cut.end, end: l.end, write: l.write})
		}
	}
	if len(kept) == 0 {
		delete(s.locks, key)
		return
	}
	s.locks[key] = kept
}

func denied(w *xdrWriter, l lockRange) uint32 {
	w.u32(_nfs4errDenied)
	w.u64(l.start)
	if l.end == ^uint64(0) {
		w.u64(^uint64(0))
	} else {
		w.u64(l.end - l.start)
	}
	if l.write {
		w.u32(2)
	} else {
		w.u32(1)
	}
	w.u64(l.owner.client)
	w.opaque([]byte(l.owner.owner))
	return _nfs4errDenied
}

// lock is LOCK: granted unless another owner holds a conflicting range.
func (s *nfsServer) lock(r *xdrReader, w *xdrWriter, st *compoundState) uint32 {
	fail := func(status uint32) uint32 {
		w.u32(status)
		return status
	}
	typ := r.u32()
	r.u32() // reclaim
	offset, length := r.u64(), r.u64()
	var owner lockOwner
	var id uint64
	var seqid uint32
	if r.u32() == 1 { // a new lock owner, from an open
		r.u32()
		r.take(16)
		seqid = r.u32()
		owner = lockOwner{r.u64(), string(r.opaque(1024))}
		s.stateids++
		id = s.stateids
	} else {
		other := r.take(16)[4:]
		seqid = r.u32()
		id = binary.BigEndian.Uint64(other)
		var ok bool
		if owner, ok = s.lockOwners[id]; !ok && !r.err {
			return fail(_nfs4errBadStateID)
		}
	}
	if r.err {
		return fail(_nfs4errBadXDR)
	}
	if st.cur == nil {
		return fail(_nfs4errNoFileHandle)
	}
	want, status := newRange(owner, typ, offset, length)
	if status != _nfsOK {
		return fail(status)
	}
	if c, ok := s.conflict(*st.cur, want); ok {
		return denied(w, c)
	}
	s.unlock(*st.cur, want) // an upgrade or downgrade replaces the owner's own range
	key := [2]uint64{uint64(st.cur.dev), st.cur.ino}
	s.locks[key] = append(s.locks[key], want)
	s.lockOwners[id] = owner
	w.u32(_nfsOK)
	w.u32(seqid + 1)
	w.u64(id)
	w.u32(0)
	return _nfsOK
}

// access is the ACCESS bits of ask the Mac user has on p. A symlink is
// only read.
func access(p string, ask uint32) uint32 {
	var st syscall.Stat_t
	if syscall.Lstat(p, &st) != nil {
		return 0
	}
	if fileType(st.Mode) == _nf4Lnk {
		return ask & _accessRead
	}
	can := func(mode uint32) bool { return syscall.Access(p, mode) == nil }
	var got uint32
	if can(4) { // R_OK
		got |= _accessRead
	}
	if can(2) { // W_OK
		got |= _accessModify | _accessExtend
		if fileType(st.Mode) == _nf4Dir {
			got |= _accessDelete
		}
	}
	if can(1) { // X_OK
		if fileType(st.Mode) == _nf4Dir {
			got |= _accessLookup
		} else {
			got |= _accessExecute
		}
	}
	return ask & got
}

// readdir is READDIR. Cookies are an entry's index in the sorted listing
// plus 3 (0 starts, 1 and 2 are reserved); the verifier is always zero.
func (s *nfsServer) readdir(r *xdrReader, w *xdrWriter, st *compoundState) uint32 {
	cookie := r.u64()
	r.take(8) // cookie verifier
	r.u32()   // dircount, a hint
	maxcount := int(r.u32())
	mask := r.bitmap()
	if r.err {
		w.u32(_nfs4errBadXDR)
		return _nfs4errBadXDR
	}
	dir, status := st.dir()
	if status == _nfsOK && cookie != 0 && cookie < 3 {
		status = _nfs4errBadCookie
	}
	var entries []os.DirEntry
	if status == _nfsOK {
		var err error
		if entries, err = os.ReadDir(dir.path); err != nil {
			status = errnoStatus(err)
		}
		entries = slices.DeleteFunc(entries, func(e os.DirEntry) bool { return s.hidden(dir.path + "/" + e.Name()) })
	}
	if status != _nfsOK {
		w.u32(status)
		return status
	}
	start := 0
	if cookie != 0 {
		start = int(cookie - 2)
	}

	var list xdrWriter
	size, n, i := 8+8, 0, start // the verifier, and the closing bool and eof
	for ; i < len(entries); i++ {
		p := dir.path + "/" + entries[i].Name()
		f, sys, err := lstatHandle(p)
		if err != nil {
			continue // gone since the listing
		}
		var e xdrWriter
		e.u32(1)
		e.u64(uint64(i) + 3)
		e.str(entries[i].Name())
		s.attrs(&e, mask, f, &sys)
		if size+len(e.b) > maxcount {
			break
		}
		size += len(e.b)
		list.b = append(list.b, e.b...)
		n++
	}
	if n == 0 && i < len(entries) {
		w.u32(_nfs4errTooSmall)
		return _nfs4errTooSmall
	}
	w.u32(_nfsOK)
	w.b = append(w.b, make([]byte, 8)...)
	w.b = append(w.b, list.b...)
	w.u32(0)
	w.u32(b2u(i >= len(entries)))
	return _nfsOK
}

func b2u(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}

// open is OPEN: an existing file, or a new one (UNCHECKED, GUARDED, or
// EXCLUSIVE with its verifier kept in the file's times, so a retransmit
// opens the file it made).
func (s *nfsServer) open(r *xdrReader, w *xdrWriter, st *compoundState) uint32 {
	fail := func(status uint32) uint32 {
		w.u32(status)
		return status
	}
	r.u32() // seqid
	shareAccess := r.u32()
	r.u32() // share_deny
	r.u64() // the owner's client id
	r.opaque(1024)
	create, how := r.u32() == 1, uint32(0)
	var mask []uint32
	var vals *xdrReader
	var verf []byte
	if create {
		switch how = r.u32(); how {
		case 0, 1: // UNCHECKED, GUARDED
			mask = r.bitmap()
			vals = &xdrReader{b: r.opaque(1 << 16)}
		default: // EXCLUSIVE
			verf = r.take(8)
		}
	}
	claim := r.u32()
	var name string
	switch claim {
	case 0: // CLAIM_NULL
		name = r.str(1024)
	case 1: // CLAIM_PREVIOUS: reclaiming after a server restart
		r.u32()
	default:
		if r.err {
			return fail(_nfs4errBadXDR)
		}
		return fail(_nfs4errNotSupp)
	}
	if r.err {
		return fail(_nfs4errBadXDR)
	}

	var p, dirPath string
	if claim == 0 {
		dir, status := st.dir()
		if status == _nfsOK {
			status = checkName(name)
		}
		if status != _nfsOK {
			return fail(status)
		}
		dirPath, p = dir.path, dir.path+"/"+name
	} else if st.cur == nil {
		return fail(_nfs4errNoFileHandle)
	} else {
		p, dirPath, create = st.cur.path, filepath.Dir(st.cur.path), false
	}
	if (create || shareAccess&2 != 0) && s.protectedFile(p) {
		return fail(s.refuse("open", p))
	}
	if s.hidden(p) {
		return fail(s.missing(p))
	}
	before := dirChange(dirPath)
	var set []int
	if create {
		flags := os.O_RDWR | os.O_CREATE | syscall.O_NOFOLLOW
		if how != 0 {
			flags |= os.O_EXCL
		}
		mode := uint32(0o644)
		if how != 2 {
			mode = createMode(mask, vals, mode)
		}
		f, err := os.OpenFile(p, flags, os.FileMode(mode))
		switch {
		case err == nil:
			_ = f.Close()
			if how == 2 {
				t := verifierTime(verf)
				_ = os.Chtimes(p, t, t)
			} else if hasAttr(mask, _attrMode) {
				_ = os.Chmod(p, os.FileMode(mode&0o777)) // past the umask
				set = append(set, _attrMode)
			}
		case how == 2 && errors.Is(err, fs.ErrExist):
			if fi, err := os.Lstat(p); err != nil || !fi.ModTime().Equal(verifierTime(verf)) {
				return fail(_nfs4errExist)
			}
		default:
			return fail(errnoStatus(err))
		}
	}
	f, sys, err := lstatHandle(p)
	if err != nil {
		return fail(errnoStatus(err))
	}
	switch fileType(sys.Mode) {
	case _nf4Dir:
		return fail(_nfs4errIsDir)
	case _nf4Lnk:
		return fail(_nfs4errSymlink)
	}
	st.cur = &f
	s.stateids++
	w.u32(_nfsOK)
	w.u32(1)
	w.u64(s.stateids)
	w.u32(0)
	changeInfo(w, before, dirChange(dirPath))
	w.u32(_openResultLocktypePOSIX)
	w.bitmap(attrMask(set...))
	w.u32(0) // no delegation
	return _nfsOK
}

// verifierTime is where an EXCLUSIVE create keeps its verifier.
func verifierTime(verf []byte) time.Time {
	return time.Unix(int64(binary.BigEndian.Uint32(verf[:4])), int64(binary.BigEndian.Uint32(verf[4:])%1e9))
}

// read is READ. Every stateid is accepted: nothing is shared but the
// guest's own opens.
func (s *nfsServer) read(r *xdrReader, w *xdrWriter, st *compoundState) uint32 {
	r.take(16) // stateid
	offset, count := r.u64(), r.u32()
	if r.err {
		w.u32(_nfs4errBadXDR)
		return _nfs4errBadXDR
	}
	if st.cur == nil {
		w.u32(_nfs4errNoFileHandle)
		return _nfs4errNoFileHandle
	}
	f, err := os.OpenFile(st.cur.path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			err = syscall.EINVAL
		}
		w.u32(errnoStatus(err))
		return errnoStatus(err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		w.u32(_nfs4errIsDir)
		return _nfs4errIsDir
	}
	buf := make([]byte, min(count, _maxIO))
	n, err := f.ReadAt(buf, int64(offset))
	if err != nil && !errors.Is(err, io.EOF) {
		w.u32(errnoStatus(err))
		return errnoStatus(err)
	}
	w.u32(_nfsOK)
	w.u32(b2u(int64(offset)+int64(n) >= fi.Size()))
	w.opaque(buf[:n])
	return _nfsOK
}

// supportedAttrs are the attributes attrs can encode.
var supportedAttrs = attrMask(_attrSupportedAttrs, _attrType, _attrFHExpireType, _attrChange, _attrSize,
	_attrLinkSupport, _attrSymlinkSupport, _attrNamedAttr, _attrFSID, _attrUniqueHandles, _attrLeaseTime,
	_attrRdattrError, _attrCanSetTime, _attrCaseInsensitive, _attrCasePreserving, _attrChownRestricted,
	_attrFileHandle, _attrFileID, _attrFilesAvail, _attrFilesFree, _attrFilesTotal, _attrHomogeneous,
	_attrMaxFileSize, _attrMaxLink, _attrMaxName, _attrMaxRead, _attrMaxWrite, _attrMode, _attrNoTrunc,
	_attrNumLinks, _attrOwner, _attrOwnerGroup, _attrRawDev, _attrSpaceAvail, _attrSpaceFree,
	_attrSpaceTotal, _attrSpaceUsed, _attrTimeAccess, _attrTimeDelta, _attrTimeMetadata, _attrTimeModify,
	_attrMountedOnFileID)

func changeOf(st *syscall.Stat_t) uint64 {
	return uint64(st.Ctimespec.Sec)*1e9 + uint64(st.Ctimespec.Nsec)
}

// attrs writes the fattr4 for the requested attributes sbox supports, in
// attribute-number order. ponytail: one fsid for the whole folder; a
// volume mounted inside it shares the folder's fsid.
func (s *nfsServer) attrs(w *xdrWriter, mask []uint32, f fileHandle, st *syscall.Stat_t) {
	var fs *syscall.Statfs_t
	statfs := func() *syscall.Statfs_t {
		if fs == nil {
			fs = &syscall.Statfs_t{}
			_ = syscall.Statfs(s.root, fs)
		}
		return fs
	}
	var got []int
	var v xdrWriter
	time := func(t syscall.Timespec) {
		v.u64(uint64(t.Sec))
		v.u32(uint32(t.Nsec))
	}
	for a := 0; a < 32*len(mask); a++ {
		if !hasAttr(mask, a) || !hasAttr(supportedAttrs, a) {
			continue
		}
		got = append(got, a)
		switch a {
		case _attrSupportedAttrs:
			v.bitmap(supportedAttrs)
		case _attrType:
			v.u32(fileType(st.Mode))
		case _attrFHExpireType:
			v.u32(0) // persistent
		case _attrChange:
			v.u64(changeOf(st))
		case _attrSize:
			v.u64(uint64(st.Size))
		case _attrLinkSupport, _attrSymlinkSupport, _attrUniqueHandles, _attrCanSetTime,
			_attrCasePreserving, _attrChownRestricted, _attrHomogeneous, _attrNoTrunc:
			v.u32(1)
		case _attrNamedAttr, _attrCaseInsensitive:
			v.u32(0)
		case _attrFSID:
			v.u64(1)
			v.u64(0)
		case _attrLeaseTime:
			v.u32(_leaseTime)
		case _attrRdattrError:
			v.u32(_nfsOK)
		case _attrFileHandle:
			v.opaque(s.handle(f))
		case _attrFileID, _attrMountedOnFileID:
			v.u64(st.Ino)
		case _attrFilesAvail, _attrFilesFree:
			v.u64(statfs().Ffree)
		case _attrFilesTotal:
			v.u64(statfs().Files)
		case _attrMaxFileSize:
			v.u64(1<<63 - 1)
		case _attrMaxLink:
			v.u32(32767)
		case _attrMaxName:
			v.u32(255)
		case _attrMaxRead, _attrMaxWrite:
			v.u64(_maxIO)
		case _attrMode:
			v.u32(uint32(st.Mode & 0o7777))
		case _attrNumLinks:
			v.u32(uint32(st.Nlink))
		case _attrOwner, _attrOwnerGroup:
			v.str(_guestOwner)
		case _attrRawDev:
			v.u32(uint32(st.Rdev) >> 24)
			v.u32(uint32(st.Rdev) & 0xffffff)
		case _attrSpaceAvail:
			v.u64(statfs().Bavail * uint64(statfs().Bsize))
		case _attrSpaceFree:
			v.u64(statfs().Bfree * uint64(statfs().Bsize))
		case _attrSpaceTotal:
			v.u64(statfs().Blocks * uint64(statfs().Bsize))
		case _attrSpaceUsed:
			v.u64(uint64(st.Blocks) * 512)
		case _attrTimeAccess:
			time(st.Atimespec)
		case _attrTimeDelta:
			v.u64(0)
			v.u32(1)
		case _attrTimeMetadata:
			time(st.Ctimespec)
		case _attrTimeModify:
			time(st.Mtimespec)
		}
	}
	w.bitmap(attrMask(got...))
	w.opaque(v.b)
}

func fileType(mode uint16) uint32 {
	switch mode & syscall.S_IFMT {
	case syscall.S_IFDIR:
		return _nf4Dir
	case syscall.S_IFLNK:
		return _nf4Lnk
	}
	return _nf4Reg
}
