package cli

import (
	_ "embed"
	"fmt"
	"io"
	"strings"
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

const SetupRecipeHelp = "Print YAML for implement-review or plan-only. Replace YOUR_AGENT with a configured agent name and merge without replacing existing entries."

// WriteSetupRecipe prints an editable YAML fragment without loading a config.
func WriteSetupRecipe(name string, w io.Writer) error {
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
	return fmt.Errorf("unknown setup recipe %q (choose %s or %s)", name, implementReviewRecipe.name, planOnlyRecipe.name)
}
