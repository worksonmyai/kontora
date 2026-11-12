package daemon

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
)

func TestResultAgentPiModes(t *testing.T) {
	cases := []struct {
		name       string
		agent      config.Agent
		unrelated  bool
		annotation bool
		wantRefuse bool
	}{
		{name: "default TUI", agent: config.Agent{Binary: "pi"}},
		{name: "text with tmux TTY", agent: config.Agent{Binary: "pi", Args: []string{"--mode", "text"}}},
		{name: "print", agent: config.Agent{Binary: "pi", Args: []string{"--print"}}, wantRefuse: true},
		{name: "short print", agent: config.Agent{Binary: "pi", Args: []string{"-p"}}, wantRefuse: true},
		{name: "JSON", agent: config.Agent{Binary: "pi", Args: []string{"--mode", "json"}}, wantRefuse: true},
		{name: "RPC", agent: config.Agent{Binary: "pi", Args: []string{"--mode", "rpc"}}, wantRefuse: true},
		{name: "equals JSON", agent: config.Agent{Binary: "pi", Args: []string{"--mode=json"}}, wantRefuse: true},
		{name: "equals RPC", agent: config.Agent{Binary: "pi", Args: []string{"--mode=rpc"}}, wantRefuse: true},
		{name: "wrapper print", agent: config.Agent{Binary: "nono", Args: []string{"run", "--", "pi", "-p"}}, wantRefuse: true},
		{name: "wrapper options", agent: config.Agent{Binary: "nono", Args: []string{"run", "-p", "json", "--", "pi"}}},
		{name: "unrelated TUI", agent: config.Agent{Binary: "pi"}, unrelated: true},
		{name: "unrelated JSON", agent: config.Agent{Binary: "pi", Args: []string{"--mode", "json"}}, unrelated: true, wantRefuse: true},
		{name: "unrelated print", agent: config.Agent{Binary: "pi", Args: []string{"--print"}}, unrelated: true, wantRefuse: true},
		{name: "annotation JSON", agent: config.Agent{Binary: "pi", Args: []string{"--mode", "json"}}, annotation: true},
		{name: "unrelated annotation print", agent: config.Agent{Binary: "pi", Args: []string{"-p"}}, unrelated: true, annotation: true},
		{name: "other agent print", agent: config.Agent{Binary: "claude", Args: []string{"--print"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newResultFixture(t)
			before, err := os.ReadFile(executionPath(f.h.cfg, f.tkt.ID))
			require.NoError(t, err)
			p := spawnAgentParams{cfg: f.h.cfg, ticketID: f.tkt.ID, stageName: f.tkt.Stage,
				filePath: f.path, agentCfg: tc.agent, annotation: tc.annotation}
			params := RunnerParams{Env: make(map[string]string)}
			epicID := "epc-result"
			if tc.unrelated {
				epicID = ""
			}
			rec, err := f.d.bindExecution(f.tkt, p, &params, epicID)
			if tc.wantRefuse {
				require.ErrorContains(t, err, "pi result reporting requires TUI mode")
				assert.Nil(t, rec)
				after, readErr := os.ReadFile(executionPath(f.h.cfg, f.tkt.ID))
				require.NoError(t, readErr)
				assert.Equal(t, before, after)
				assert.Equal(t, "inv-result", f.h.readTask(f.tkt.ID+".md").InvocationID)
			} else {
				require.NoError(t, err)
				if tc.annotation {
					assert.Nil(t, rec)
					assert.Empty(t, params.Env["KONTORA_INVOCATION_ID"])
				} else {
					require.NotNil(t, rec)
					assert.Equal(t, epicID, rec.EpicID)
					assert.NotEmpty(t, params.Env["KONTORA_INVOCATION_ID"])
				}
			}
		})
	}
}
