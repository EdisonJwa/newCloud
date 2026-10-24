package external

// The FTP provider: a small RFC 959 client speaking MLSD for listings, with a LIST
// fallback for servers that predate it.
//
// FTP has no per-operation contexts, so every call runs on the caller's deadline the only
// way FTP allows: the command connection is dialed per operation group and torn down
// after, which is also the polite thing to do to a server that counts sessions. Passive
// mode only (EPSV, falling back to PASV); active mode is a firewall hole no modern
// deployment should open for this.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"path"
	"strconv"
	"strings"
	"time"
)

// FTP is a Storage over an FTP server.
type FTP struct {
	addr string // host:port
	user string
	pass string
	tls  bool   // ftps:// dials TLS from the first byte (implicit FTPS)
	root string // mounted subtree below the login directory, "" for the login dir
}

// DialFTP builds the provider. endpoint is ftp://host:port or ftps://host:port.
func DialFTP(ctx context.Context, endpoint, rootPath, username, password string) (Storage, error) {
	u, err := parseHostPort(endpoint, "ftp", "ftps")
	if err != nil {
		return nil, err
	}
	// The address keeps its port when the endpoint carried one; port() supplies the
	// scheme's default only for a bare host.
	addr := u.host
	if u.port != "" {
		addr = net.JoinHostPort(u.host, u.port)
	}
	return &FTP{
		addr: addr, user: username, pass: password,
		tls: u.scheme == "ftps", root: strings.Trim(rootPath, "/"),
	}, nil
}

type hostPort struct {
	scheme, host, port string
}

func parseHostPort(endpoint string, schemes ...string) (hostPort, error) {
	for _, s := range schemes {
		if rest, ok := strings.CutPrefix(endpoint, s+"://"); ok {
			host, port, err := net.SplitHostPort(rest)
			if err != nil {
				// A bare host is legal; the caller's scheme supplies the default port.
				return hostPort{scheme: s, host: rest}, nil
			}
			return hostPort{scheme: s, host: host, port: port}, nil
		}
	}
	return hostPort{}, fmt.Errorf("external: endpoint must be %s, got %q",
		strings.Join(schemes, " or "), endpoint)
}

// session is one command connection. Every Storage method opens one, does its work, and
// quits; holding a session open across requests would couple the request lifecycle to the
// remote's idle timeout, which neither side configured.
type ftpSession struct {
	conn *textproto.Conn
	addr string
	ping bool // session reused for Ping only, so QUIT is still polite
}

func (f *FTP) dial(ctx context.Context) (*ftpSession, error) {
	d := net.Dialer{Timeout: 15 * time.Second}
	raw, err := d.DialContext(ctx, "tcp", f.dialAddr())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	deadline, hasDeadline := ctx.Deadline()
	if hasDeadline {
		raw.SetDeadline(deadline)
	} else {
		raw.SetDeadline(time.Now().Add(60 * time.Second))
	}
	conn := textproto.NewConn(raw)
	s := &ftpSession{conn: conn, addr: f.addr}

	code, msg, err := conn.ReadResponse(220)
	if err != nil || code != 220 {
		s.quit()
		return nil, fmt.Errorf("%w: greeting %d %s %v", ErrUnavailable, code, msg, err)
	}
	if f.tls {
		s.quit()
		return nil, fmt.Errorf("%w: implicit FTPS is not implemented; use ftp:// with the server's TLS upgrade or an explicit-FTPS gateway", ErrUnsupported)
	}
	if err := s.cmd("USER %s", f.user); err != nil {
		s.quit()
		return nil, err
	}
	if f.pass != "" {
		if err := s.cmd("PASS %s", f.pass); err != nil {
			s.quit()
			return nil, err
		}
	}
	if f.root != "" {
		// CWD to the mounted subtree so every later path is relative to it, exactly like
		// the other providers' rootPath.
		if err := s.cmd("CWD /%s", f.root); err != nil {
			s.quit()
			return nil, err
		}
	}
	return s, nil
}

// dialAddr is the address to dial: the endpoint's own host:port when it carried one,
// otherwise the bare host plus the scheme's default port.
func (f *FTP) dialAddr() string {
	if _, _, err := net.SplitHostPort(f.addr); err == nil {
		return f.addr
	}
	if f.tls {
		return net.JoinHostPort(f.addr, "990")
	}
	return net.JoinHostPort(f.addr, "21")
}

