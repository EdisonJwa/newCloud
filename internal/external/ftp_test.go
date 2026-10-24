package external_test

// An in-process FTP server, speaking exactly the commands the provider issues, backed by
// an in-memory filesystem. The point is not RFC completeness: it is that the provider's
// bytes on the wire — command spellings, EPSV parsing, MLSD facts, the RETR/STOR
// completion dance — are exercised against a real TCP conversation rather than mocked
// function calls.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EdisonJwa/newCloud/internal/external"
)

type ftpEntry struct {
	isDir bool
	body  []byte
}

type ftpServer struct {
	t  *testing.T
	ln net.Listener

	mu    sync.Mutex
	files map[string]*ftpEntry // clean path, "" = root
}

func startFTPServer(t *testing.T) *ftpServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &ftpServer{t: t, ln: ln, files: map[string]*ftpEntry{"": {isDir: true}}}
	go s.acceptLoop()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *ftpServer) addr() string { return s.ln.Addr().String() }

func (s *ftpServer) addFile(path string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[path] = &ftpEntry{body: body}
}

func (s *ftpServer) addDir(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[path] = &ftpEntry{isDir: true}
}

func (s *ftpServer) exists(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.files[path]
	return ok
}

func (s *ftpServer) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		c := &ftpConn{srv: s, conn: conn}
		go c.serve()
	}
}

// ftpConn is one control session. All of its state — the pending data connection, the
// rename source — lives here, because FTP makes both per-session and the provider opens
// one session per operation group.
type ftpConn struct {
	srv  *ftpServer
	conn net.Conn

	writeMu sync.Mutex
	dataCh  chan net.Conn
	rename  string
}

func (c *ftpConn) line(format string, args ...any) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if os.Getenv("FTP_DEBUG") != "" {
		c.srv.t.Logf("S: "+format, args...)
	}
	fmt.Fprintf(c.conn, format+"\r\n", args...)
}

// announcePassive listens, publishes the port, and accepts in the background: the FTP
// ordering is announce-then-dial, so the accept can only complete after the reply.
func (c *ftpConn) announcePassive() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	c.dataCh = make(chan net.Conn, 1)
	go func() {
		defer ln.Close()
		dconn, aerr := ln.Accept()
		if aerr != nil {
			close(c.dataCh)
			return
		}
		c.dataCh <- dconn
	}()
	return port, nil
}

func (c *ftpConn) serve() {
	defer c.conn.Close()
	c.line("220 test FTP server")
	sc := bufio.NewScanner(c.conn)
	for sc.Scan() {
		verb, rest, _ := strings.Cut(sc.Text(), " ")
		if os.Getenv("FTP_DEBUG") != "" {
			c.srv.t.Logf("C: %s %s", verb, rest)
		}
		if !c.dispatch(strings.ToUpper(verb), rest) {
			return
		}
	}
}

// dispatch answers one command; false ends the session.
func (c *ftpConn) dispatch(verb, rest string) bool {
	s := c.srv
	switch verb {
	case "USER":
		c.line("331 password required")
	case "PASS":
		c.line("230 logged in")
	case "NOOP":
		c.line("200 noop")
	case "QUIT":
		c.line("221 bye")
		return false
	case "CWD":
		s.mu.Lock()
		f := s.files[strings.Trim(rest, "/")]
		s.mu.Unlock()
		if f != nil && f.isDir {
			c.line("250 ok")
		} else {
			c.line("550 no such directory")
		}
	case "EPSV":
		port, err := c.announcePassive()
		if err != nil {
			c.line("425 cannot open data connection")
			return true
		}
		c.line("229 Entering Extended Passive Mode (|||%d|)", port)
	case "PASV":
		port, err := c.announcePassive()
		if err != nil {
			c.line("425 cannot open data connection")
			return true
		}
		host, _, _ := net.SplitHostPort(c.conn.LocalAddr().String())
		parts := strings.Split(host, ".")
		var n [4]int
		for i := 0; i < 4; i++ {
			if i < len(parts) {
				n[i], _ = strconv.Atoi(parts[i])
			}
		}
		c.line("227 Entering Passive Mode (%d,%d,%d,%d,%d,%d)",
			n[0], n[1], n[2], n[3], port>>8, port&0xff)
	case "MLSD", "LIST":
		c.transferListing(verb, strings.Trim(rest, "./"))
	case "RETR":
		c.transferFile(strings.Trim(rest, "/"))
	case "STOR":
		c.receiveFile(strings.Trim(rest, "/"))
	case "SIZE":
		s.mu.Lock()
		f := s.files[strings.Trim(rest, "/")]
		s.mu.Unlock()
		if f == nil || f.isDir {
			c.line("550 no such file")
		} else {
			c.line("213 %d", len(f.body))
		}
	case "MKD":
		path := strings.Trim(rest, "/")
		s.mu.Lock()
		if _, exists := s.files[path]; exists {
			s.mu.Unlock()
			c.line("550 already exists")
		} else {
			s.files[path] = &ftpEntry{isDir: true}
			s.mu.Unlock()
			c.line("257 created")
		}
	case "DELE":
		path := strings.Trim(rest, "/")
		s.mu.Lock()
		f := s.files[path]
		if f != nil && !f.isDir {
			delete(s.files, path)
		}
		s.mu.Unlock()
		if f != nil && !f.isDir {
			c.line("250 deleted")
		} else {
			c.line("550 not a plain file")
		}
	case "RMD":
		path := strings.Trim(rest, "/")
		s.mu.Lock()
		f := s.files[path]
		empty := true
		for p := range s.files {
			if strings.HasPrefix(p, path+"/") {
				empty = false
				break
			}
		}
		if f != nil && f.isDir && empty {
			delete(s.files, path)
		}
		s.mu.Unlock()
		if f != nil && f.isDir && empty {
			c.line("250 removed")
		} else if f != nil && !f.isDir {
			c.line("550 not a directory")
		} else {
			c.line("550 directory not empty or missing")
		}
	case "RNFR":
		path := strings.Trim(rest, "/")
		s.mu.Lock()
		_, ok := s.files[path]
		s.mu.Unlock()
		if ok {
			c.rename = path
			c.line("350 ready for destination")
		} else {
			c.line("550 no such file")
		}
	case "RNTO":
		path := strings.Trim(rest, "/")
		s.mu.Lock()
		if _, exists := s.files[path]; exists {
			s.mu.Unlock()
			c.line("553 destination exists")
		} else {
			f := s.files[c.rename]
			delete(s.files, c.rename)
			s.files[path] = f
			s.mu.Unlock()
			c.line("250 renamed")
		}
	default:
		c.line("502 command not implemented: %s", verb)
	}
	return true
}

