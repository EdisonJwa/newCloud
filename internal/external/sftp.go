package external

// The SFTP provider: version 3 of the SSH File Transfer Protocol, spoken over
// golang.org/x/crypto/ssh (a dependency this repository already carries). No SFTP
// library was added for this (ADR-0014): the v3 subset a mount needs — open, read,
// write, close, opendir, readdir, stat, remove, mkdir, rmdir, rename — is a small,
// stable wire protocol, and the codec below is shared by the loopback server the tests
// run against, so both sides are held to the same bytes.
//
// Sessions are per operation group, like FTP: the request's context bounds the SSH
// connection's lifetime, and nothing holds a remote session open between requests.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// SFTP packet types (draft-ietf-secsh-filexfer-02).
const (
	sftpInit     = 1
	sftpVersion  = 2
	sftpOpen     = 3
	sftpClose    = 4
	sftpRead     = 5
	sftpWrite    = 6
	sftpOpendir  = 11
	sftpReaddir  = 12
	sftpRemove   = 13
	sftpMkdir    = 14
	sftpRmdir    = 15
	sftpRealpath = 16
	sftpStat     = 17
	sftpRename   = 18

	sftpStatus  = 101
	sftpHandle  = 102
	sftpData    = 103
	sftpName    = 104
	sftpAttrsID = 105
)

// Status codes.
const (
	sftpOK           = 0
	sftpEOF          = 1
	sftpNoFile       = 2
	sftpDenied       = 3
	sftpFailure      = 4
	sftpBadMsg       = 5
	sftpUnsupported2 = 8
)

// Attribute flags.
const (
	sftpFlagSize   = 0x01
	sftpFlagPerms  = 0x04
	sftpFlagACTime = 0x08
)

const sftpDirMode = 0o40000

// ---- wire codec ----

type pkt struct {
	typ  byte
	body []byte
}

func (p *pkt) u32(v uint32) { p.body = binary.BigEndian.AppendUint32(p.body, v) }
func (p *pkt) u64(v uint64) { p.body = binary.BigEndian.AppendUint64(p.body, v) }
func (p *pkt) str(s string) {
	p.u32(uint32(len(s)))
	p.body = append(p.body, s...)
}

type pktReader struct {
	body []byte
	err  error
}

func (r *pktReader) u32() uint32 {
	if len(r.body) < 4 {
		r.err = errors.New("sftp: short packet")
		return 0
	}
	v := binary.BigEndian.Uint32(r.body)
	r.body = r.body[4:]
	return v
}

func (r *pktReader) u64() uint64 {
	if len(r.body) < 8 {
		r.err = errors.New("sftp: short packet")
		return 0
	}
	v := binary.BigEndian.Uint64(r.body)
	r.body = r.body[8:]
	return v
}

func (r *pktReader) str() string {
	n := r.u32()
	if r.err != nil || uint32(len(r.body)) < n {
		r.err = errors.New("sftp: short string")
		return ""
	}
	s := string(r.body[:n])
	r.body = r.body[n:]
	return s
}

func (r *pktReader) rest() []byte {
	out := r.body
	r.body = nil
	return out
}

// writeFrame writes one length-prefixed packet.
func writeFrame(w io.Writer, p *pkt) error {
	buf := make([]byte, 0, 1+len(p.body)+4)
	buf = binary.BigEndian.AppendUint32(buf, uint32(1+len(p.body)))
	buf = append(buf, p.typ)
	buf = append(buf, p.body...)
	_, err := w.Write(buf)
	return err
}

// readFrame reads one length-prefixed packet, refusing absurd sizes.
func readFrame(r io.Reader) (pkt, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return pkt{}, err
	}
	n := binary.BigEndian.Uint32(lenBuf[:])
	if n == 0 || n > 8<<20 {
		return pkt{}, fmt.Errorf("sftp: bad frame length %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return pkt{}, err
	}
	return pkt{typ: buf[0], body: buf[1:]}, nil
}

// sftpAttrs is the file-attribute record both STAT answers and READDIR entries carry.
type sftpFileAttrs struct {
	size    int64
	isDir   bool
	mtime   time.Time
	hasTime bool
}

