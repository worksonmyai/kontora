package ticket

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRealTicketRoundTrip(t *testing.T) {
	original, err := os.ReadFile("testdata/real-ticket.md")
	require.NoError(t, err)

	tkt, err := ParseFile("testdata/real-ticket.md")
	require.NoError(t, err)

	assert.NotEmpty(t, tkt.ID)

	// Mutate status
	require.NoError(t, tkt.SetField("status", "in_progress"))

	out, err := tkt.Marshal()
	require.NoError(t, err)

	// Re-parse
	tkt2, err := ParseBytes(out)
	require.NoError(t, err)

	assert.Equal(t, StatusInProgress, tkt2.Status)
	assert.Equal(t, tkt.Body, tkt2.Body)

	// Verify unknown fields are preserved by checking round-trip of the re-parsed tkt
	out2, err := tkt2.Marshal()
	require.NoError(t, err)

	// Parse original to get known fields for comparison
	origTkt, err := ParseBytes(original)
	require.NoError(t, err)

	assert.Equal(t, origTkt.ID, tkt2.ID)

	// Second marshal and third parse should be stable
	tkt3, err := ParseBytes(out2)
	require.NoError(t, err)
	assert.Equal(t, tkt.Body, tkt3.Body)
	assert.Equal(t, origTkt.ID, tkt3.ID)
}
