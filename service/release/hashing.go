package release

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strings"
)

// StableBucket maps a subject to a deterministic bucket in [0, 100).
func StableBucket(parts ...string) int {
	digest := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return int(binary.BigEndian.Uint64(digest[:8]) % 100)
}

// CanarySelected is the reference stable/canary split: a conversation lands
// on the canary when its stable bucket falls under percentage. Conversations
// without an id never take the canary.
func CanarySelected(tenantID int64, key, conversationID string, percentage int) bool {
	if percentage <= 0 || conversationID == "" {
		return false
	}
	if percentage >= 100 {
		return true
	}
	return StableBucket(fmt.Sprintf("%d", tenantID), key, conversationID) < percentage
}