// cmd issues one command and requires a 2xx answer.
func (s *ftpSession) cmd(format string, args ...any) error {
	code, msg, err := s.cmdResponse(format, args...)
	if err != nil {
		return err
	}
	if code >= 500 && code < 600 {
		if code == 550 {
			return ErrNotFound
		}
		if code == 530 || code == 532 {
			return ErrPermission
		}
		return fmt.Errorf("external: server refused: %d %s", code, msg)
	}
	if code >= 400 {
		return fmt.Errorf("%w: %d %s", ErrUnavailable, code, msg)
	}
	return nil
}

func (s *ftpSession) cmdResponse(format string, args ...any) (int, string, error) {
	id, err := s.conn.Cmd(format, args...)
	if err != nil {
		return 0, "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	s.conn.StartResponse(id)
	defer s.conn.EndResponse(id)
	return s.conn.ReadResponse(0)
}

func (s *ftpSession) quit() {
	_, _ = s.conn.Cmd("QUIT")
	_ = s.conn.Close()
}

// passive opens the data connection for one transfer, EPSV first (no address family
// assumptions), PASV for the servers that never grew it.
func (s *ftpSession) passive(ctx context.Context) (net.Conn, error) {
	id, err := s.conn.Cmd("EPSV")
	if err == nil {
		s.conn.StartResponse(id)
		code, msg, rerr := s.conn.ReadResponse(0)
		s.conn.EndResponse(id)
		if rerr == nil && code == 229 {
			if port, ok := epsvPort(msg); ok {
				return net.DialTimeout("tcp", net.JoinHostPort(hostOnly(s.addr), strconv.Itoa(port)), 15*time.Second)
			}
		}
	}
	id, err = s.conn.Cmd("PASV")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	s.conn.StartResponse(id)
	code, msg, rerr := s.conn.ReadResponse(0)
	s.conn.EndResponse(id)
	if rerr != nil || code != 227 {
		return nil, fmt.Errorf("%w: PASV %d %s %v", ErrUnavailable, code, msg, rerr)
	}
	host, port, ok := pasvAddress(msg, hostOnly(s.addr))
	if !ok {
		return nil, fmt.Errorf("%w: unparseable PASV %q", ErrUnavailable, msg)
	}
	return net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 15*time.Second)
}

func hostOnly(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// epsvPort reads "(|||port|)".
func epsvPort(msg string) (int, bool) {
	open := strings.IndexByte(msg, '(')
	closeIdx := strings.LastIndexByte(msg, ')')
	if open < 0 || closeIdx < open {
		return 0, false
	}
	// "(|||port|)" splits into five fields: three empties, the port, one empty.
	fields := strings.Split(msg[open+1:closeIdx], "|")
	if len(fields) != 5 {
		return 0, false
	}
	n, err := strconv.Atoi(fields[3])
	return n, err == nil
}

// pasvAddress reads "h1,h2,h3,h4,p1,p2". Many modern servers return a bogus host in
// PASV (0.0.0.0 or an internal address); the command connection's peer is the truth, so
// the reply's port is used with the COMMAND host unless the reply is fully qualified and
// different — the behaviour every serious FTP client converged on.
func pasvAddress(msg, cmdHost string) (string, int, bool) {
	start := strings.IndexByte(msg, '(')
	end := strings.LastIndexByte(msg, ')')
	if start < 0 || end < start {
		start, end = 0, len(msg)
	}
	fields := strings.Split(strings.TrimSpace(msg[start+1:end]), ",")
	if len(fields) != 6 {
		return "", 0, false
	}
	var nums [6]int
	for i, f := range fields {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil {
			return "", 0, false
		}
		nums[i] = n
	}
	host := fmt.Sprintf("%d.%d.%d.%d", nums[0], nums[1], nums[2], nums[3])
	if host == "0.0.0.0" {
		host = cmdHost
	}
	return host, nums[4]<<8 | nums[5], true
}

func (f *FTP) rel(p string) string { return strings.Trim(p, "/") }

func (f *FTP) List(ctx context.Context, dir string) ([]Entry, error) {
	s, err := f.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer s.quit()

	entries, err := s.mlsd(ctx, f.rel(dir))
	if err == nil {
		return entries, nil
	}
	if !errors.Is(err, ErrUnsupported) {
		return nil, err
	}
	return s.list(ctx, f.rel(dir))
}

// mlsd reads an MLSD listing: "type=file;size=123;modify=20260101000000; name".
func (s *ftpSession) mlsd(ctx context.Context, dir string) ([]Entry, error) {
	data, err := s.passive(ctx)
	if err != nil {
		return nil, err
	}
	defer data.Close()
	id, err := s.conn.Cmd("MLSD %s", "./"+dir)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	s.conn.StartResponse(id)
	code, msg, rerr := s.conn.ReadResponse(0)
	if rerr == nil && (code == 500 || code == 502) {
		s.conn.EndResponse(id)
		data.Close()
		return nil, fmt.Errorf("%w: no MLSD", ErrUnsupported)
	}
	if rerr == nil && code == 550 {
		s.conn.EndResponse(id)
		data.Close()
		return nil, ErrNotFound
	}
	if rerr != nil || code != 150 {
		s.conn.EndResponse(id)
		data.Close()
		return nil, fmt.Errorf("%w: MLSD %d %s %v", ErrUnavailable, code, msg, rerr)
	}

	var out []Entry
	sc := bufio.NewScanner(data)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		e, ok := parseMLSDLine(sc.Text())
		if !ok {
			continue
		}
		out = append(out, e)
	}
	s.conn.EndResponse(id)
	_, _, _ = s.conn.ReadResponse(226) // transfer complete
	return sortEntries(out), nil
}

