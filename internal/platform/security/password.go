// Package security provides password hashing and token primitives.
package security

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Hasher hashes passwords with argon2id and stores parameters in the PHC string so they can be raised later.
type Hasher struct {
	Time, MemoryKiB uint32
	Threads         uint8
}

// DefaultHasher follows the OWASP minimum recommendation (19 MiB, t=2, p=1), raised to 64 MiB.
func DefaultHasher() Hasher { return Hasher{Time: 2, MemoryKiB: 64 * 1024, Threads: 2} }

const keyLen = 32

func (h Hasher) Hash(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, h.Time, h.MemoryKiB, h.Threads, keyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, h.MemoryKiB, h.Time, h.Threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

var ErrBadHash = errors.New("malformed password hash")

// Verify reports whether password matches encoded, in constant time with respect to the key comparison.
func (h Hasher) Verify(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, ErrBadHash
	}
	var ver int
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[2], "v=%d", &ver); err != nil || ver != argon2.Version {
		return false, ErrBadHash
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false, ErrBadHash
	}
	if m > 1<<20 || t > 10 || p == 0 { // refuse absurd parameters from a corrupted row
		return false, ErrBadHash
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false, ErrBadHash
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// Dummy burns the same CPU as a real verification. Call it when the account does not exist so sign-in
// timing does not reveal which emails are registered.
func (h Hasher) Dummy(password string) {
	salt := make([]byte, 16)
	_ = argon2.IDKey([]byte(password), salt, h.Time, h.MemoryKiB, h.Threads, keyLen)
}
