package memory

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

func newLeaseOwner() (string, error) {
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", fmt.Errorf("create lease owner: %w", err)
	}
	return hex.EncodeToString(token[:]), nil
}
