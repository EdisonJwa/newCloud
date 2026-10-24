package external

// A loopback SSH server whose "sftp" subsystem is an in-memory filesystem speaking the
// same v3 codec as the client above. Sharing the codec is the point: both sides are held
// to one definition of the wire format, and the test exercises the client's bytes against
// a real SSH transport (kex, auth, channel, subsystem) rather than a pipe.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

type sftpNode struct {
	isDir bool
	body  []byte
}

type sftpServer struct {
	t  *testing.T
	ln net.Listener
	mu sync.Mutex
	fs map[string]*sftpNode // clean absolute path; "/" = root
}

func startSFTPServer(t *testing.T) *sftpServer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if c.User() == "svc" && string(pass) == "opensesame" {
				return nil, nil
			}
			return nil, errors.New("bad credentials")
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &sftpServer{t: t, ln: ln, fs: map[string]*sftpNode{"/": {isDir: true}}}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serveConn(conn, cfg)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *sftpServer) addFile(p string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fs[path.Clean("/"+p)] = &sftpNode{body: body}
}

func (s *sftpServer) addDir(p string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fs[path.Clean("/"+p)] = &sftpNode{isDir: true}
}

func (s *sftpServer) exists(p string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.fs[path.Clean("/"+p)]
	return ok
}

func (s *sftpServer) serveConn(netConn net.Conn, cfg *ssh.ServerConfig) {
	sshConn, chans, reqs, err := ssh.NewServerConn(netConn, cfg)
	if err != nil {
		netConn.Close()
		return
	}
	defer sshConn.Close()
	go ssh.DiscardRequests(reqs)
	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			newChan.Reject(ssh.UnknownChannelType, "sessions only")
			continue
		}
		ch, requests, err := newChan.Accept()
		if err != nil {
			continue
		}
		go s.serveSession(ch, requests)
	}
}

func (s *sftpServer) serveSession(ch ssh.Channel, requests <-chan *ssh.Request) {
	defer ch.Close()
	for req := range requests {
		if os.Getenv("SFTP_DEBUG") != "" {
			s.t.Logf("session req %q payload %q", req.Type, req.Payload)
		}
		if req.Type == "subsystem" && string(req.Payload[4:]) == "sftp" {
			if req.Reply(true, nil) != nil {
				return
			}
			s.serveSFTP(ch)
			return
		}
		req.Reply(false, nil)
	}
}

