// Package ticket implements the credential format used for every bearer-style
// token in the system: browser sessions, app passwords, share access cookies,
// one-shot download URLs and email verification links.
//
// One master secret, many derived keys. Each purpose gets its own key via
// HKDF-style domain separation, so a token minted for one purpose can never be
// replayed as another — a download URL cannot be presented as a session, and a
// session cannot be replayed as a share cookie.
//
// Rationale for not using a slow KDF here: these payloads are 128-bit random
// values, not human-chosen passwords. There is no dictionary to attack, so
// Argon2 buys nothing while costing tens of milliseconds on every WebDAV
// request. Passwords that humans type (break-glass, share passwords) do use
// Argon2id — see internal/auth/password.go.
package ticket

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Purpose names a token's intended use. Adding one is safe; renaming one
// invalidates every outstanding token of that kind.
type Purpose string

const (
	PurposeSession     Purpose = "session"
	PurposeAppPassword Purpose = "apppassword"
	PurposeShareAccess Purpose = "share_access"
	PurposeDownload    Purpose = "download"
	PurposeEmailVerify Purpose = "email_verify"
	PurposeSCIMToken   Purpose = "scim"
)

var errInvalid = errors.New("ticket: invalid or tampered token")

// ErrExpired is returned for a structurally valid token whose expiry passed.
var ErrExpired = errors.New("ticket: expired")

// Issuer mints and verifies tokens.
type Issuer struct {
	master []byte
	now    func() time.Time
}

// NewIssuer requires a secret of at least 32 bytes; anything shorter is a
// configuration bug rather than a runtime one.
func NewIssuer(master []byte) (*Issuer, error) {
	if len(master) < 32 {
		return nil, fmt.Errorf("ticket: master secret must be >= 32 bytes, got %d", len(master))
	}
	cp := make([]byte, len(master))
	copy(cp, master)
	return &Issuer{master: cp, now: time.Now}, nil
}

// derive produces a purpose-bound key. The label includes a version so that a
// future format change can coexist with outstanding tokens.
func (i *Issuer) derive(p Purpose) []byte {
	m := hmac.New(sha256.New, i.master)
	m.Write([]byte("newcloud/v1/"))
	m.Write([]byte(p))
	return m.Sum(nil)
}

// Claims is the payload bound into a token.
//
// Subject and ID are opaque strings so the same format serves both user
// sessions (subject = user UUID) and anonymous shares (subject = share UUID).
type Claims struct {
	Subject string
	ID      string // jti: lets a session be revoked individually
	Issued  time.Time
	Expiry  time.Time
	Extra   map[string]string
}

// Mint returns an opaque token of the form <prefix>.<payload>.<mac>.
func (i *Issuer) Mint(p Purpose, c Claims) (string, error) {
	if c.Subject == "" {
		return "", errors.New("ticket: empty subject")
	}
	if c.Expiry.IsZero() {
		return "", errors.New("ticket: empty expiry")
	}
	if c.Issued.IsZero() {
		c.Issued = i.now()
	}

	body := encodeClaims(c)
	mac := i.sign(p, body)

	return string(p) + "." + b64(body) + "." + b64(mac), nil
}

// Verify checks the MAC and expiry and returns the claims.
func (i *Issuer) Verify(p Purpose, token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, errInvalid
	}
	if Purpose(parts[0]) != p {
		// Cross-purpose replay attempt: a download token presented as a session.
		return Claims{}, errInvalid
	}
	body, err := unb64(parts[1])
	if err != nil {
		return Claims{}, errInvalid
	}
	gotMAC, err := unb64(parts[2])
	if err != nil {
		return Claims{}, errInvalid
	}
	wantMAC := i.sign(p, body)
	if subtle.ConstantTimeCompare(gotMAC, wantMAC) != 1 {
		return Claims{}, errInvalid
	}
	c, err := decodeClaims(body)
	if err != nil {
		return Claims{}, errInvalid
	}
	if !c.Expiry.IsZero() && i.now().After(c.Expiry) {
		return c, ErrExpired
	}
	return c, nil
}

func (i *Issuer) sign(p Purpose, body []byte) []byte {
	m := hmac.New(sha256.New, i.derive(p))
	m.Write(body)
	return m.Sum(nil)
}

// encodeClaims uses a length-prefixed binary form.
//
// An earlier draft used a 0x1f separator. A subject containing that byte
// shifted every subsequent field, which either rejected a legitimate user or
// (worse, in other schemes) let a crafted subject inject an Extra claim. Length
// prefixes remove the ambiguity by construction: no field value can influence
// how the next field is parsed.
func encodeClaims(c Claims) []byte {
	var buf []byte
	put := func(s string) {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(s)))
		buf = append(buf, n[:]...)
		buf = append(buf, s...)
	}
	put(c.Subject)
	put(c.ID)
	put(c.Issued.UTC().Format(time.RFC3339))
	put(c.Expiry.UTC().Format(time.RFC3339))
	put(strconv.Itoa(len(c.Extra)))
	for _, k := range sortedKeys(c.Extra) {
		put(k)
		put(c.Extra[k])
	}
	return buf
}

func decodeClaims(b []byte) (Claims, error) {
	r := &reader{b: b}
	subject, err := r.str()
	if err != nil {
		return Claims{}, err
	}
	id, err := r.str()
	if err != nil {
		return Claims{}, err
	}
	issuedStr, err := r.str()
	if err != nil {
		return Claims{}, err
	}
	expiryStr, err := r.str()
	if err != nil {
		return Claims{}, err
	}
	nExtraStr, err := r.str()
	if err != nil {
		return Claims{}, err
	}
	nExtra, err := strconv.Atoi(nExtraStr)
	if err != nil || nExtra < 0 || nExtra > 64 {
		return Claims{}, errInvalid
	}

	c := Claims{Subject: subject, ID: id}
	if c.Issued, err = time.Parse(time.RFC3339, issuedStr); err != nil {
		return Claims{}, err
	}
	if c.Expiry, err = time.Parse(time.RFC3339, expiryStr); err != nil {
		return Claims{}, err
	}
	for i := 0; i < nExtra; i++ {
		k, err := r.str()
		if err != nil {
			return Claims{}, err
		}
		v, err := r.str()
		if err != nil {
			return Claims{}, err
		}
		if c.Extra == nil {
			c.Extra = map[string]string{}
		}
		c.Extra[k] = v
	}
	if r.pos != len(r.b) {
		return Claims{}, errInvalid
	}
	return c, nil
}

type reader struct {
	b   []byte
	pos int
}

func (r *reader) str() (string, error) {
	if len(r.b)-r.pos < 4 {
		return "", errInvalid
	}
	n := int(binary.BigEndian.Uint32(r.b[r.pos:]))
	r.pos += 4
	if n < 0 || len(r.b)-r.pos < n {
		return "", errInvalid
	}
	s := string(r.b[r.pos : r.pos+n])
	r.pos += n
	return s, nil
}

func sortedKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func unb64(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}
