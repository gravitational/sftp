package sftp

// sftp server counterpart

import (
	"encoding"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const (
	// SftpServerWorkerCount defines the number of workers for the SFTP server
	SftpServerWorkerCount = 8

	defaultFileMode = 0o644
	defaultDirMode  = 0o750
)

// Server is an SSH File Transfer Protocol (sftp) server.
// This is intended to provide the sftp subsystem to an ssh server daemon.
// This implementation currently supports most of sftp server protocol version 3,
// as specified at http://tools.ietf.org/html/draft-ietf-secsh-filexfer-02
type Server struct {
	*serverConn
	debugStream   io.Writer
	reqCallback   RequestCallback
	readOnly      bool
	pktMgr        *packetManager
	openFiles     map[string]*os.File
	openFilesLock sync.RWMutex
	handleCount   int
}

func (svr *Server) nextHandle(f *os.File) string {
	svr.openFilesLock.Lock()
	defer svr.openFilesLock.Unlock()
	svr.handleCount++
	handle := strconv.Itoa(svr.handleCount)
	svr.openFiles[handle] = f
	return handle
}

func (svr *Server) closeHandle(handle string) error {
	svr.openFilesLock.Lock()
	defer svr.openFilesLock.Unlock()
	if f, ok := svr.openFiles[handle]; ok {
		delete(svr.openFiles, handle)
		return f.Close()
	}

	return EBADF
}

func (svr *Server) getHandle(handle string) (*os.File, bool) {
	svr.openFilesLock.RLock()
	defer svr.openFilesLock.RUnlock()
	f, ok := svr.openFiles[handle]
	return f, ok
}

type serverRespondablePacket interface {
	encoding.BinaryUnmarshaler
	id() uint32
	respond(svr *Server) responsePacket
}

// NewServer creates a new Server instance around the provided streams, serving
// content from the root of the filesystem.  Optionally, ServerOption
// functions may be specified to further configure the Server.
//
// A subsequent call to Serve() is required to begin serving files over SFTP.
func NewServer(rwc io.ReadWriteCloser, options ...ServerOption) (*Server, error) {
	svrConn := &serverConn{
		conn: conn{
			Reader:      rwc,
			WriteCloser: rwc,
		},
	}
	s := &Server{
		serverConn:  svrConn,
		debugStream: ioutil.Discard,
		// default to setting reqCallback to a function that does nothing
		// so we don't have to check if reqCallback is nil every time we
		// want to call it
		reqCallback: func(_ RequestPacket) {},
		pktMgr:      newPktMgr(svrConn),
		openFiles:   make(map[string]*os.File),
	}

	for _, o := range options {
		if err := o(s); err != nil {
			return nil, err
		}
	}

	return s, nil
}

// A ServerOption is a function which applies configuration to a Server.
type ServerOption func(*Server) error

// WithDebug enables Server debugging output to the supplied io.Writer.
func WithDebug(w io.Writer) ServerOption {
	return func(s *Server) error {
		s.debugStream = w
		return nil
	}
}

// ReadOnly configures a Server to serve files in read-only mode.
func ReadOnly() ServerOption {
	return func(s *Server) error {
		s.readOnly = true
		return nil
	}
}

// RequestType is the type of Request a client made.
type RequestType uint

const (
	Open RequestType = iota
	Close
	Read
	Write
	Lstat
	Fstat
	Setstat
	Fsetstat
	Opendir
	Readdir
	Remove
	Mkdir
	Rmdir
	Realpath
	Stat
	Rename
	Readlink
	Symlink
)

// RequestPacket is information about a client request.
type RequestPacket struct {
	Type RequestType
	// Path is the path the request specified, or if the request specified
	// a handle instead, Path is the path corresponding to that handle.
	Path string
	// TargetPath is the new path in a rename request, or the new path in
	// a symlink request.
	TargetPath string
	// Flags is any flags the request passed.
	Flags      uint32
	Attributes *Attributes
	// Err is the error that occured from handling the request.
	Err error
}

// Attributes is optional metadata that may or may not be present in a
// client request.
type Attributes struct {
	Size             *uint64
	UID              *uint32
	GID              *uint32
	Permissions      *fs.FileMode
	AccessTime       *time.Time
	ModificationTime *time.Time
}

// RequestCallback is the type of function called by a Server when
// WithRequestCallback is set. reqPacket will contain details of
// a client request the server has recieved.
type RequestCallback func(reqPacket RequestPacket)

// WithRequestCallback sets a RequestCallback to be called whenever
// the server recieves a client request.
func WithRequestCallback(reqCallback RequestCallback) ServerOption {
	return func(s *Server) error {
		s.reqCallback = reqCallback
		return nil
	}
}

// WithAllocator enable the allocator.
// After processing a packet we keep in memory the allocated slices
// and we reuse them for new packets.
// The allocator is experimental
func WithAllocator() ServerOption {
	return func(s *Server) error {
		alloc := newAllocator()
		s.pktMgr.alloc = alloc
		s.conn.alloc = alloc
		return nil
	}
}

type rxPacket struct {
	pktType  fxp
	pktBytes []byte
}

// Up to N parallel servers
func (svr *Server) sftpServerWorker(pktChan chan orderedRequest) error {
	for pkt := range pktChan {
		// readonly checks
		readonly := true
		switch pkt := pkt.requestPacket.(type) {
		case notReadOnly:
			readonly = false
		case *sshFxpOpenPacket:
			readonly = pkt.readonly()
		case *sshFxpExtendedPacket:
			readonly = pkt.readonly()
		}

		// If server is operating read-only and a write operation is requested,
		// return permission denied
		if !readonly && svr.readOnly {
			svr.pktMgr.readyPacket(
				svr.pktMgr.newOrderedResponse(statusFromError(pkt.id(), syscall.EPERM), pkt.orderID()),
			)
			continue
		}

		if err := handlePacket(svr, pkt); err != nil {
			return err
		}
	}
	return nil
}

func handlePacket(s *Server, p orderedRequest) error {
	var rpkt responsePacket
	orderID := p.orderID()
	switch p := p.requestPacket.(type) {
	case *sshFxInitPacket:
		rpkt = &sshFxVersionPacket{
			Version:    sftpProtocolVersion,
			Extensions: sftpExtensions,
		}
	case *sshFxpStatPacket:
		// stat the requested file
		info, err := os.Stat(toLocalPath(p.Path))
		rpkt = &sshFxpStatResponse{
			ID:   p.ID,
			info: info,
		}
		if err != nil {
			rpkt = statusFromError(p.ID, err)
		}
		s.reqCallback(RequestPacket{
			Type: Stat,
			Path: p.Path,
			Err:  err,
		})
	case *sshFxpLstatPacket:
		// stat the requested file
		info, err := os.Lstat(toLocalPath(p.Path))
		rpkt = &sshFxpStatResponse{
			ID:   p.ID,
			info: info,
		}
		if err != nil {
			rpkt = statusFromError(p.ID, err)
		}
		s.reqCallback(RequestPacket{
			Type: Lstat,
			Path: p.Path,
			Err:  err,
		})
	case *sshFxpFstatPacket:
		f, ok := s.getHandle(p.Handle)
		var err error = EBADF
		var info os.FileInfo
		if ok {
			info, err = f.Stat()
			rpkt = &sshFxpStatResponse{
				ID:   p.ID,
				info: info,
			}
		}
		if err != nil {
			rpkt = statusFromError(p.ID, err)
		}
		s.reqCallback(RequestPacket{
			Type: Fstat,
			Path: f.Name(),
			Err:  err,
		})
	case *sshFxpMkdirPacket:
		var mode os.FileMode = 0o755
		var attributes *Attributes
		if p.Attrs != nil {
			attrs, _ := unmarshalFileStat(p.Flags, p.Attrs.([]byte))
			if p.Flags&sshFileXferAttrPermissions != 0 {
				mode = toFileMode(attrs.Mode)
				attributes = &Attributes{
					Permissions: &mode,
				}
			}
		}

		err := os.Mkdir(toLocalPath(p.Path), mode)
		rpkt = statusFromError(p.ID, err)
		s.reqCallback(RequestPacket{
			Type:       Mkdir,
			Path:       p.Path,
			Flags:      p.Flags,
			Attributes: attributes,
			Err:        err,
		})
	case *sshFxpRmdirPacket:
		err := os.Remove(toLocalPath(p.Path))
		rpkt = statusFromError(p.ID, err)
		s.reqCallback(RequestPacket{
			Type: Rmdir,
			Path: p.Path,
			Err:  err,
		})
	case *sshFxpRemovePacket:
		err := os.Remove(toLocalPath(p.Filename))
		rpkt = statusFromError(p.ID, err)
		s.reqCallback(RequestPacket{
			Type: Remove,
			Path: p.Filename,
			Err:  err,
		})
	case *sshFxpRenamePacket:
		err := os.Rename(toLocalPath(p.Oldpath), toLocalPath(p.Newpath))
		rpkt = statusFromError(p.ID, err)
		s.reqCallback(RequestPacket{
			Type:       Rename,
			Path:       p.Oldpath,
			TargetPath: p.Newpath,
			Err:        err,
		})
	case *sshFxpSymlinkPacket:
		err := os.Symlink(toLocalPath(p.Targetpath), toLocalPath(p.Linkpath))
		rpkt = statusFromError(p.ID, err)
		s.reqCallback(RequestPacket{
			Type:       Symlink,
			Path:       p.Targetpath,
			TargetPath: p.Linkpath,
			Err:        err,
		})
	case *sshFxpClosePacket:
		f, ok := s.getHandle(p.Handle)
		var err error = EBADF
		if ok {
			err = s.closeHandle(p.Handle)
		}

		rpkt = statusFromError(p.ID, err)
		s.reqCallback(RequestPacket{
			Type: Close,
			Path: f.Name(),
			Err:  err,
		})
	case *sshFxpReadlinkPacket:
		f, err := os.Readlink(toLocalPath(p.Path))
		rpkt = &sshFxpNamePacket{
			ID: p.ID,
			NameAttrs: []*sshFxpNameAttr{
				{
					Name:     f,
					LongName: f,
					Attrs:    emptyFileStat,
				},
			},
		}
		if err != nil {
			rpkt = statusFromError(p.ID, err)
		}
		s.reqCallback(RequestPacket{
			Type: Readlink,
			Path: p.Path,
			Err:  err,
		})
	case *sshFxpRealpathPacket:
		f, err := filepath.Abs(toLocalPath(p.Path))
		f = cleanPath(f)
		rpkt = &sshFxpNamePacket{
			ID: p.ID,
			NameAttrs: []*sshFxpNameAttr{
				{
					Name:     f,
					LongName: f,
					Attrs:    emptyFileStat,
				},
			},
		}
		if err != nil {
			rpkt = statusFromError(p.ID, err)
		}
		s.reqCallback(RequestPacket{
			Type: Realpath,
			Path: p.Path,
			Err:  err,
		})
	case *sshFxpOpendirPacket:
		p.Path = toLocalPath(p.Path)

		stat, err := os.Stat(p.Path)
		if err != nil {
			rpkt = statusFromError(p.ID, err)
			s.reqCallback(RequestPacket{
				Type: Opendir,
				Path: p.Path,
				Err:  err,
			})
		} else if !stat.IsDir() {
			rpkt = statusFromError(p.ID, &os.PathError{
				Path: p.Path, Err: syscall.ENOTDIR})
			s.reqCallback(RequestPacket{
				Type: Opendir,
				Path: p.Path,
				Err:  err,
			})
		} else {
			rpkt = (&sshFxpOpenPacket{
				ID:     p.ID,
				Path:   p.Path,
				Pflags: sshFxfRead,
			}).respond(s)
		}
	case *sshFxpReadPacket:
		f, ok := s.getHandle(p.Handle)
		var err error = EBADF
		if ok {
			err = nil
			data := p.getDataSlice(s.pktMgr.alloc, orderID)
			n, _err := f.ReadAt(data, int64(p.Offset))
			if _err != nil && (_err != io.EOF || n == 0) {
				err = _err
			}
			rpkt = &sshFxpDataPacket{
				ID:     p.ID,
				Length: uint32(n),
				Data:   data[:n],
				// do not use data[:n:n] here to clamp the capacity, we allocated extra capacity above to avoid reallocations
			}
		}
		if err != nil {
			rpkt = statusFromError(p.ID, err)
		}
		s.reqCallback(RequestPacket{
			Type: Read,
			Path: f.Name(),
			Err:  err,
		})
	case *sshFxpWritePacket:
		f, ok := s.getHandle(p.Handle)
		var err error = EBADF
		if ok {
			_, err = f.WriteAt(p.Data, int64(p.Offset))
		}
		rpkt = statusFromError(p.ID, err)
		s.reqCallback(RequestPacket{
			Type: Write,
			Path: f.Name(),
			Err:  err,
		})
	case *sshFxpExtendedPacket:
		if p.SpecificPacket == nil {
			rpkt = statusFromError(p.ID, ErrSSHFxOpUnsupported)
		} else {
			rpkt = p.respond(s)
		}
	case serverRespondablePacket:
		rpkt = p.respond(s)
	default:
		return fmt.Errorf("unexpected packet type %T", p)
	}

	s.pktMgr.readyPacket(s.pktMgr.newOrderedResponse(rpkt, orderID))
	return nil
}

// Serve serves SFTP connections until the streams stop or the SFTP subsystem
// is stopped.
func (svr *Server) Serve() error {
	defer func() {
		if svr.pktMgr.alloc != nil {
			svr.pktMgr.alloc.Free()
		}
	}()
	var wg sync.WaitGroup
	runWorker := func(ch chan orderedRequest) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := svr.sftpServerWorker(ch); err != nil {
				svr.conn.Close() // shuts down recvPacket
			}
		}()
	}
	pktChan := svr.pktMgr.workerChan(runWorker)

	var err error
	var pkt requestPacket
	var pktType uint8
	var pktBytes []byte
	for {
		pktType, pktBytes, err = svr.serverConn.recvPacket(svr.pktMgr.getNextOrderID())
		if err != nil {
			// we don't care about releasing allocated pages here, the server will quit and the allocator freed
			break
		}

		pkt, err = makePacket(rxPacket{fxp(pktType), pktBytes})
		if err != nil {
			switch {
			case errors.Is(err, errUnknownExtendedPacket):
				//if err := svr.serverConn.sendError(pkt, ErrSshFxOpUnsupported); err != nil {
				//	debug("failed to send err packet: %v", err)
				//	svr.conn.Close() // shuts down recvPacket
				//	break
				//}
			default:
				debug("makePacket err: %v", err)
				svr.conn.Close() // shuts down recvPacket
				break
			}
		}

		pktChan <- svr.pktMgr.newOrderedRequest(pkt)
	}

	close(pktChan) // shuts down sftpServerWorkers
	wg.Wait()      // wait for all workers to exit

	// close any still-open files
	for handle, file := range svr.openFiles {
		fmt.Fprintf(svr.debugStream, "sftp server file with handle %q left open: %v\n", handle, file.Name())
		file.Close()
	}
	return err // error from recvPacket
}

