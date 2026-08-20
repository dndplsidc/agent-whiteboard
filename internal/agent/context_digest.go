package agent

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash"
)

const (
	ResourceKindMarkdown = "markdown"
	ResourceKindHTML     = "html"

	markdownContextDomain = "agent-whiteboard-context-v1"
	htmlContextDomain     = "agent-whiteboard-html-context-v1"
)

// CalculateContextDigest returns the lowercase hexadecimal canonical digest of
// the exact Markdown and creator-context bytes. Its output is a compatibility
// contract retained from the original Markdown-only Page Agent.
func CalculateContextDigest(markdown, creatorContext []byte) string {
	return calculateContextDigest(markdownContextDomain, markdown, creatorContext)
}

// CalculateContextDigestForKind returns the canonical digest of the exact page
// source and creator-context bytes in the resource kind's distinct domain.
func CalculateContextDigestForKind(kind string, source, creatorContext []byte) (string, error) {
	var domain string
	switch kind {
	case ResourceKindMarkdown:
		domain = markdownContextDomain
	case ResourceKindHTML:
		domain = htmlContextDomain
	default:
		return "", errors.New("invalid page resource kind")
	}
	return calculateContextDigest(domain, source, creatorContext), nil
}

func calculateContextDigest(domain string, source, creatorContext []byte) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte(domain))
	writeLengthPrefixed(digest, source)
	writeLengthPrefixed(digest, creatorContext)
	return hex.EncodeToString(digest.Sum(nil))
}

func writeLengthPrefixed(digest hash.Hash, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = digest.Write(length[:])
	_, _ = digest.Write(value)
}
