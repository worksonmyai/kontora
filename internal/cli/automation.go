package cli

import (
	"crypto/rand"
	"encoding/hex"

	"github.com/worksonmyai/kontora/internal/automation"
	"github.com/worksonmyai/kontora/internal/cli/remote"
)

func NewAutomation(client *remote.Client, key string, spec automation.Spec) (automation.Definition, error) {
	spec = spec.Normalize()
	if err := spec.Validate(); err != nil {
		return automation.Definition{}, err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return automation.Definition{}, err
	}
	return client.CreateAutomation(hex.EncodeToString(nonce[:]), key, spec)
}