type ider interface {
	id() uint32
}

// The init packet has no ID, so we just return a zero-value ID
func (p *sshFxInitPacket) id() uint32 { return 0 }

type sshFxpStatResponse struct {
	ID   uint32
	info os.FileInfo
}

func (p *sshFxpStatResponse) marshalPacket() ([]byte, []byte, error) {
	l := 4 + 1 + 4 // uint32(length) + byte(type) + uint32(id)

	b := make([]byte, 4, l)
	b = append(b, sshFxpAttrs)
	b = marshalUint32(b, p.ID)

	var payload []byte
	payload = marshalFileInfo(payload, p.info)

	return b, payload, nil
}

func (p *sshFxpStatResponse) MarshalBinary() ([]byte, error) {
	header, payload, err := p.marshalPacket()
	return append(header, payload...), err
}

var emptyFileStat = []interface{}{uint32(0)}

func (p *sshFxpOpenPacket) readonly() bool {
	return !p.hasPflags(sshFxfWrite)
}

func (p *sshFxpOpenPacket) hasPflags(flags ...uint32) bool {
	for _, f := range flags {
		if p.Pflags&f == 0 {
			return false
		}
	}
	return true
}

func (p *sshFxpOpenPacket) respond(svr *Server) responsePacket {
	var osFlags int
	if p.hasPflags(sshFxfRead, sshFxfWrite) {
		osFlags |= os.O_RDWR
	} else if p.hasPflags(sshFxfWrite) {
		osFlags |= os.O_WRONLY
	} else if p.hasPflags(sshFxfRead) {
		osFlags |= os.O_RDONLY
	} else {
		// how are they opening?
		return statusFromError(p.ID, syscall.EINVAL)
	}

	// Don't use O_APPEND flag as it conflicts with WriteAt.
	// The sshFxfAppend flag is a no-op here as the client sends the offsets.

	if p.hasPflags(sshFxfCreat) {
		osFlags |= os.O_CREATE
	}
	if p.hasPflags(sshFxfTrunc) {
		osFlags |= os.O_TRUNC
	}
	if p.hasPflags(sshFxfExcl) {
		osFlags |= os.O_EXCL
	}

	var mode os.FileMode = defaultFileMode
	var attributes *Attributes
	if p.Attrs != nil {
		attrs, _ := unmarshalFileStat(p.Flags, p.Attrs.([]byte))
		if p.Flags&sshFileXferAttrPermissions != 0 {
			mode = toFileMode(attrs.Mode)
			attributes = &Attributes{
				Permissions: &mode,
			}
		}
	}

	f, err := os.OpenFile(toLocalPath(p.Path), osFlags, mode)
	svr.reqCallback(RequestPacket{
		Type:       Open,
		Path:       p.Path,
		Flags:      p.Pflags,
		Attributes: attributes,
		Err:        err,
	})
	if err != nil {
		return statusFromError(p.ID, err)
	}

	handle := svr.nextHandle(f)
	return &sshFxpHandlePacket{ID: p.ID, Handle: handle}
}

