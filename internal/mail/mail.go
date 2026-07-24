// Package mail sends the few messages this product needs.
//
// There is exactly one message type — a share recipient's verification code — and the
// design reflects that. A general templating system would be more code than the thing
// it renders, and every added feature is another place a recipient address or a share
// label can end up somewhere it should not.
//
// SMTP is the transport because it is what a self-hosted deployment already has: a
// relay, or a local agent on loopback. Nothing here reaches a third-party API.
package mail

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// Config describes the outbound mail path.
type Config struct {
	// Enabled turns delivery on. When false, the server still accepts email shares but
	// records a warning every time a code is generated: a deployment that has been
	// configured to invite people by email must not look like it works if it cannot
	// actually send anything.
	Enabled bool
	// Host and Port of the SMTP relay.
	Host string
	Port int
	// Username and Password authenticate to the relay. Empty means no authentication,
	// which is normal for a loopback MTA.
	Username string
	Password string
	// From is the envelope and header sender.
	From string
	// ImplicitTLS dials TLS from the first byte (port 465). StartTLS upgrades a plain
	// connection instead (port 587) and is what most relays expect.
	ImplicitTLS bool
	// StartTLS requests an upgrade. Left false for a loopback relay that does not offer
	// it; mandatory TLS to a remote relay should be on.
	StartTLS bool
	// InsecureSkipVerify disables certificate verification. It exists for a relay with
	// a self-signed certificate on a private network, and is logged loudly at startup
	// because it removes the only protection against an active attacker on that path.
	InsecureSkipVerify bool
	// Timeout bounds one delivery attempt.
	Timeout time.Duration
}

// Sender delivers messages.
type Sender interface {
	SendRecipientCode(to, shareLabel, code, ttl string)
}

// SMTP delivers over SMTP.
type SMTP struct {
	cfg Config
	// log receives delivery failures. Mail is best-effort by nature: a failed send must
	// not crash a request or leak the failure to an anonymous caller.
	log Logger
}

// Logger is the slice of slog this package needs.
type Logger interface {
	Warn(msg string, args ...any)
	Info(msg string, args ...any)
}

// New builds a sender. A nil cfg.Enabled value yields a nil sender, which callers treat
// as "no mail transport configured".
func New(cfg Config, log Logger) (*SMTP, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if cfg.Host == "" {
		return nil, errors.New("mail: enabled but mail.host is empty")
	}
	if cfg.From == "" {
		return nil, errors.New("mail: enabled but mail.from is empty")
	}
	if cfg.Port == 0 {
		cfg.Port = 587
		if cfg.ImplicitTLS {
			cfg.Port = 465
		}
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 15 * time.Second
	}
	return &SMTP{cfg: cfg, log: log}, nil
}

// SendRecipientCode mails a verification code.
//
// It runs synchronously but never returns an error: the caller is an unauthenticated
// request, and a delivery failure must not become a signal the caller can observe.
// Failures go to the log, where an operator will see a broken relay.
func (s *SMTP) SendRecipientCode(to, shareLabel, code, ttl string) {
	if s == nil {
		return
	}
	if err := s.send(to, s.composeRecipientCode(shareLabel, code, ttl)); err != nil {
		// The address is logged because an operator debugging a relay needs to know
		// which delivery failed. The code is not logged: it is a credential.
		s.log.Warn("share verification code delivery failed",
			"to", to, "error", err.Error())
		return
	}
	s.log.Info("share verification code sent", "to", to)
}

// composeRecipientCode builds the message.
//
// Plain text, short, and with no link: a clickable link in email is exactly the pattern
// phishing trains people to follow, and this product should not teach that reflex.
func (s *SMTP) composeRecipientCode(shareLabel, code, ttl string) []byte {
	label := shareLabel
	if label == "" {
		label = "a shared item"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", s.cfg.From)
	b.WriteString("Subject: Your verification code\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	fmt.Fprintf(&b, "Someone shared %s with you.\r\n\r\n", label)
	fmt.Fprintf(&b, "Your verification code is:\r\n\r\n    %s\r\n\r\n", code)
	fmt.Fprintf(&b, "It expires in %s and can be used once.\r\n\r\n", ttl)
	b.WriteString("If you were not expecting this, you can ignore this message.\r\n")
	return []byte(b.String())
}

// send delivers one message.
func (s *SMTP) send(to string, body []byte) error {
	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	deadline := time.Now().Add(s.cfg.Timeout)

	dialer := &net.Dialer{Timeout: s.cfg.Timeout}
	var conn net.Conn
	var err error

	if s.cfg.ImplicitTLS {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, s.tlsConfig())
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("mail: dial %s: %w", addr, err)
	}
	// A stuck relay must not hold the connection open forever.
	_ = conn.SetDeadline(deadline)

	client, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("mail: greeting: %w", err)
	}
	defer client.Close()

	if s.cfg.StartTLS && !s.cfg.ImplicitTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(s.tlsConfig()); err != nil {
				return fmt.Errorf("mail: starttls: %w", err)
			}
		} else if s.cfg.Host != "localhost" && s.cfg.Host != "127.0.0.1" {
			// Refusing to continue in clear text to a remote relay is the difference
			// between "credentials may have leaked" and "they did not".
			return errors.New("mail: relay does not offer STARTTLS and is not local")
		}
	}

	if s.cfg.Username != "" {
		// PlainAuth refuses to send credentials over an unencrypted connection, which
		// is the behaviour we want; the error surfaces in the log.
		auth := smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("mail: auth: %w", err)
		}
	}

	if err := client.Mail(s.cfg.From); err != nil {
		return fmt.Errorf("mail: envelope from: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("mail: envelope to: %w", err)
	}

	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("mail: data: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		w.Close()
		return fmt.Errorf("mail: write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mail: close: %w", err)
	}
	return client.Quit()
}

func (s *SMTP) tlsConfig() *tls.Config {
	return &tls.Config{
		ServerName:         s.cfg.Host,
		InsecureSkipVerify: s.cfg.InsecureSkipVerify,
		MinVersion:         tls.VersionTLS12,
	}
}
