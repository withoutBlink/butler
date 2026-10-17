package ids

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// UUID returns a random RFC 4122 version 4 UUID in canonical form
// (8-4-4-4-12 hex). Agent identities use these: an id carries no derived
// meaning (the display name is the human-readable label and can change),
// it is stable across re-registration and cannot collide with a sibling.
func UUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("ids: crypto/rand failed: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func New() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("ids: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}
