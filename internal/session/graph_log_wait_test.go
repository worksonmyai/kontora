package session

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGraphLogPathInvocationQualification(t *testing.T) {
	require.Equal(t, filepath.Join("logs", "ticket", "node.visit.log"), GraphLogPath("logs", "ticket", "node", "visit"))
	require.Equal(t, GraphLogPath("logs", "ticket", "node", "visit"), GraphLogPath("logs", "ticket", "node", "visit", ""))
	require.Equal(t, filepath.Join("logs", "ticket", "node.visit.invocation.log"), GraphLogPath("logs", "ticket", "node", "visit", "invocation"))
	require.NotEqual(t, GraphLogPath("logs", "ticket", "node", "visit", "first"), GraphLogPath("logs", "ticket", "node", "visit", "continuation"))
}