func encodeAttrs(a sftpFileAttrs) *pkt {
	p := &pkt{}
	var flags uint32
	if a.size >= 0 {
		flags |= sftpFlagSize
	}
	flags |= sftpFlagPerms
	if a.hasTime {
		flags |= sftpFlagACTime
	}
	p.u32(flags)
	if a.size >= 0 {
		p.u64(uint64(a.size))
	}
	perms := uint32(0o644)
	if a.isDir {
		perms = 0o755 | sftpDirMode
	}
	p.u32(perms)
	if a.hasTime {
		p.u32(uint32(a.mtime.Unix()))
		p.u32(uint32(a.mtime.Unix()))
	}
	return p
}

func decodeAttrs(r *pktReader) sftpFileAttrs {
	var a sftpFileAttrs
	flags := r.u32()
	if flags&sftpFlagSize != 0 {
		a.size = int64(r.u64())
	}
	if flags&(sftpFlagPerms|0x02) != 0 {
		perms := r.u32()
		a.isDir = perms&sftpDirMode != 0
	}
	if flags&sftpFlagACTime != 0 {
		atime := r.u32()
		mtime := r.u32()
		_, _ = atime, mtime
		a.mtime = time.Unix(int64(mtime), 0)
		a.hasTime = true
	}
	return a
}

// ---- provider ----

// SFTP is a Storage over an SSH/SFTP server.
type SFTP struct {
	addr string // host:port
	user string
	pass string
	root string
}

// DialSFTP builds the provider. endpoint is sftp://host[:port].
func DialSFTP(ctx context.Context, endpoint, rootPath, username, password string) (Storage, error) {
	rest, ok := strings.CutPrefix(endpoint, "sftp://")
	if !ok {
		return nil, fmt.Errorf("external: sftp endpoint must be sftp://host[:port], got %q", endpoint)
	}
	addr := rest
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, "22")
	}
	return &SFTP{addr: addr, user: username, pass: password, root: strings.Trim(rootPath, "/")}, nil
}

// session is one SSH connection with an SFTP channel open on it.
type sftpSession struct {
	ssh  ssh.Conn
	sess *ssh.Session
	w    io.WriteCloser
	r    io.Reader
	next uint32
}

