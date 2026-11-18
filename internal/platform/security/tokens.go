package security

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"math/big"
)

// NewToken returns a 256-bit random URL-safe token with a short type prefix (for example "pat_").
func NewToken(prefix string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken returns HMAC-SHA256(pepper, parts...). Only hashes are stored, so a database leak does not expose live tokens.
func HashToken(pepper []byte, parts ...string) []byte {
	m := hmac.New(sha256.New, pepper)
	for _, p := range parts {
		m.Write([]byte(p))
		m.Write([]byte{0})
	}
	return m.Sum(nil)
}

// NewCode returns a uniformly random 6-digit numeric code.
func NewCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	s := n.String()
	for len(s) < 6 {
		s = "0" + s
	}
	return s, nil
}