func parseMLSDLine(line string) (Entry, bool) {
	facts, name, ok := strings.Cut(line, "; ")
	if !ok {
		return Entry{}, false
	}
	e := Entry{Name: name}
	for _, fact := range strings.Split(facts, ";") {
		k, v, ok := strings.Cut(fact, "=")
		if !ok {
			continue
		}
		switch strings.ToLower(k) {
		case "type":
			if v == "dir" || v == "cdir" || v == "pdir" {
				if v == "dir" {
					e.IsDir = true
				} else {
					return Entry{}, false // the directory itself, not a child
				}
			}
		case "size":
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				e.Size = n
			}
		case "modify":
			if t, err := time.Parse("20060102150405", strings.TrimSuffix(v, "Z")); err == nil {
				e.ModTime = t.UTC()
			}
		}
	}
	return e, true
}

// list is the LIST fallback, parsing the two formats that cover effectively every FTP
// server still running: unix -rw-r--r-- ... and Windows/dos style.
func (s *ftpSession) list(ctx context.Context, dir string) ([]Entry, error) {
	data, err := s.passive(ctx)
	if err != nil {
		return nil, err
	}
	defer data.Close()
	id, err := s.conn.Cmd("LIST %s", "./"+dir)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	s.conn.StartResponse(id)
	code, msg, rerr := s.conn.ReadResponse(0)
	if rerr == nil && code == 550 {
		s.conn.EndResponse(id)
		data.Close()
		return nil, ErrNotFound
	}
	if rerr != nil || code != 150 {
		s.conn.EndResponse(id)
		data.Close()
		return nil, fmt.Errorf("%w: LIST %d %s %v", ErrUnavailable, code, msg, rerr)
	}
	var out []Entry
	sc := bufio.NewScanner(data)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		if e, ok := parseUnixListLine(sc.Text()); ok {
			out = append(out, e)
		} else if e, ok := parseDOSListLine(sc.Text()); ok {
			out = append(out, e)
		}
	}
	s.conn.EndResponse(id)
	_, _, _ = s.conn.ReadResponse(226)
	return sortEntries(out), nil
}

// parseUnixListLine handles "-rw-r--r-- 1 owner group 1234 Jan 02 15:04 name" and its
// directory twin. Names with spaces survive because everything before the 9th field is
// metadata and the rest is the name.
func parseUnixListLine(line string) (Entry, bool) {
	fields := strings.Fields(line)
	if len(fields) < 9 {
		return Entry{}, false
	}
	if len(fields[0]) == 0 || (fields[0][0] != '-' && fields[0][0] != 'd') {
		return Entry{}, false
	}
	size, err := strconv.ParseInt(fields[4], 10, 64)
	if err != nil {
		return Entry{}, false
	}
	name := strings.TrimSpace(strings.Join(fields[8:], " "))
	if name == "" || name == "." || name == ".." {
		return Entry{}, false
	}
	return Entry{Name: name, IsDir: fields[0][0] == 'd', Size: size}, true
}