func (s *SFTP) dial(ctx context.Context) (*sftpSession, error) {
	cfg := ssh.ClientConfig{
		User:            s.user,
		Auth:            []ssh.AuthMethod{ssh.Password(s.pass)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // mounts trust the network they were pointed at; recording the key at create is future work (ADR-0014 consequences)
		Timeout:         15 * time.Second,
	}
	deadline, has := ctx.Deadline()
	d := net.Dialer{Timeout: cfg.Timeout}
	netConn, err := d.DialContext(ctx, "tcp", s.addr)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if has {
		netConn.SetDeadline(deadline)
	} else {
		netConn.SetDeadline(time.Now().Add(60 * time.Second))
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(netConn, s.addr, &cfg)
	if err != nil {
		netConn.Close()
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	client := ssh.NewClient(sshConn, chans, reqs)
	sess, err := client.NewSession()
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	w, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		client.Close()
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	r, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		client.Close()
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err := sess.RequestSubsystem("sftp"); err != nil {
		sess.Close()
		client.Close()
		return nil, fmt.Errorf("%w: no sftp subsystem: %v", ErrUnsupported, err)
	}
	out := &sftpSession{ssh: client, sess: sess, w: w, r: r, next: 1}
	init := &pkt{typ: sftpInit}
	init.u32(3) // protocol version 3
	if err := writeFrame(w, init); err != nil {
		out.close()
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	ver, err := readFrame(r)
	if err != nil || ver.typ != sftpVersion {
		out.close()
		return nil, fmt.Errorf("%w: bad handshake %v", ErrUnavailable, err)
	}
	return out, nil
}

func (s *sftpSession) close() {
	if s.sess != nil {
		s.sess.Close()
	}
	if s.ssh != nil {
		s.ssh.Close()
	}
}

// call sends one request and reads until its answer arrives. SFTP servers answer in
// order for a single client, so reading until the id matches is exact, not hopeful.
func (s *sftpSession) call(p *pkt) (pkt, error) {
	id := s.next
	s.next++
	body := append([]byte{}, byte(0), byte(0), byte(0), byte(0))
	body = binary.BigEndian.AppendUint32(body[:0], id)
	body = append(body, p.body...)
	p.body = body
	if err := writeFrame(s.w, p); err != nil {
		return pkt{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	for {
		resp, err := readFrame(s.r)
		if err != nil {
			return pkt{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		r := &pktReader{body: resp.body}
		gotID := r.u32()
		if r.err != nil || gotID != id {
			continue // a stale answer from an aborted request; keep reading
		}
		resp.body = r.rest()
		return resp, nil
	}
}

// statusErr maps a STATUS packet to the provider vocabulary.
//
// The body arrives with the request id already stripped by call, so the status code is
// the first field. (An earlier version skipped a second u32 "for the id" here, which
// shifted every read: an EOF's code slot held the message length and the whole error
// mapping silently answered the wrong question.)
func statusErr(p pkt) error {
	r := &pktReader{body: p.body}
	code := r.u32()
	msg := r.str()
	switch code {
	case sftpOK:
		return nil
	case sftpEOF:
		return io.EOF
	case sftpNoFile:
		return ErrNotFound
	case sftpDenied:
		return ErrPermission
	case sftpBadMsg, sftpUnsupported2:
		return fmt.Errorf("%w: %s", ErrUnsupported, msg)
	default:
		return fmt.Errorf("%w: %s", ErrUnavailable, msg)
	}
}

func (s *SFTP) full(rel string) string {
	if s.root == "" {
		return "/" + strings.Trim(rel, "/")
	}
	return "/" + s.root + "/" + strings.Trim(rel, "/")
}

func (s *SFTP) List(ctx context.Context, dir string) ([]Entry, error) {
	sess, err := s.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer sess.close()

	open := &pkt{typ: sftpOpendir}
	open.str(s.full(dir))
	resp, err := sess.call(open)
	if err != nil {
		return nil, err
	}
	if resp.typ == sftpStatus {
		return nil, statusErr(resp)
	}
	hr := &pktReader{body: resp.body}
	handle := hr.str()

	var out []Entry
	for {
		rd := &pkt{typ: sftpReaddir}
		rd.str(handle)
		resp, err := sess.call(rd)
		if err != nil {
			return nil, err
		}
		if resp.typ == sftpStatus {
			if errors.Is(statusErr(resp), io.EOF) {
				break
			}
			return nil, statusErr(resp)
		}
		if resp.typ != sftpName {
			return nil, fmt.Errorf("%w: unexpected packet %d in listing", ErrUnavailable, resp.typ)
		}
		nr := &pktReader{body: resp.body}
		count := nr.u32()
		for i := uint32(0); i < count; i++ {
			name := nr.str()
			_ = nr.str() // longname
			attrs := decodeAttrs(nr)
			if name == "." || name == ".." {
				continue
			}
			out = append(out, Entry{Name: name, IsDir: attrs.isDir, Size: attrs.size, ModTime: attrs.mtime})
		}
		if nr.err != nil {
			return nil, fmt.Errorf("%w: truncated listing", ErrUnavailable)
		}
	}
	closePkt := &pkt{typ: sftpClose}
	closePkt.str(handle)
	if _, err := sess.call(closePkt); err != nil {
		return nil, err
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Name < out[j-1].Name; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

func (s *SFTP) Stat(ctx context.Context, rel string) (Entry, error) {
	sess, err := s.dial(ctx)
	if err != nil {
		return Entry{}, err
	}
	defer sess.close()
	st := &pkt{typ: sftpStat}
	st.str(s.full(rel))
	resp, err := sess.call(st)
	if err != nil {
		return Entry{}, err
	}
	if resp.typ == sftpStatus {
		return Entry{}, statusErr(resp)
	}
	if resp.typ != sftpAttrsID {
		return Entry{}, fmt.Errorf("%w: unexpected stat packet %d", ErrUnavailable, resp.typ)
	}
	attrs := decodeAttrs(&pktReader{body: resp.body})
	name := rel
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		name = rel[i+1:]
	}
	return Entry{Name: name, IsDir: attrs.isDir, Size: attrs.size, ModTime: attrs.mtime}, nil
}

func (s *SFTP) Open(ctx context.Context, rel string) (io.ReadCloser, error) {
	sess, err := s.dial(ctx)
	if err != nil {
		return nil, err
	}
	open := &pkt{typ: sftpOpen}
	open.str(s.full(rel))
	open.u32(0x00000001) // SSH_FXF_READ
	open.u32(0)          // no attrs
	resp, err := sess.call(open)
	if err != nil {
		sess.close()
		return nil, err
	}
	if resp.typ == sftpStatus {
		sess.close()
		return nil, statusErr(resp)
	}
	hr := &pktReader{body: resp.body}
	handle := hr.str()
	return &sftpReader{sess: sess, handle: handle, off: 0}, nil
}

// sftpReader streams one file in protocol-sized chunks; the session lives until Close,
// because the handle is only usable on the channel it was opened on.
type sftpReader struct {
	sess   *sftpSession
	handle string
	off    uint64
	buf    []byte
	done   bool
}

// debugSFTP prints client-side listing parse details when set.
var debugSFTP = os.Getenv("SFTP_DEBUG") != ""

const newline = "\n"

const sftpChunk = 32768

func (r *sftpReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		if r.done {
			return 0, io.EOF
		}
		rd := &pkt{typ: sftpRead}
		rd.str(r.handle)
		rd.u64(r.off)
		rd.u32(sftpChunk)
		resp, err := r.sess.call(rd)
		if err != nil {
			return 0, err
		}
		if resp.typ == sftpStatus {
			if errors.Is(statusErr(resp), io.EOF) {
				r.done = true
				return 0, io.EOF
			}
			return 0, statusErr(resp)
		}
		if resp.typ != sftpData {
			return 0, fmt.Errorf("%w: unexpected read packet %d", ErrUnavailable, resp.typ)
		}
		// The payload is a length-prefixed string, like every other v3 field: taking
		// the raw rest handed the caller the four length bytes as file content.
		dr := &pktReader{body: resp.body}
		chunk := dr.str()
		if dr.err != nil {
			return 0, fmt.Errorf("%w: truncated data packet", ErrUnavailable)
		}
		if len(chunk) == 0 {
			r.done = true
			return 0, io.EOF
		}
		r.buf = []byte(chunk)
		r.off += uint64(len(chunk))
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func (r *sftpReader) Close() error {
	cl := &pkt{typ: sftpClose}
	cl.str(r.handle)
	_, _ = r.sess.call(cl)
	r.sess.close()
	return nil
}

func (s *SFTP) Create(ctx context.Context, rel string, rd io.Reader) (int64, error) {
	sess, err := s.dial(ctx)
	if err != nil {
		return 0, err
	}
	defer sess.close()
	open := &pkt{typ: sftpOpen}
	open.str(s.full(rel))
	open.u32(0x00000002 | 0x00000008 | 0x00000010) // WRITE | CREAT | TRUNC
	open.u32(0)
	resp, err := sess.call(open)
	if err != nil {
		return 0, err
	}
	if resp.typ == sftpStatus {
		return 0, statusErr(resp)
	}
	hr := &pktReader{body: resp.body}
	handle := hr.str()

	var total int64
	buf := make([]byte, sftpChunk)
	for {
		n, rerr := rd.Read(buf)
		if n > 0 {
			off := uint64(total)
			sent := 0
			for sent < n {
				wr := &pkt{typ: sftpWrite}
				wr.str(handle)
				wr.u64(off + uint64(sent))
				wr.str(string(buf[sent:n]))
				resp, err := sess.call(wr)
				if err != nil {
					return total, err
				}
				if resp.typ == sftpStatus {
					if serr := statusErr(resp); serr != nil {
						return total, serr
					}
				}
				sent = n
			}
			total += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return total, fmt.Errorf("%w: %v", ErrUnavailable, rerr)
		}
	}
	cl := &pkt{typ: sftpClose}
	cl.str(handle)
	if _, err := sess.call(cl); err != nil {
		return total, err
	}
	return total, nil
}

func (s *SFTP) Mkdir(ctx context.Context, rel string) error {
	sess, err := s.dial(ctx)
	if err != nil {
		return err
	}
	defer sess.close()
	mk := &pkt{typ: sftpMkdir}
	mk.str(s.full(rel))
	mk.u32(0o755 | sftpDirMode)
	resp, err := sess.call(mk)
	if err != nil {
		return err
	}
	if resp.typ == sftpStatus {
		serr := statusErr(resp)
		// v3 has no "exists" code; FAILURE on a create whose parent exists is the
		// every-server answer for a taken name.
		if errors.Is(serr, ErrUnavailable) {
			return ErrExists
		}
		return serr
	}
	return nil
}

func (s *SFTP) Remove(ctx context.Context, rel string, recursive bool) error {
	sess, err := s.dial(ctx)
	if err != nil {
		return err
	}
	defer sess.close()
	return s.removeIn(sess, rel, recursive)
}

func (s *SFTP) removeIn(sess *sftpSession, rel string, recursive bool) error {
	rm := &pkt{typ: sftpRemove}
	rm.str(s.full(rel))
	resp, err := sess.call(rm)
	if err != nil {
		return err
	}
	if resp.typ != sftpStatus {
		return nil
	}
	if serr := statusErr(resp); serr == nil {
		return nil
	} else if !errors.Is(serr, ErrNotFound) {
		return serr
	}
	// Not a file: a directory, or missing. RMDIR distinguishes them.
	rmd := &pkt{typ: sftpRmdir}
	rmd.str(s.full(rel))
	resp, err = sess.call(rmd)
	if err != nil {
		return err
	}
	if resp.typ != sftpStatus {
		return nil
	}
	serr := statusErr(resp)
	if serr == nil {
		return nil
	}
	if errors.Is(serr, ErrNotFound) {
		return ErrNotFound
	}
	if !recursive {
		return ErrExists // "not empty" in v3's only vocabulary for it
	}
	entries, lerr := s.listIn(sess, rel)
	if lerr != nil {
		return lerr
	}
	for _, e := range entries {
		if rerr := s.removeIn(sess, rel+"/"+e.Name, true); rerr != nil {
			return rerr
		}
	}
	rmd = &pkt{typ: sftpRmdir}
	rmd.str(s.full(rel))
	resp, err = sess.call(rmd)
	if err != nil {
		return err
	}
	if resp.typ == sftpStatus {
		return statusErr(resp)
	}
	return nil
}

// listIn is List on an existing session, for the recursive remove.
func (s *SFTP) listIn(sess *sftpSession, dir string) ([]Entry, error) {
	open := &pkt{typ: sftpOpendir}
	open.str(s.full(dir))
	resp, err := sess.call(open)
	if err != nil {
		return nil, err
	}
	if resp.typ == sftpStatus {
		return nil, statusErr(resp)
	}
	hr := &pktReader{body: resp.body}
	handle := hr.str()
	defer func() {
		cl := &pkt{typ: sftpClose}
		cl.str(handle)
		_, _ = sess.call(cl)
	}()
	var out []Entry
	for {
		rd := &pkt{typ: sftpReaddir}
		rd.str(handle)
		resp, err := sess.call(rd)
		if err != nil {
			return nil, err
		}
		if resp.typ == sftpStatus {
			if errors.Is(statusErr(resp), io.EOF) {
				return out, nil
			}
			return nil, statusErr(resp)
		}
		if resp.typ != sftpName {
			return nil, fmt.Errorf("%w: listing packet %d", ErrUnavailable, resp.typ)
		}
		nr := &pktReader{body: resp.body}
		count := nr.u32()
		for i := uint32(0); i < count; i++ {
			name := nr.str()
			_ = nr.str()
			attrs := decodeAttrs(nr)
			if name == "." || name == ".." {
				continue
			}
			out = append(out, Entry{Name: name, IsDir: attrs.isDir, Size: attrs.size})
		}
	}
}

func (s *SFTP) Rename(ctx context.Context, oldRel, newRel string) error {
	sess, err := s.dial(ctx)
	if err != nil {
		return err
	}
	defer sess.close()
	rn := &pkt{typ: sftpRename}
	rn.str(s.full(oldRel))
	rn.str(s.full(newRel))
	resp, err := sess.call(rn)
	if err != nil {
		return err
	}
	if resp.typ == sftpStatus {
		serr := statusErr(resp)
		// v3 rename refuses an existing destination with FAILURE.
		if errors.Is(serr, ErrUnavailable) {
			return ErrExists
		}
		return serr
	}
	return nil
}

func (s *SFTP) Ping(ctx context.Context) error {
	sess, err := s.dial(ctx)
	if err != nil {
		return err
	}
	// REALPATH "." proves the channel answers real requests, not just handshakes.
	rp := &pkt{typ: sftpRealpath}
	rp.str(".")
	resp, err := sess.call(rp)
	if err != nil {
		sess.close()
		return err
	}
	sess.close()
	if resp.typ == sftpStatus {
		return statusErr(resp)
	}
	return nil
}

var _ Storage = (*SFTP)(nil)

// keep strconv referenced for future error messages with ports.
var _ = strconv.Itoa
