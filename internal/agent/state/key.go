package state

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
)

const conversationKeyDomain = "agent-whiteboard-conversation-key-v1"

func ConversationKey(identity Identity) (string, error) {
	if err := identity.Validate(); err != nil {
		return "", err
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte(conversationKeyDomain))
	for _, value := range []string{identity.Origin, string(identity.Kind), identity.CapabilityID, string(identity.Provider)} {
		writeKeyValue(digest, value)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func writeKeyValue(digest hash.Hash, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = digest.Write(length[:])
	_, _ = digest.Write([]byte(value))
}