func (p *sshFxpReaddirPacket) respond(svr *Server) responsePacket {
	f, ok := svr.getHandle(p.Handle)
	if !ok {
		return statusFromError(p.ID, EBADF)
	}

	dirents, err := f.Readdir(128)
	svr.reqCallback(RequestPacket{
		Type: Readdir,
		Path: f.Name(),
		Err:  err,
	})
	if err != nil {
		return statusFromError(p.ID, err)
	}

	idLookup := osIDLookup{}

	ret := &sshFxpNamePacket{ID: p.ID}
	for _, dirent := range dirents {
		ret.NameAttrs = append(ret.NameAttrs, &sshFxpNameAttr{
			Name:     dirent.Name(),
			LongName: runLs(idLookup, dirent),
			Attrs:    []interface{}{dirent},
		})
	}
	return ret
}

func (p *sshFxpSetstatPacket) respond(svr *Server) responsePacket {
	// additional unmarshalling is required for each possibility here
	b := p.Attrs.([]byte)

	var (
		err         error
		fileSize    *uint64
		permissions *uint32
		accessTime  *time.Time
		modTime     *time.Time
		fileUID     *uint32
		fileGID     *uint32
	)

	p.Path = toLocalPath(p.Path)

	debug("setstat name \"%s\"", p.Path)
	if (p.Flags & sshFileXferAttrSize) != 0 {
		var size uint64
		if size, b, err = unmarshalUint64Safe(b); err == nil {
			err = os.Truncate(p.Path, int64(size))
			fileSize = &size
		}
	}
	if (p.Flags & sshFileXferAttrPermissions) != 0 {
		var mode uint32
		if mode, b, err = unmarshalUint32Safe(b); err == nil {
			err = os.Chmod(p.Path, os.FileMode(mode))
			permissions = &mode
		}
	}
	if (p.Flags & sshFileXferAttrACmodTime) != 0 {
		var atime uint32
		var mtime uint32
		if atime, b, err = unmarshalUint32Safe(b); err != nil {
		} else if mtime, b, err = unmarshalUint32Safe(b); err != nil {
		} else {
			atimeT := time.Unix(int64(atime), 0)
			accessTime = &atimeT
			mtimeT := time.Unix(int64(mtime), 0)
			modTime = &mtimeT
			err = os.Chtimes(p.Path, atimeT, mtimeT)
		}
	}
	if (p.Flags & sshFileXferAttrUIDGID) != 0 {
		var uid uint32
		var gid uint32
		if uid, b, err = unmarshalUint32Safe(b); err != nil {
		} else if gid, _, err = unmarshalUint32Safe(b); err != nil {
		} else {
			err = os.Chown(p.Path, int(uid), int(gid))
			fileUID = &uid
			fileGID = &gid
		}
	}
	svr.reqCallback(RequestPacket{
		Type: Setstat,
		Path: p.Path,
		Attributes: &Attributes{
			Size:             fileSize,
			UID:              fileUID,
			GID:              fileGID,
			Permissions:      (*fs.FileMode)(permissions),
			AccessTime:       accessTime,
			ModificationTime: modTime,
		},
		Err: err,
	})

	return statusFromError(p.ID, err)
}

