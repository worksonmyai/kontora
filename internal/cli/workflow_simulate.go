package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/pipeline"
	"github.com/worksonmyai/kontora/internal/ticket"
)

type WorkflowSimulationOptions struct {
	Recipe, Config, Pipeline, Scenario string
	Preflight                          bool
}

// WorkflowSimulate reads only explicitly selected input files and writes the
// report to out. It does not construct a daemon/client, store, or runner.
func WorkflowSimulate(out io.Writer, opts WorkflowSimulationOptions) error {
	if opts.Preflight == (opts.Scenario != "") {
		return fmt.Errorf("select exactly one of --scenario FILE and --preflight")
	}
	graph, err := simulationGraph(opts)
	if err != nil {
		return err
	}
	var report strings.Builder
	var resultErr error
	if opts.Preflight {
		if slices.ContainsFunc(graph.Nodes, config.WorkflowNode.CommandCondition) {
			report.WriteString("Preflight uses only synthetic exit codes 0 and 1 for command conditions.\n")
		}
		result := pipeline.PreflightGraph(graph, pipeline.GraphPreflightLimit)
		for _, d := range result.Diagnostics {
			fmt.Fprintf(&report, "Rejected at %q: %s\n", d.Node, d.Reason)
			if p := d.Producer; p != nil {
				fmt.Fprintf(&report, "  Producer %q: %s, exit_code=%d", p.NodeID, p.Outcome, *p.ExitCode)
				if p.Verdict != "" {
					fmt.Fprintf(&report, ", verdict=%q", p.Verdict)
				}
				report.WriteByte('\n')
			} else {
				report.WriteString("  No preceding executable result\n")
			}
			fmt.Fprintf(&report, "  Witness (entry %q):\n", graph.Entry)
			writeSimulationEdges(&report, d.Path)
		}
		switch {
		case result.Inconclusive:
			fmt.Fprintf(&report, "Preflight inconclusive: exploration cap reached (%d interpreter steps).\n", result.Explored)
			resultErr = errors.New("preflight inconclusive")
		case len(result.Diagnostics) != 0:
			fmt.Fprintf(&report, "Preflight found %d diagnostic(s) in %d interpreter steps.\n", len(result.Diagnostics), result.Explored)
			resultErr = errors.New("preflight found rejected paths")
		default:
			fmt.Fprintf(&report, "Preflight found no rejected paths in %d interpreter steps.\n", result.Explored)
		}
	} else {
		events, err := readWorkflowScenario(opts.Scenario)
		if err != nil {
			return fmt.Errorf("reading scenario: %w", err)
		}
		result := pipeline.SimulateGraph(graph, events)
		fmt.Fprintf(&report, "Entry: %s\n", graph.Entry)
		writeSimulationEdges(&report, result.Transitions)
		fmt.Fprintf(&report, "Final: %s (%s, status=%s)", result.Cursor, result.Stop, result.Status)
		if result.Reason != "" {
			fmt.Fprintf(&report, ": %s", result.Reason)
		}
		report.WriteByte('\n')
		if result.Stop == "rejected" {
			resultErr = errors.New("scenario transition rejected")
		}
	}
	if _, err := io.WriteString(out, report.String()); err != nil {
		return err
	}
	return resultErr
}

func simulationGraph(opts WorkflowSimulationOptions) (*config.WorkflowGraph, error) {
	if opts.Recipe != "" {
		if opts.Config != "" || opts.Pipeline != "" {
			return nil, fmt.Errorf("--recipe cannot be combined with --config or --pipeline")
		}
		if opts.Recipe != graphRecipeName {
			return nil, fmt.Errorf("unknown graph recipe %q (choose %s)", opts.Recipe, graphRecipeName)
		}
		const agent = "synthetic-agent"
		recipe, graph := graphRecipe(agent)
		cfg := config.Config{
			DefaultAgent: agent, TmuxSession: "offline",
			Agents:    map[string]config.Agent{agent: {Binary: agent}},
			Stages:    make(map[string]config.Stage, len(recipe.steps)),
			Pipelines: map[string]config.PipelineDefinition{recipe.name: {Graph: graph}},
		}
		for _, step := range recipe.steps {
			cfg.Stages[step.name] = config.Stage{Prompt: step.prompt}
		}
		if err := cfg.Validate(); err != nil {
			return nil, fmt.Errorf("validating graph recipe: %w", err)
		}
		return graph, nil
	}
	if opts.Config == "" || opts.Pipeline == "" {
		return nil, fmt.Errorf("select either --recipe NAME or both --config FILE and --pipeline NAME")
	}
	cfg, err := config.Load(opts.Config)
	if err != nil {
		return nil, err
	}
	definition, ok := cfg.Pipelines[opts.Pipeline]
	if !ok {
		return nil, fmt.Errorf("unknown pipeline %q", opts.Pipeline)
	}
	if !definition.IsGraph() {
		return nil, fmt.Errorf("pipeline %q is not a graph", opts.Pipeline)
	}
	return definition.Graph, nil
}

func writeSimulationEdges(w *strings.Builder, edges []ticket.GraphTransition) {
	for _, edge := range edges {
		fmt.Fprintf(w, "  %s --%s--> %s", edge.From, edge.Port, edge.To)
		if edge.LoopCount != 0 {
			fmt.Fprintf(w, " (loop count %d)", edge.LoopCount)
		}
		w.WriteByte('\n')
	}
}

func readWorkflowScenario(path string) ([]pipeline.GraphScenarioEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	var events []pipeline.GraphScenarioEvent
	if err := dec.Decode(&events); err != nil {
		return nil, err
	}
	if events == nil {
		return nil, fmt.Errorf("expected a JSON array of events")
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("unexpected data after scenario array")
	}
	return events, nil
}
