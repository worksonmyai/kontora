package web

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/worksonmyai/kontora/internal/ticket/app"
)

func TestTicketInfoFromView_ClaimedBy(t *testing.T) {
	info := TicketInfoFromView(app.View{ID: "t1", ClaimedBy: "alpha"})
	assert.Equal(t, "alpha", info.ClaimedBy)

	// An empty claim stays empty (and is omitted from JSON via omitempty).
	assert.Empty(t, TicketInfoFromView(app.View{ID: "t2"}).ClaimedBy)
}
