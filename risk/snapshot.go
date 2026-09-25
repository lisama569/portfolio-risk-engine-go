package risk

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// hashInputs produces a stable hex digest identifying the exact input
// snapshot used by a calculation. Equal inputs yield equal hashes, which
// makes reproducibility and "incremental == full recompute" claims
// auditable in test logs.
func hashInputs(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		// separator prevents ("ab","c") and ("a","bc") colliding
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// joinStrings is a tiny helper kept out of hot paths' allocation noise.
func joinStrings(parts ...string) string { return strings.Join(parts, "|") }