// parseDOSListLine handles "01-02-26 03:04PM <DIR> name" / "01-02-26 03:04PM 1234 name".
func parseDOSListLine(line string) (Entry, bool) {
	fields := strings.Fields(line)
	if len(fields) < 4 || len(fields[0]) != 8 || fields[0][2] != '-' || fields[0][5] != '-' {
		return Entry{}, false
	}
	isDir := fields[2] == "<DIR>"
	size := int64(0)
	if !isDir {
		n, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			return Entry{}, false
		}
		size = n
	}
	name := strings.Join(fields[3:], " ")
	if name == "" || name == "." || name == ".." {
		return Entry{}, false
	}
	return Entry{Name: name, IsDir: isDir, Size: size}, true
}

func sortEntries(in []Entry) []Entry {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j].Name < in[j-1].Name; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
	return in
}

func (f *FTP) Stat(ctx context.Context, target string) (Entry, error) {
	if f.rel(target) == "" {
		return Entry{Name: "", IsDir: true}, nil
	}
	s, err := f.dial(ctx)
	if err != nil {
		return Entry{}, err
	}
	defer s.quit()
	id, err := s.conn.Cmd("SIZE %s", f.rel(target))
	if err == nil {
		s.conn.StartResponse(id)
		code, msg, rerr := s.conn.ReadResponse(0)
		s.conn.EndResponse(id)
		if rerr == nil && code == 550 {
			return Entry{}, ErrNotFound
		}
		if rerr == nil && code == 213 {
			if n, perr := strconv.ParseInt(strings.TrimSpace(msg), 10, 64); perr == nil {
				return Entry{Name: path.Base(target), Size: n}, nil
			}
		}
	}
	// Not a file, or SIZE unsupported: a directory of this name?
	entries, lerr := s.mlsd(ctx, f.rel(target))
	if lerr == nil {
		_ = entries
		return Entry{Name: path.Base(target), IsDir: true}, nil
	}
	if errors.Is(lerr, ErrNotFound) {
		return Entry{}, ErrNotFound
	}
	// LIST fallback for directory-ness.
	entries, lerr = s.list(ctx, f.rel(target))
	if lerr == nil {
		_ = entries
		return Entry{Name: path.Base(target), IsDir: true}, nil
	}
	if errors.Is(lerr, ErrNotFound) {
		return Entry{}, ErrNotFound
	}
	return Entry{}, lerr
}

