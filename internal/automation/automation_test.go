package automation

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func testSpec() Spec {
	return Spec{Name: "Nightly report", Prompt: "Report {{ .Automation.Name }}", Path: "~/missing-repo", Agent: "pi", Schedule: "0 3 * * *", Enabled: true}
}

func TestSpecDefaultsAndValidation(t *testing.T) {
	s := testSpec().Normalize()
	require.Equal(t, "UTC", s.Timezone)
	require.Equal(t, "worktree", s.Workspace)
	require.Equal(t, "skip", s.Overlap)
	require.Equal(t, "30m", s.Timeout)
	require.Equal(t, s, s.Normalize())
	require.NoError(t, s.Validate())
	cases := []struct {
		name   string
		change func(*Spec)
	}{
		{"name", func(s *Spec) { s.Name = " " }},
		{"prompt", func(s *Spec) { s.Prompt = "" }},
		{"path", func(s *Spec) { s.Path = "\x00bad" }},
		{"agent", func(s *Spec) { s.Agent = "" }},
		{"model", func(s *Spec) { s.Model = "bad\nmodel" }},
		{"workspace", func(s *Spec) { s.Workspace = "ticket" }},
		{"overlap", func(s *Spec) { s.Overlap = "parallel" }},
		{"timeout", func(s *Spec) { s.Timeout = "-1s" }},
		{"zero timeout", func(s *Spec) { s.Timeout = "0s" }},
		{"schedule", func(s *Spec) { s.Schedule = "@hourly" }},
		{"timezone", func(s *Spec) { s.Timezone = "Local" }},
		{"long prompt", func(s *Spec) { s.Prompt = strings.Repeat("a", 65537) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { s := testSpec(); tc.change(&s); require.ErrorIs(t, s.Validate(), ErrValidation) })
	}
	for _, key := range []string{"nightly", "nightly-report", "job2", "a"} {
		require.NoError(t, ValidateKey(key))
	}
	for _, key := range []string{"", "../job", "job_1", "-job", "Job", "job-", "a--b"} {
		require.True(t, errors.Is(ValidateKey(key), ErrValidation))
	}
}

func TestDefinitionJSONEmbedsSpec(t *testing.T) {
	data, err := json.Marshal(Definition{Spec: testSpec(), ID: "aut-test", Key: "nightly"})
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &fields))
	require.Contains(t, fields, "name")
	require.Contains(t, fields, "prompt")
	require.NotContains(t, fields, "Spec")
	require.NotContains(t, fields, "instructions")
	require.NotContains(t, fields, "deleted")
	require.JSONEq(t, `null`, string(fields["success_rate"]))
}
