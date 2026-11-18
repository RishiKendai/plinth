// Package ids validates and generates resource identifiers. Clients may generate UUIDs before a record reaches
// the server (offline creation), so the server validates the format instead of trusting it.
package ids

import (
	"crypto/rand"
	"fmt"
	"regexp"
	"strings"
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Valid reports whether s is a lower-case canonical UUID.
func Valid(s string) bool { return uuidRe.MatchString(s) }

// Normalize lower-cases a UUID so equal IDs compare equal; the result may still be invalid.
func Normalize(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// New returns a random version-4 UUID.
func New() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