func (f *FTP) Open(ctx context.Context, path string) (io.ReadCloser, error) {
	s, err := f.dial(ctx)
	if err != nil {
		return nil, err
	}
	data, err := s.passive(ctx)
	if err != nil {
		s.quit()
		return nil, err
	}
	id, err := s.conn.Cmd("RETR %s", f.rel(path))
	if err != nil {
		data.Close()
		s.quit()
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	s.conn.StartResponse(id)
	code, msg, rerr := s.conn.ReadResponse(0)
	if rerr != nil || code != 150 {
		s.conn.EndResponse(id)
		data.Close()
		s.quit()
		if code == 550 {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("%w: RETR %d %s %v", ErrUnavailable, code, msg, rerr)
	}
	s.conn.EndResponse(id)

	// The session lives until the reader closes: the 226 completion arrives on the
	// command connection only after the data stream is drained.
	return &ftpReadCloser{data: data, session: s}, nil
}

type ftpReadCloser struct {
	data    net.Conn
	session *ftpSession
	done    bool
}

func (r *ftpReadCloser) Read(p []byte) (int, error) { return r.data.Read(p) }

func (r *ftpReadCloser) Close() error {
	r.data.Close()
	if !r.done {
		// Drain the completion reply so the session's QUIT is not answered out of turn.
		// The caller's context deadline still bounds the session's socket.
		_, _, _ = r.session.conn.ReadResponse(226)
	}
	r.session.quit()
	return nil
}

func (f *FTP) Create(ctx context.Context, path string, r io.Reader) (int64, error) {
	s, err := f.dial(ctx)
	if err != nil {
		return 0, err
	}
	defer s.quit()
	data, err := s.passive(ctx)
	if err != nil {
		return 0, err
	}
	defer data.Close()
	id, err := s.conn.Cmd("STOR %s", f.rel(path))
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	s.conn.StartResponse(id)
	code, msg, rerr := s.conn.ReadResponse(0)
	if rerr != nil || code != 150 {
		s.conn.EndResponse(id)
		if code == 550 {
			return 0, ErrPermission
		}
		return 0, fmt.Errorf("%w: STOR %d %s %v", ErrUnavailable, code, msg, rerr)
	}
	s.conn.EndResponse(id)
	n, werr := io.Copy(data, r)
	data.Close()
	if werr != nil {
		return n, fmt.Errorf("%w: %v", ErrUnavailable, werr)
	}
	// The 226 arrives after the server drains the data connection; reading it is what
	// makes Create's return mean "the server has it", not "the kernel buffer has it".
	if _, _, err := s.conn.ReadResponse(226); err != nil {
		return n, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return n, nil
}

func (f *FTP) Mkdir(ctx context.Context, path string) error {
	s, err := f.dial(ctx)
	if err != nil {
		return err
	}
	defer s.quit()
	if err := s.cmd("MKD %s", f.rel(path)); err != nil {
		if errors.Is(err, ErrUnavailable) {
			return ErrExists // 5xx on MKD is a taken name on every common server
		}
		return err
	}
	return nil
}

func (f *FTP) Remove(ctx context.Context, path string, recursive bool) error {
	s, err := f.dial(ctx)
	if err != nil {
		return err
	}
	defer s.quit()
	// Try the file delete first: FTP has no stat to ask which it is, and DELE on a
	// directory answers 550, as RMD on a file does — the code IS the type test.
	if err := s.cmd("DELE %s", f.rel(path)); err == nil {
		return nil
	} else if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrPermission) {
		return err
	}
	if !recursive {
		// RMD on a non-empty directory is the protocol's refusal; surface it as Exists
		// so the caller's "not empty" handling engages.
		if err := s.cmd("RMD %s", f.rel(path)); err != nil {
			if errors.Is(err, ErrUnavailable) {
				return ErrExists
			}
			return err
		}
		return nil
	}
	entries, lerr := s.mlsd(ctx, f.rel(path))
	if lerr != nil {
		if errors.Is(lerr, ErrUnsupported) {
			entries, lerr = s.list(ctx, f.rel(path))
		}
		if lerr != nil {
			return lerr
		}
	}
	for _, e := range entries {
		if rerr := f.removeWithin(ctx, s, f.rel(path)+"/"+e.Name); rerr != nil {
			return rerr
		}
	}
	if err := s.cmd("RMD %s", f.rel(path)); err != nil {
		if errors.Is(err, ErrUnavailable) {
			return ErrExists // still non-empty: a concurrent writer added a child
		}
		return err
	}
	return nil
}

func (f *FTP) removeWithin(ctx context.Context, s *ftpSession, rel string) error {
	if err := s.cmd("DELE %s", rel); err == nil {
		return nil
	} else if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrPermission) {
		return err
	}
	entries, lerr := s.mlsd(ctx, rel)
	if lerr != nil {
		if errors.Is(lerr, ErrUnsupported) {
			entries, lerr = s.list(ctx, rel)
		}
		if lerr != nil {
			return lerr
		}
	}
	for _, e := range entries {
		if rerr := f.removeWithin(ctx, s, rel+"/"+e.Name); rerr != nil {
			return rerr
		}
	}
	return s.cmd("RMD %s", rel)
}

func (f *FTP) Rename(ctx context.Context, oldRel, newRel string) error {
	s, err := f.dial(ctx)
	if err != nil {
		return err
	}
	defer s.quit()
	if err := s.cmd("RNFR %s", f.rel(oldRel)); err != nil {
		return err
	}
	// RNTO's failure shapes: 553 is the standard "destination exists".
	id, err := s.conn.Cmd("RNTO %s", f.rel(newRel))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	s.conn.StartResponse(id)
	code, msg, rerr := s.conn.ReadResponse(0)
	s.conn.EndResponse(id)
	if rerr != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, rerr)
	}
	if code == 553 {
		return ErrExists
	}
	if code >= 500 {
		return fmt.Errorf("external: rename refused: %d %s", code, msg)
	}
	if code >= 400 {
		return fmt.Errorf("%w: %d %s", ErrUnavailable, code, msg)
	}
	return nil
}

func (f *FTP) Ping(ctx context.Context) error {
	s, err := f.dial(ctx)
	if err != nil {
		return err
	}
	// A NOOP proves the session is really usable, not just connected.
	if err := s.cmd("NOOP"); err != nil {
		s.quit()
		return err
	}
	s.quit()
	return nil
}

var _ Storage = (*FTP)(nil)
