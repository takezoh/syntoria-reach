// Package ids generates random identifiers. It is the one source of
// randomness for IDs; the service receives it as a dependency.
package ids

import (
	"crypto/rand"
	"encoding/hex"
)

// New returns prefix followed by 24 random hex digits.
func New(prefix string) string {
	b := make([]byte, 12)
	rand.Read(b)
	return prefix + hex.EncodeToString(b)
}
