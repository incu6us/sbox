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
	"os"
	"path/filepath"
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
	_nfs4errNoFileHandle      = 10020
	_nfs4errMinorVersMismatch = 10021
	_nfs4errStaleClientID     = 10022
	_nfs4errSymlink           = 10029
	_nfs4errRestoreFH         = 10030
	_nfs4errBadXDR            = 10036
	_nfs4errBadName           = 10041
	_nfs4errOpIllegal         = 10044

	_opAccess             = 3
	_opClose              = 4
	_opDelegPurge         = 7
	_opDelegReturn        = 8
	_opGetAttr            = 9
	_opGetFH              = 10
	_opLookup             = 15
	_opLookupP            = 16
	_opOpen               = 18
	_opPutFH              = 22
	_opPutPubFH           = 23
	_opPutRootFH          = 24
	_opRead               = 25
	_opReadDir            = 26
	_opReadLink           = 27
	_opRenew              = 30
	_opRestoreFH          = 31
	_opSaveFH             = 32
	_opSecInfo            = 33
	_opSetClientID        = 35
	_opSetClientIDConfirm = 36
	_opReleaseLockOwner   = 39
	_opIllegal            = 10044

	_nf4Reg = 1
	_nf4Dir = 2
	_nf4Lnk = 5

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
	_attrTimeDelta       = 51
	_attrTimeMetadata    = 52
	_attrTimeModify      = 53
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
}

func newNFSServer(root string, key []byte) *nfsServer {
	return &nfsServer{root: root, key: key, clients: map[uint64][8]byte{}, nextID: uint64(time.Now().Unix()) << 32}
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
	if err != nil || (p != s.root && !strings.HasPrefix(p, s.root+"/")) {
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
		if dir.path == s.root {
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
		w.u32(access(st.cur.path, ask))
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
	return _nfs4errOpIllegal
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

// open is OPEN. ponytail: reading only for now; writes come with the
// protected paths.
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
	create := r.u32()
	if create == 1 {
		switch r.u32() {
		case 0, 1: // UNCHECKED, GUARDED
			r.bitmap()
			r.opaque(1 << 16)
		default: // EXCLUSIVE
			r.take(8)
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
	if create == 1 || shareAccess&2 != 0 {
		return fail(_nfs4errROFS)
	}

	var dirChange uint64
	target := st.cur
	if claim == 0 {
		dir, status := st.dir()
		if status == _nfsOK {
			status = checkName(name)
		}
		if status != _nfsOK {
			return fail(status)
		}
		var dsys syscall.Stat_t
		if err := syscall.Lstat(dir.path, &dsys); err != nil {
			return fail(errnoStatus(err))
		}
		dirChange = changeOf(&dsys)
		f, _, err := lstatHandle(dir.path + "/" + name)
		if err != nil {
			return fail(errnoStatus(err))
		}
		target = &f
	} else if target == nil {
		return fail(_nfs4errNoFileHandle)
	}
	var sys syscall.Stat_t
	if err := syscall.Lstat(target.path, &sys); err != nil {
		return fail(errnoStatus(err))
	}
	switch fileType(sys.Mode) {
	case _nf4Dir:
		return fail(_nfs4errIsDir)
	case _nf4Lnk:
		return fail(_nfs4errSymlink)
	}
	st.cur = target
	s.stateids++
	w.u32(_nfsOK)
	w.u32(1)
	w.u64(s.stateids)
	w.u32(0)
	w.u32(0) // change_info: not atomic
	w.u64(dirChange)
	w.u64(dirChange)
	w.u32(_openResultLocktypePOSIX)
	w.bitmap(nil) // attrset
	w.u32(0)      // no delegation
	return _nfsOK
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
