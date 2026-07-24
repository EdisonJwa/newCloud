package main

// A minimal RFC 6455 client, sufficient for the Chrome DevTools protocol.
//
// It exists because the only thing that needs a WebSocket here is a development
// check. Pulling in a WebSocket library would add a dependency to the project's
// module graph — and therefore to what `go vet`, `govulncheck` and a reviewer must
// consider — for code that never ships in the server binary.
//
// Scope: text frames, client masking, no extensions, no continuation frames (CDP
// messages arrive whole and are far below any size where fragmentation starts).

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type wsConn struct {
	conn net.Conn
	br   *bufio.Reader

	writeMu  sync.Mutex
	nextID   int64
	mu       sync.Mutex
	pending  map[int64]chan json.RawMessage
	handlers map[string][]func(json.RawMessage)

	closed chan struct{}
	once   sync.Once
}

// dialWS opens a WebSocket to a ws:// URL and starts the read loop.
func dialWS(rawURL string) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":80"
	}

	conn, err := net.DialTimeout("tcp", host, 10*time.Second)
	if err != nil {
		return nil, err
	}

	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		conn.Close()
		return nil, err
	}
	accept := base64.StdEncoding.EncodeToString(key)

	req := fmt.Sprintf("GET %s HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"Upgrade: websocket\r\n"+
		"Connection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\n"+
		"Sec-WebSocket-Version: 13\r\n"+
		"\r\n", u.RequestURI(), u.Host, accept)
	if _, err := io.WriteString(conn, req); err != nil {
		conn.Close()
		return nil, err
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		conn.Close()
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return nil, fmt.Errorf("websocket handshake: status %d", resp.StatusCode)
	}

	w := &wsConn{
		conn:     conn,
		br:       br,
		pending:  map[int64]chan json.RawMessage{},
		handlers: map[string][]func(json.RawMessage){},
		closed:   make(chan struct{}),
	}
	go w.readLoop()
	return w, nil
}

// Close shuts the socket down.
func (w *wsConn) Close() error {
	var err error
	w.once.Do(func() {
		close(w.closed)
		err = w.conn.Close()
	})
	return err
}

// On registers a handler for server-initiated events.
func (w *wsConn) On(method string, fn func(json.RawMessage)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.handlers[method] = append(w.handlers[method], fn)
}

// Call sends a command and waits for its result, discarding it.
func (w *wsConn) Call(method string, params any) error {
	_, err := w.CallWithResult(method, params)
	return err
}

// CallWithResult sends a command and returns the raw result.
func (w *wsConn) CallWithResult(method string, params any) (json.RawMessage, error) {
	w.mu.Lock()
	w.nextID++
	id := w.nextID
	ch := make(chan json.RawMessage, 1)
	w.pending[id] = ch
	w.mu.Unlock()

	defer func() {
		w.mu.Lock()
		delete(w.pending, id)
		w.mu.Unlock()
	}()

	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	if err := w.writeText(payload); err != nil {
		return nil, err
	}

	select {
	case raw := <-ch:
		// A protocol error arrives as a result with an "error" member.
		var probe struct {
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &probe); err == nil && probe.Error != nil {
			return nil, fmt.Errorf("%s: %s", method, probe.Error.Message)
		}
		return raw, nil
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("%s: timed out", method)
	case <-w.closed:
		return nil, fmt.Errorf("%s: connection closed", method)
	}
}

func (w *wsConn) writeText(payload []byte) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()

	// Client-to-server frames must be masked (RFC 6455 §5.3).
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}

	header := make([]byte, 0, 14)
	header = append(header, 0x81) // FIN + text

	n := len(payload)
	switch {
	case n < 126:
		header = append(header, byte(n)|0x80)
	case n <= 0xFFFF:
		header = append(header, 126|0x80)
		header = binary.BigEndian.AppendUint16(header, uint16(n))
	default:
		header = append(header, 127|0x80)
		header = binary.BigEndian.AppendUint64(header, uint64(n))
	}
	header = append(header, mask...)

	masked := make([]byte, n)
	for i := 0; i < n; i++ {
		masked[i] = payload[i] ^ mask[i%4]
	}

	if _, err := w.conn.Write(append(header, masked...)); err != nil {
		return err
	}
	return nil
}

func (w *wsConn) readLoop() {
	defer w.Close()

	for {
		opcode, payload, err := w.readFrame()
		if err != nil {
			return
		}

		switch opcode {
		case 0x1: // text
			w.dispatch(payload)
		case 0x8: // close
			return
		case 0x9: // ping
			w.writeMu.Lock()
			_ = w.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			_, _ = w.conn.Write([]byte{0x8A, 0x00})
			_ = w.conn.SetWriteDeadline(time.Time{})
			w.writeMu.Unlock()
		}
	}
}

func (w *wsConn) dispatch(payload []byte) {
	// Route by id (a command result) or by method (an event).
	var envelope struct {
		ID     *int64          `json:"id"`
		Method string          `json:"method"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return
	}

	if envelope.ID != nil {
		w.mu.Lock()
		ch, ok := w.pending[*envelope.ID]
		w.mu.Unlock()
		if ok {
			// Deliver the whole message so callers can see protocol errors.
			select {
			case ch <- payload:
			default:
			}
		}
		return
	}

	if envelope.Method != "" {
		w.mu.Lock()
		fns := w.handlers[envelope.Method]
		w.mu.Unlock()
		for _, fn := range fns {
			fn(payload)
		}
	}
}

func (w *wsConn) readFrame() (byte, []byte, error) {
	var head [2]byte
	if _, err := io.ReadFull(w.br, head[:]); err != nil {
		return 0, nil, err
	}

	opcode := head[0] & 0x0F
	masked := head[1]&0x80 != 0
	length := int64(head[1] & 0x7F)

	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(w.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(w.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint64(ext[:]))
	}

	var maskKey [4]byte
	if masked {
		if _, err := io.ReadFull(w.br, maskKey[:]); err != nil {
			return 0, nil, err
		}
	}

	if length < 0 || length > 64<<20 {
		return 0, nil, fmt.Errorf("websocket: frame too large (%d)", length)
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(w.br, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i%4]
		}
	}
	return opcode, payload, nil
}

// decodeBase64 decodes standard base64, tolerating padding variants.
func decodeBase64(s string) []byte {
	out, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		out, _ = base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "="))
	}
	return out
}
