package ticket

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCustomChildActivityAndDependencies(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(fmt.Sprintf("active=%t", active), func(t *testing.T) {
			c := child("child", "awaiting_data", "epic", 1)
			if active {
				c.RunningStatus = "awaiting_data"
			}
			index := Index([]*Ticket{epic("epic"), c, dep("waiting", StatusTodo, "child")})
			want := StatusOpen
			if active {
				want = StatusInProgress
			}
			assert.Equal(t, want, DeriveEpicStatus(index, "epic"))
			ready := Classify(index["waiting"], index)
			assert.False(t, ready.Ready)
			assert.Equal(t, []string{"child"}, ready.Blockers)
		})
	}
}