// serveSFTP is the server half of the v3 protocol, written against the same codec.
func (s *sftpServer) serveSFTP(ch ssh.Channel) {
	type handle struct {
		path    string
		isDir   bool
		off     uint64
		readIdx int
	}
	handles := map[string]*handle{}
	nextHandle := 0
	reply := func(id uint32, typ byte, body []byte) {
		frame := binary.BigEndian.AppendUint32(nil, id)
		frame = append(frame, body...)
		err := writeFrame(ch, &pkt{typ: typ, body: frame})
		if os.Getenv("SFTP_DEBUG") != "" {
			s.t.Logf("sftp reply id=%d typ=%d err=%v", id, typ, err)
		}
	}

	for {
		frame, err := readFrame(ch)
		if os.Getenv("SFTP_DEBUG") != "" {
			s.t.Logf("sftp frame typ=%d err=%v", frame.typ, err)
		}
		if err != nil {
			return
		}
		r := &pktReader{body: frame.body}
		id := r.u32()
		switch frame.typ {
		case sftpInit:
			ver := &pkt{}
			ver.u32(3)
			reply(0, sftpVersion, ver.body)
		case sftpOpen:
			_ = id // id
			p := r.str()
			pflags := r.u32()
			const (
				flagCreat = 0x08
				flagTrunc = 0x10
			)
			clean := path.Clean(p)
			s.mu.Lock()
			node := s.fs[clean]
			if node == nil && pflags&flagCreat != 0 {
				// A create-open makes the file; the parent must exist, as on a real server.
				if _, parentOK := s.fs[path.Dir(clean)]; parentOK {
					node = &sftpNode{}
					s.fs[clean] = node
				}
			}
			if node != nil && !node.isDir && pflags&flagTrunc != 0 {
				node.body = nil
			}
			s.mu.Unlock()
			if node == nil || node.isDir {
				st := &pkt{}
				st.u32(sftpFailure)
				st.str("no such file")
				reply(id, sftpStatus, st.body)
				continue
			}
			nextHandle++
			h := &handle{path: path.Clean(p)}
			key := "h" + itoa(nextHandle)
			handles[key] = h
			hr := &pkt{}
			hr.str(key)
			reply(id, sftpHandle, hr.body)
		case sftpRead:
			_ = id
			key := r.str()
			off := r.u64()
			h := handles[key]
			if h == nil {
				st := &pkt{}
				st.u32(sftpFailure)
				st.str("bad handle")
				reply(id, sftpStatus, st.body)
				continue
			}
			s.mu.Lock()
			body := s.fs[h.path].body
			s.mu.Unlock()
			if int(off) >= len(body) {
				st := &pkt{}
				st.u32(sftpEOF)
				st.str("eof")
				reply(id, sftpStatus, st.body)
				continue
			}
			end := int(off) + 32768
			if end > len(body) {
				end = len(body)
			}
			dr := &pkt{}
			dr.str(string(body[off:end]))
			reply(id, sftpData, dr.body)
		case sftpWrite:
			_ = id
			key := r.str()
			off := r.u64()
			data := r.str()
			h := handles[key]
			if h == nil {
				st := &pkt{}
				st.u32(sftpFailure)
				reply(id, sftpStatus, st.body)
				continue
			}
			s.mu.Lock()
			n := s.fs[h.path]
			if int(off)+len(data) > len(n.body) {
				grown := make([]byte, int(off)+len(data))
				copy(grown, n.body)
				n.body = grown
			}
			copy(n.body[off:], data)
			s.mu.Unlock()
			st := &pkt{}
			st.u32(sftpOK)
			reply(id, sftpStatus, st.body)
		case sftpClose:
			_ = id
			key := r.str()
			delete(handles, key)
			st := &pkt{}
			st.u32(sftpOK)
			reply(id, sftpStatus, st.body)
		case sftpOpendir:
			_ = id
			p := r.str()
			clean := path.Clean(p)
			s.mu.Lock()
			node := s.fs[clean]
			s.mu.Unlock()
			if node == nil || !node.isDir {
				st := &pkt{}
				st.u32(sftpNoFile)
				reply(id, sftpStatus, st.body)
				continue
			}
			nextHandle++
			h := &handle{path: clean, isDir: true}
			key := "h" + itoa(nextHandle)
			handles[key] = h
			hr := &pkt{}
			hr.str(key)
			reply(id, sftpHandle, hr.body)
		case sftpReaddir:
			_ = id
			key := r.str()
			h := handles[key]
			if h == nil || !h.isDir {
				st := &pkt{}
				st.u32(sftpFailure)
				reply(id, sftpStatus, st.body)
				continue
			}
			s.mu.Lock()
			var names []string
			for p := range s.fs {
				if p == h.path {
					continue
				}
				if path.Dir(p) != h.path {
					continue
				}
				names = append(names, p)
			}
			// Deterministic order: the client sorts, the server need not.
			for i := 1; i < len(names); i++ {
				for j := i; j > 0 && names[j] < names[j-1]; j-- {
					names[j], names[j-1] = names[j-1], names[j]
				}
			}
			if h.readIdx >= len(names) {
				s.mu.Unlock()
				st := &pkt{}
				st.u32(sftpEOF)
				reply(id, sftpStatus, st.body)
				continue
			}
			nr := &pkt{}
			nr.u32(uint32(len(names) - h.readIdx))
			for _, p := range names[h.readIdx:] {
				node := s.fs[p]
				nr.str(path.Base(p))
				nr.str(path.Base(p))
				nr.body = append(nr.body, encodeAttrs(sftpFileAttrs{
					size: int64(len(node.body)), isDir: node.isDir,
					mtime: time.Unix(1767000000, 0), hasTime: true,
				}).body...)
			}
			h.readIdx = len(names)
			s.mu.Unlock()
			reply(id, sftpName, nr.body)
		case sftpStat:
			_ = id
			p := r.str()
			clean := path.Clean(p)
			s.mu.Lock()
			node := s.fs[clean]
			s.mu.Unlock()
			if node == nil {
				st := &pkt{}
				st.u32(sftpNoFile)
				reply(id, sftpStatus, st.body)
				continue
			}
			attrs := encodeAttrs(sftpFileAttrs{size: int64(len(node.body)), isDir: node.isDir,
				mtime: time.Unix(1767000000, 0), hasTime: true})
			reply(id, sftpAttrsID, attrs.body)
		case sftpRemove:
			_ = id
			p := r.str()
			clean := path.Clean(p)
			s.mu.Lock()
			node := s.fs[clean]
			if node != nil && !node.isDir {
				delete(s.fs, clean)
			}
			s.mu.Unlock()
			st := &pkt{}
			if node != nil && !node.isDir {
				st.u32(sftpOK)
			} else {
				st.u32(sftpNoFile)
			}
			reply(id, sftpStatus, st.body)
		case sftpMkdir:
			_ = id
			p := r.str()
			clean := path.Clean(p)
			s.mu.Lock()
			if _, exists := s.fs[clean]; exists {
				s.mu.Unlock()
				st := &pkt{}
				st.u32(sftpFailure)
				reply(id, sftpStatus, st.body)
				continue
			}
			s.fs[clean] = &sftpNode{isDir: true}
			s.mu.Unlock()
			st := &pkt{}
			st.u32(sftpOK)
			reply(id, sftpStatus, st.body)
		case sftpRmdir:
			_ = id
			p := r.str()
			clean := path.Clean(p)
			s.mu.Lock()
			node := s.fs[clean]
			empty := true
			for q := range s.fs {
				if strings.HasPrefix(q, clean+"/") {
					empty = false
					break
				}
			}
			if node != nil && node.isDir && empty {
				delete(s.fs, clean)
			}
			s.mu.Unlock()
			st := &pkt{}
			switch {
			case node != nil && node.isDir && empty:
				st.u32(sftpOK)
			case node == nil:
				st.u32(sftpNoFile)
			default:
				st.u32(sftpFailure)
			}
			reply(id, sftpStatus, st.body)
		case sftpRename:
			_ = id
			from := r.str()
			to := r.str()
			s.mu.Lock()
			node := s.fs[path.Clean(from)]
			_, destExists := s.fs[path.Clean(to)]
			if node != nil && !destExists {
				delete(s.fs, path.Clean(from))
				s.fs[path.Clean(to)] = node
			}
			s.mu.Unlock()
			st := &pkt{}
			if node == nil {
				st.u32(sftpNoFile)
			} else if destExists {
				st.u32(sftpFailure)
			} else {
				st.u32(sftpOK)
			}
			reply(id, sftpStatus, st.body)
		case sftpRealpath:
			_ = id
			p := r.str()
			nr := &pkt{}
			nr.u32(1)
			nr.str(path.Clean("/" + p))
			nr.str(path.Clean("/" + p))
			nr.body = append(nr.body, encodeAttrs(sftpFileAttrs{isDir: true}).body...)
			reply(id, sftpName, nr.body)
		default:
			st := &pkt{}
			st.u32(sftpUnsupported2)
			reply(id, sftpStatus, st.body)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func TestSFTPProviderAgainstTheLoopbackServer(t *testing.T) {
	srv := startSFTPServer(t)
	srv.addDir("/docs")
	srv.addFile("/docs/readme.txt", []byte("over sftp"))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	storage, err := DialSFTP(ctx, "sftp://"+srv.ln.Addr().String(), "", "svc", "opensesame")
	if err != nil {
		t.Fatal(err)
	}

	if err := storage.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	entries, err := storage.List(ctx, "docs")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "readme.txt" || entries[0].Size != 9 {
		t.Fatalf("list wrong: %+v", entries)
	}

	st, err := storage.Stat(ctx, "docs/readme.txt")
	if err != nil || st.IsDir || st.Size != 9 {
		t.Fatalf("stat: %+v %v", st, err)
	}
	if _, err := storage.Stat(ctx, "docs/none.txt"); err == nil {
		t.Fatal("missing file must stat as not found")
	}

	rc, err := storage.Open(ctx, "docs/readme.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(rc)
	rc.Close()
	if string(body) != "over sftp" {
		t.Fatalf("read %q", string(body))
	}

	n, err := storage.Create(ctx, "docs/written.txt", strings.NewReader("written over sftp"))
	if err != nil || n != int64(len("written over sftp")) {
		t.Fatalf("create: %d %v", n, err)
	}
	// Read it back through a second session: the write really landed.
	rc, err = storage.Open(ctx, "docs/written.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(rc)
	rc.Close()
	if string(body) != "written over sftp" {
		t.Fatalf("round trip: %q", string(body))
	}

	if err := storage.Mkdir(ctx, "newdir"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := storage.Rename(ctx, "docs/written.txt", "docs/renamed.txt"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := storage.Remove(ctx, "docs/renamed.txt", true); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if srv.exists("/docs/renamed.txt") {
		t.Fatal("the file survived the delete")
	}

	// Recursive remove.
	srv.addDir("/deep")
	srv.addDir("/deep/inner")
	srv.addFile("/deep/inner/file.txt", []byte("x"))
	if err := storage.Remove(ctx, "deep", true); err != nil {
		t.Fatalf("recursive remove: %v", err)
	}
	if srv.exists("/deep") {
		t.Fatal("the tree survived the recursive delete")
	}
}
