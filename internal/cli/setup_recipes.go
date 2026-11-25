package cli

import (
	_ "embed"
	"fmt"
	"io"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/worksonmyai/kontora/internal/config"
)

//go:embed starter-prompts/implement.md
var starterImplementPrompt string

//go:embed starter-prompts/review.md
var starterReviewPrompt string

//go:embed starter-prompts/plan.md
var starterPlanPrompt string

type setupRecipe struct {
	name  string
	steps []setupRecipeStage
}

type setupRecipeStage struct {
	name    string
	prompt  string
	timeout string
}

var (
	implementReviewRecipe = setupRecipe{
		name: "implement-review",
		steps: []setupRecipeStage{
			{name: "starter-implement", prompt: starterImplementPrompt, timeout: "60m"},
			{name: "starter-review", prompt: starterReviewPrompt, timeout: "30m"},
		},
	}
	planOnlyRecipe = setupRecipe{
		name: "plan-only",
		steps: []setupRecipeStage{
			{name: "starter-plan", prompt: starterPlanPrompt, timeout: "30m"},
		},
	}
	setupRecipes = []setupRecipe{implementReviewRecipe, planOnlyRecipe}
)

func (r setupRecipe) writeStages(b *strings.Builder) {
	for _, stage := range r.steps {
		fmt.Fprintf(b, "  %s:\n    prompt: |\n", stage.name)
		for line := range strings.SplitSeq(strings.TrimRight(stage.prompt, "\n"), "\n") {
			if line != "" {
				b.WriteString("      " + line)
			}
			b.WriteByte('\n')
		}
		fmt.Fprintf(b, "    timeout: %s\n\n", stage.timeout)
	}
}

func (r setupRecipe) writePipeline(b *strings.Builder, agent string) {
	fmt.Fprintf(b, "  %s:\n", r.name)
	for i, step := range r.steps {
		success := "next"
		if i == len(r.steps)-1 {
			success = "human_review"
		}
		fmt.Fprintf(b, "    - stage: %s\n      agent: %s\n      on_success: %s\n      on_failure: pause\n",
			step.name, yamlQuote(agent), success)
	}
}

const graphRecipeName = "implement-test-review"

// The setup wizard must not install this opt-in recipe.
func graphRecipe(agent string) (setupRecipe, *config.WorkflowGraph) {
	const implementStage = "starter-graph-implement"
	const repairStage = "starter-graph-repair"
	const reviewStage = "starter-graph-review"
	recipe := setupRecipe{name: graphRecipeName, steps: []setupRecipeStage{
		{name: implementStage, prompt: starterImplementPrompt, timeout: "60m"},
		{name: repairStage, timeout: "60m", prompt: `Repair the failing tests for ticket {{ .Ticket.ID }} in the existing ticket worktree. Read the repository instructions before editing.

{{ .Ticket.Description }}

Use **Workflow predecessor evidence** if supplied. Otherwise run the configured test command below. If the command is not configured, report what you need to continue.

Test command: YOUR_TEST_COMMAND

Fix the cause without weakening checks to make them pass. Leave the changes in the worktree. Do not commit, push, or open a pull request.

Save a handoff note with what changed, which checks ran, any failures, and what you could not verify. Use the handoff instructions below. Follow the result instructions if the run provides them.`},
		{name: reviewStage, timeout: "30m", prompt: strings.TrimSpace(starterReviewPrompt) + `

If you cannot verify the candidate, skip inspection, explain why, and report complete with changes_requested. Use the same result for actionable defects. Report complete with approved only after verifying and inspecting a candidate that needs no changes. Do not open a pull request. A verdict is not permission to publish.`},
	}}
	edge := func(from, port, to string) config.WorkflowEdge {
		return config.WorkflowEdge{From: config.WorkflowPort{Node: from, Port: port}, To: to}
	}
	graph := &config.WorkflowGraph{Version: 2, Entry: "implement",
		Nodes: []config.WorkflowNode{
			{ID: "implement", Type: "agent", Stage: implementStage, Agent: agent},
			{ID: "tests", Type: "check", Command: "exit 2", Timeout: config.Duration{Duration: 10 * time.Minute}},
			{ID: "repairable", Type: "condition", When: &config.WorkflowCondition{Field: "exit_code", Equals: 1}},
			{ID: "repair-budget", Type: "loop", Max: 2},
			{ID: "repair", Type: "agent", Stage: repairStage, Agent: agent},
			{ID: "review", Type: "agent", Stage: reviewStage, Agent: agent, Verdicts: []string{"approved", "changes_requested"}},
			{ID: "approved", Type: "condition", When: &config.WorkflowCondition{Field: "verdict", Equals: "approved"}},
			{ID: "release", Type: "wait", Mode: "human", Timeout: config.Duration{Duration: 24 * time.Hour}},
			{ID: "handoff", Type: "end", Status: "human_review"},
			{ID: "inspection", Type: "end", Status: "paused"},
		},
		Edges: []config.WorkflowEdge{
			edge("implement", "complete", "tests"), edge("implement", "failed", "inspection"),
			edge("tests", "pass", "review"), edge("tests", "fail", "repairable"),
			edge("repairable", "true", "repair-budget"), edge("repairable", "false", "inspection"),
			edge("repair-budget", "repeat", "repair"), edge("repair-budget", "exhausted", "inspection"),
			edge("repair", "complete", "tests"), edge("repair", "failed", "inspection"),
			edge("review", "complete", "approved"), edge("review", "failed", "inspection"),
			edge("approved", "true", "release"), edge("approved", "false", "inspection"),
			edge("release", "released", "handoff"), edge("release", "timeout", "inspection"),
		},
	}
	return recipe, graph
}

const SetupRecipeHelp = "Print YAML for implement-review, plan-only, or implement-test-review. Replace YOUR_AGENT with a configured agent name and merge without replacing existing entries."

// WriteSetupRecipe prints an editable YAML fragment without loading a config.
func WriteSetupRecipe(name string, w io.Writer) error {
	if name == graphRecipeName {
		recipe, graph := graphRecipe("YOUR_AGENT")
		var b strings.Builder
		b.WriteString("stages:\n")
		recipe.writeStages(&b)
		b.WriteString("# Replace the tests node's exit 2 with a trusted test command.\n" +
			"# Use the same command in place of YOUR_TEST_COMMAND in the repair prompt.\n" +
			"# Use 0 for pass, 1 only for repairable test failures, and another nonzero exit for other failures.\n")
		err := yaml.NewEncoder(&b).Encode(map[string]map[string]*config.WorkflowGraph{
			"pipelines": {recipe.name: graph},
		})
		if err != nil {
			return err
		}
		_, err = io.WriteString(w, b.String())
		return err
	}
	for _, recipe := range setupRecipes {
		if recipe.name == name {
			var b strings.Builder
			b.WriteString("stages:\n")
			recipe.writeStages(&b)
			b.WriteString("pipelines:\n")
			recipe.writePipeline(&b, "YOUR_AGENT")
			_, err := io.WriteString(w, b.String())
			return err
		}
	}
	if name == "" {
		return fmt.Errorf("--recipe requires a name")
	}
	return fmt.Errorf("unknown setup recipe %q (choose %s, %s, or %s)", name, implementReviewRecipe.name, planOnlyRecipe.name, graphRecipeName)
}
