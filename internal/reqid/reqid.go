// Package reqid generates time-ordered request IDs in ULID format.
package reqid

import (
	"crypto/rand"
	"encoding/binary"
	"time"
)

const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// New returns a 26-character ULID: 48 bits of millisecond timestamp followed by 80 random bits.
func New() string {
	return newAt(time.Now())
}

func newAt(t time.Time) string {
	var b [16]byte
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(t.UnixMilli()))
	copy(b[:6], ts[2:])
	_, _ = rand.Read(b[6:])
	return encode(b)
}

// encode writes 128 bits as 26 base32 characters, treating the input as a
// 130-bit number with two leading zero bits.
func encode(b [16]byte) string {
	var out [26]byte
	for i := range out {
		bit := i*5 - 2
		var v byte
		for j := 0; j < 5; j++ {
			v <<= 1
			pos := bit + j
			if pos >= 0 && b[pos/8]&(0x80>>(pos%8)) != 0 {
				v |= 1
			}
		}
		out[i] = alphabet[v]
	}
	return string(out[:])
}