func (p *sshFxpFsetstatPacket) respond(svr *Server) responsePacket {
	f, ok := svr.getHandle(p.Handle)
	if !ok {
		return statusFromError(p.ID, EBADF)
	}

	// additional unmarshalling is required for each possibility here
	b := p.Attrs.([]byte)

	var (
		err         error
		fileSize    *uint64
		permissions *uint32
		accessTime  *time.Time
		modTime     *time.Time
		fileUID     *uint32
		fileGID     *uint32
	)

	debug("fsetstat name \"%s\"", f.Name())
	if (p.Flags & sshFileXferAttrSize) != 0 {
		var size uint64
		if size, b, err = unmarshalUint64Safe(b); err == nil {
			err = f.Truncate(int64(size))
			fileSize = &size
		}
	}
	if (p.Flags & sshFileXferAttrPermissions) != 0 {
		var mode uint32
		if mode, b, err = unmarshalUint32Safe(b); err == nil {
			err = f.Chmod(os.FileMode(mode))
			permissions = &mode
		}
	}
	if (p.Flags & sshFileXferAttrACmodTime) != 0 {
		var atime uint32
		var mtime uint32
		if atime, b, err = unmarshalUint32Safe(b); err != nil {
		} else if mtime, b, err = unmarshalUint32Safe(b); err != nil {
		} else {
			atimeT := time.Unix(int64(atime), 0)
			accessTime = &atimeT
			mtimeT := time.Unix(int64(mtime), 0)
			modTime = &mtimeT
			err = os.Chtimes(f.Name(), atimeT, mtimeT)
		}
	}
	if (p.Flags & sshFileXferAttrUIDGID) != 0 {
		var uid uint32
		var gid uint32
		if uid, b, err = unmarshalUint32Safe(b); err != nil {
		} else if gid, _, err = unmarshalUint32Safe(b); err != nil {
		} else {
			err = f.Chown(int(uid), int(gid))
			fileUID = &uid
			fileGID = &gid
		}
	}
	svr.reqCallback(RequestPacket{
		Type: Fsetstat,
		Path: f.Name(),
		Attributes: &Attributes{
			Size:             fileSize,
			UID:              fileUID,
			GID:              fileGID,
			Permissions:      (*fs.FileMode)(permissions),
			AccessTime:       accessTime,
			ModificationTime: modTime,
		},
		Err: err,
	})

	return statusFromError(p.ID, err)
}