func (c *ftpConn) takeData() net.Conn {
	if c.dataCh == nil {
		return nil
	}
	select {
	case d := <-c.dataCh:
		return d
	case <-time.After(2 * time.Second):
		return nil
	}
}

func (c *ftpConn) transferListing(verb, dir string) {
	data := c.takeData()
	if data == nil {
		c.line("425 no data connection")
		return
	}
	s := c.srv
	s.mu.Lock()
	f := s.files[dir]
	if f == nil || !f.isDir {
		s.mu.Unlock()
		data.Close()
		c.line("550 no such directory")
		return
	}
	var names []string
	for p := range s.files {
		if p == "" || p == dir {
			continue
		}
		parent := ""
		if i := strings.LastIndex(p, "/"); i > 0 {
			parent = p[:i]
		}
		if parent != dir {
			continue
		}
		names = append(names, p)
	}
	sort.Strings(names)
	type row struct {
		name  string
		entry *ftpEntry
	}
	rows := make([]row, 0, len(names))
	for _, p := range names {
		rows = append(rows, row{strings.TrimPrefix(p, dir+"/"), s.files[p]})
	}
	s.mu.Unlock()

	c.line("150 here comes the listing")
	go func() {
		defer data.Close()
		w := bufio.NewWriter(data)
		for _, r := range rows {
			if verb == "MLSD" {
				kind := "file"
				if r.entry.isDir {
					kind = "dir"
				}
				fmt.Fprintf(w, "type=%s;size=%d;modify=20260102030405; %s\r\n", kind, len(r.entry.body), r.name)
			} else {
				flag := "-"
				if r.entry.isDir {
					flag = "d"
				}
				fmt.Fprintf(w, "%srw-r--r-- 1 owner group %d Jan 02 03:04 %s\r\n", flag, len(r.entry.body), r.name)
			}
		}
		w.Flush()
		c.line("226 transfer complete")
	}()
}

func (c *ftpConn) transferFile(path string) {
	data := c.takeData()
	if data == nil {
		c.line("425 no data connection")
		return
	}
	s := c.srv
	s.mu.Lock()
	f := s.files[path]
	s.mu.Unlock()
	if f == nil || f.isDir {
		data.Close()
		c.line("550 no such file")
		return
	}
	c.line("150 opening data connection")
	go func() {
		defer data.Close()
		_, _ = data.Write(f.body)
		c.line("226 transfer complete")
	}()
}

func (c *ftpConn) receiveFile(path string) {
	data := c.takeData()
	if data == nil {
		c.line("425 no data connection")
		return
	}
	c.line("150 ok to send data")
	go func() {
		defer data.Close()
		body, _ := io.ReadAll(data)
		s := c.srv
		s.mu.Lock()
		s.files[path] = &ftpEntry{body: body}
		s.mu.Unlock()
		c.line("226 transfer complete")
	}()
}

func TestFTPProviderAgainstTheTestServer(t *testing.T) {
	srv := startFTPServer(t)
	srv.addDir("docs")
	srv.addFile("docs/readme.txt", []byte("over ftp"))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	storage, err := external.DialFTP(ctx, "ftp://"+srv.addr(), "", "user", "pass")
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
	if len(entries) != 1 || entries[0].Name != "readme.txt" || entries[0].Size != 8 {
		t.Fatalf("list wrong: %+v", entries)
	}

	st, err := storage.Stat(ctx, "docs/readme.txt")
	if err != nil || st.IsDir || st.Size != 8 {
		t.Fatalf("stat: %+v %v", st, err)
	}
	if _, err := storage.Stat(ctx, "docs/missing.txt"); err == nil {
		t.Fatal("a missing file must stat as not found")
	}

	rc, err := storage.Open(ctx, "docs/readme.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(rc)
	rc.Close()
	if string(body) != "over ftp" {
		t.Fatalf("read %q", string(body))
	}

	n, err := storage.Create(ctx, "docs/written.txt", strings.NewReader("written via provider"))
	if err != nil || n != int64(len("written via provider")) {
		t.Fatalf("create: %d %v", n, err)
	}
	if !srv.exists("docs/written.txt") {
		t.Fatal("the upload never reached the server")
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
	if srv.exists("docs/renamed.txt") {
		t.Fatal("the file survived the delete")
	}

	// Recursive remove of a directory with contents.
	srv.addDir("deep")
	srv.addDir("deep/inner")
	srv.addFile("deep/inner/file.txt", []byte("x"))
	if err := storage.Remove(ctx, "deep", true); err != nil {
		t.Fatalf("recursive remove: %v", err)
	}
	if srv.exists("deep") {
		t.Fatal("the directory tree survived the recursive delete")
	}
}
