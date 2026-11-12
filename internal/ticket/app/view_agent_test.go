package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func TestResolveViewAgentScalarParity(t *testing.T) {
	cfg := &config.Config{
		DefaultAgent:    "fallback",
		ReworkIsBuiltin: true,
		Pipelines: map[string]config.Pipeline{
			"work":         {{Stage: "code", Agent: "coder"}, {Stage: "review", Agent: "reviewer"}},
			"empty":        {},
			"with-rework":  {{Stage: config.ReworkStageName, Agent: "reworker"}},
			"blank-rework": {{Stage: config.ReworkStageName}},
		},
	}
	cases := []struct {
		name   string
		ticket ticket.Ticket
		want   string
	}{
		{"override", ticket.Ticket{Agent: "explicit", Pipeline: "work", Stage: "code"}, "explicit"},
		{"pipeline", ticket.Ticket{Pipeline: "work", Stage: "review"}, "reviewer"},
		{"rework override", ticket.Ticket{Agent: "explicit", Pipeline: "work", Stage: config.ReworkStageName, History: []ticket.HistoryEntry{{Agent: "previous"}}}, "explicit"},
		{"rework latest nonempty history agent", ticket.Ticket{Pipeline: "work", Stage: config.ReworkStageName, History: []ticket.HistoryEntry{{Agent: "older"}, {Agent: "latest"}, {}}}, "latest"},
		{"rework no history", ticket.Ticket{Pipeline: "work", Stage: config.ReworkStageName}, "fallback"},
		{"rework empty history agents", ticket.Ticket{Pipeline: "work", Stage: config.ReworkStageName, History: []ticket.HistoryEntry{{}, {}}}, "fallback"},
		{"rework empty pipeline", ticket.Ticket{Pipeline: "empty", Stage: config.ReworkStageName}, "fallback"},
		{"configured rework before history", ticket.Ticket{Pipeline: "with-rework", Stage: config.ReworkStageName, History: []ticket.HistoryEntry{{Agent: "previous"}}}, "reworker"},
		{"configured rework override", ticket.Ticket{Agent: "explicit", Pipeline: "with-rework", Stage: config.ReworkStageName}, "explicit"},
		{"configured rework empty agent", ticket.Ticket{Pipeline: "blank-rework", Stage: config.ReworkStageName, History: []ticket.HistoryEntry{{Agent: "previous"}}}, ""},
		{"rework unknown pipeline", ticket.Ticket{Kontora: true, Pipeline: "gone", Stage: config.ReworkStageName, History: []ticket.HistoryEntry{{Agent: "previous"}}}, ""},
		{"rework standalone", ticket.Ticket{Kontora: true, Stage: config.ReworkStageName, History: []ticket.HistoryEntry{{Agent: "previous"}}}, "fallback"},
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
	t.Run("non-built-in rework has no history fallback", func(t *testing.T) {
		customCfg := *cfg
		customCfg.ReworkIsBuiltin = false
		tkt := &ticket.Ticket{Pipeline: "work", Stage: config.ReworkStageName, History: []ticket.HistoryEntry{{Agent: "previous"}}}
		assert.Empty(t, ResolveViewAgent(&customCfg, tkt))
		for _, detail := range []bool{false, true} {
			assert.Empty(t, BuildView(&customCfg, tkt, detail).Agent)
		}
	})
	cfg.Pipelines[""] = config.Pipeline{{Stage: "code", Agent: "empty-name"}}
	tkt := &ticket.Ticket{Kontora: true, Stage: "code"}
	assert.Equal(t, "empty-name", ResolveViewAgent(cfg, tkt))
	assert.Equal(t, BuildView(cfg, tkt, false).Agent, ResolveViewAgent(cfg, tkt))
}
