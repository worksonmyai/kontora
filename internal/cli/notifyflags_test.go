package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseNotifyList(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  []string
	}{
		{name: "empty", value: "", want: []string{}},
		{name: "whitespace", value: " \t\n ", want: []string{}},
		{name: "empty items", value: " , ,\t,", want: []string{}},
		{name: "single", value: "done", want: []string{"done"}},
		{name: "list", value: "human_review,done", want: []string{"human_review", "done"}},
		{name: "trim and drop empties", value: ", human_review, ,\tdone\n,", want: []string{"human_review", "done"}},
		{name: "none is not special", value: " none ", want: []string{"none"}},
		{name: "mixed none", value: "none,done", want: []string{"none", "done"}},
		{name: "preserve order and duplicates", value: "done,waiting,done", want: []string{"done", "waiting", "done"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseNotifyList(tc.value)
			assert.NotNil(t, got)
			assert.Equal(t, tc.want, got)
		})
	}
}
