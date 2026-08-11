package agent

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
)

const domain = "agent-whiteboard-context-v1"

// CalculateContextDigest returns the lowercase hexadecimal canonical digest of the exact
// Markdown and creator-context bytes.
func CalculateContextDigest(markdown, creatorContext []byte) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte(domain))
	writeLengthPrefixed(digest, markdown)
	writeLengthPrefixed(digest, creatorContext)
	return hex.EncodeToString(digest.Sum(nil))
}

func writeLengthPrefixed(digest hash.Hash, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = digest.Write(length[:])
	_, _ = digest.Write(value)
}