func statusFromError(id uint32, err error) *sshFxpStatusPacket {
	ret := &sshFxpStatusPacket{
		ID: id,
		StatusError: StatusError{
			// sshFXOk               = 0
			// sshFXEOF              = 1
			// sshFXNoSuchFile       = 2 ENOENT
			// sshFXPermissionDenied = 3
			// sshFXFailure          = 4
			// sshFXBadMessage       = 5
			// sshFXNoConnection     = 6
			// sshFXConnectionLost   = 7
			// sshFXOPUnsupported    = 8
			Code: sshFxOk,
		},
	}
	if err == nil {
		return ret
	}

	debug("statusFromError: error is %T %#v", err, err)
	ret.StatusError.Code = sshFxFailure
	ret.StatusError.msg = err.Error()

	if errors.Is(err, os.ErrNotExist) {
		ret.StatusError.Code = sshFxNoSuchFile
		return ret
	}
	if errors.Is(err, os.ErrExist) {
		ret.StatusError.Code = sshFxFileAlreadyExists
		return ret
	}
	if code, ok := translateSyscallError(err); ok {
		ret.StatusError.Code = code
		return ret
	}

	switch e := err.(type) {
	case fxerr:
		ret.StatusError.Code = uint32(e)
	default:
		if e == io.EOF {
			ret.StatusError.Code = sshFxEOF
		}
	}

	return ret
}
