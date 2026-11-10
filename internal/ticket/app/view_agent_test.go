package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func TestResolveViewAgentScalarParity(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent: "fallback",
		Pipelines: map[string]config.Pipeline{
			"work":  {{Stage: "code", Agent: "coder"}, {Stage: "review", Agent: "reviewer"}},
			"empty": {},
		},
	}
	cases := []struct {
		name   string
		ticket ticket.Ticket
		want   string
	}{
		{"override", ticket.Ticket{Agent: "explicit", Pipeline: "work", Stage: "code"}, "explicit"},
		{"pipeline", ticket.Ticket{Pipeline: "work", Stage: "review"}, "reviewer"},
		{"missing stage", ticket.Ticket{Kontora: true, Pipeline: "work", Stage: "gone"}, ""},
		{"empty stage", ticket.Ticket{Kontora: true, Pipeline: "work"}, ""},
		{"missing pipeline", ticket.Ticket{Kontora: true, Pipeline: "gone"}, ""},
		{"empty pipeline", ticket.Ticket{Kontora: true, Pipeline: "empty"}, ""},
		{"default", ticket.Ticket{Kontora: true}, "fallback"},
		{"unmanaged", ticket.Ticket{}, ""},
		{"passive epic", ticket.Ticket{Kontora: true, Kind: ticket.KindEpic}, ""},
		{"passive override", ticket.Ticket{Kontora: true, Kind: ticket.KindEpic, Agent: "explicit"}, "explicit"},
		{"passive pipeline", ticket.Ticket{Kontora: true, Kind: ticket.KindEpic, Pipeline: "work", Stage: "code"}, "coder"},
		{"agent epic", ticket.Ticket{Kontora: true, Kind: ticket.KindEpic, EpicExecution: ticket.EpicExecutionAgent}, "fallback"},
		{"agent epic pipeline", ticket.Ticket{Kontora: true, Kind: ticket.KindEpic, EpicExecution: ticket.EpicExecutionAgent, Pipeline: "work", Stage: "code"}, "coder"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ResolveViewAgent(cfg, &tc.ticket))
			for _, detail := range []bool{false, true} {
				v := BuildView(cfg, &tc.ticket, detail)
				assert.Equal(t, tc.want, v.Agent)
				assert.Equal(t, tc.ticket.Agent != "", v.AgentOverride)
			}
			assert.Zero(t, testing.AllocsPerRun(100, func() { ResolveViewAgent(cfg, &tc.ticket) }))
		})
	}
	cfg.Pipelines[""] = config.Pipeline{{Stage: "code", Agent: "empty-name"}}
	tkt := &ticket.Ticket{Kontora: true, Stage: "code"}
	assert.Equal(t, "empty-name", ResolveViewAgent(cfg, tkt))
	assert.Equal(t, BuildView(cfg, tkt, false).Agent, ResolveViewAgent(cfg, tkt))
}
