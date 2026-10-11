// Package keyid derives stable, non-reversible identifiers from provider API keys.
package keyid

import (
	"crypto/sha256"
	"encoding/hex"
)

// Fingerprint returns a stable identifier and a short human-readable hint for key.
// The key itself is never stored. An empty key yields empty results.
func Fingerprint(key string) (id, hint string) {
	if key == "" {
		return "", ""
	}
	sum := sha256.Sum256([]byte(key))
	id = "k_" + hex.EncodeToString(sum[:])[:12]
	if len(key) > 4 {
		hint = "…" + key[len(key)-4:]
	}
	return id, hint
}
