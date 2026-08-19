package session_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/session"
)

// TestKindsMatchConfig pins the two names together. session cannot import
// config without making the config package a dependency of every path helper,
// so the constants are duplicated; this is what keeps the copies honest.
func TestKindsMatchConfig(t *testing.T) {
	assert.Equal(t, config.AgentKindClaude, session.KindClaude)
	assert.Equal(t, config.AgentKindPi, session.KindPi)
}
