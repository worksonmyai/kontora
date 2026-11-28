package daemon

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

func (d *Daemon) buildRunPreview(cfg *config.Config, t *ticket.Ticket, filePath string) *web.RunPreview {
	_, repo, repoErr := d.resolvePath(t)
	preview := &web.RunPreview{Stages: []string{}, Repo: repo}
	if t.AnnotationReturnStatus != "" {
		preview.Branch = strings.TrimSpace(t.Branch)
		if t.Path == "" {
			preview.Repo, repoErr = filepath.Dir(filePath), nil
		}
	} else {
		preview.Branch = effectiveClaimBranch(cfg, t)
	}

	err := d.selectRunPreview(cfg, t, preview)
	if err == nil {
		err = repoErr
	}
	if err == nil && preview.Agent != "" {
		if _, ok := cfg.Agents[preview.Agent]; !ok {
			err = fmt.Errorf("unknown agent %q; select a configured agent or restore it in configuration", preview.Agent)
		}
	}
	if err != nil {
		preview.Stages = preview.Stages[:0]
		preview.Agent = ""
		preview.Error = err.Error()
	}
	return preview
}

func (d *Daemon) selectRunPreview(cfg *config.Config, t *ticket.Ticket, preview *web.RunPreview) error {
	definition := cfg.Pipelines[t.Pipeline]
	switch {
	case definition.IsGraph():
		return previewGraphRun(cfg, t, definition, preview)
	case t.GraphState != nil:
		preview.Kind = "graph"
		return errWorkflowDefinitionChanged
	case t.AnnotationReturnStatus != "":
		preview.Kind = "annotation"
		file := annotationsPath(config.ExpandTilde(cfg.Plannotator.ReviewsDir), t.ID)
		pending, err := readPendingAnnotations(file)
		if err != nil {
			return fmt.Errorf("annotation: check the pending notes file: %w", err)
		}
		if pending != "" {
			preview.Agent = d.reworkAgent(cfg, t)
		}
	case t.IsAgentEpic() || t.Pipeline == "":
		preview.Agent = simpleTicketAgent(cfg, t)
	default:
		return d.previewLegacyRun(cfg, t, definition, preview)
	}
	return nil
}

func (d *Daemon) previewLegacyRun(cfg *config.Config, t *ticket.Ticket, definition config.PipelineDefinition, preview *web.RunPreview) error {
	stage, rework, err := legacyPickupStage(cfg, t, definition.Legacy)
	if err != nil {
		return fmt.Errorf("%w; select a configured pipeline or restore it in configuration", err)
	}
	if rework {
		preview.Kind = "rework"
		preview.Stages = append(preview.Stages, stage)
		preview.Agent = d.reworkAgent(cfg, t)
		return nil
	}
	resolved, err := definition.Resolve(stage, t.Agent)
	if err != nil {
		return fmt.Errorf("%w in pipeline %q; select a configured stage or restore it in configuration", err, t.Pipeline)
	}
	preview.Agent = resolved.Agent
	start := slices.IndexFunc(definition.Legacy, func(step config.PipelineStep) bool { return step.Stage == stage })
	for _, step := range definition.Legacy[start:] {
		preview.Stages = append(preview.Stages, step.Stage)
	}
	return nil
}

func previewGraphRun(cfg *config.Config, t *ticket.Ticket, definition config.PipelineDefinition, preview *web.RunPreview) error {
	preview.Kind = "graph"
	graph := definition.Graph
	fingerprint, err := workflowPickupFingerprint(cfg, t, graph)
	if err != nil {
		return err
	}
	state, stage, err := workflowPickupCursor(t, graph, fingerprint, func() string { return "preview" })
	if err != nil {
		return err
	}
	after, err := advanceWorkflowPickup(graph, stage, state)
	if err != nil {
		if errors.Is(err, errWorkflowWaitNotReady) {
			preview.Kind = "wait"
		}
		return err
	}
	if after.Rejected {
		if stage == "" {
			stage = graph.Entry
		}
		return fmt.Errorf("workflow node %q: %s; inspect the saved workflow before running", stage, after.Reason)
	}
	preview.Stages = append(preview.Stages, after.Stage)
	switch {
	case after.Next != nil:
		resolved, err := definition.Resolve(after.Next.ID, t.Agent)
		if err != nil {
			return err
		}
		if resolved.Kind == "check" {
			preview.Kind = "check"
		}
		preview.Agent = resolved.Agent
	case after.Condition != nil:
		preview.Kind = "condition"
	case after.Wait != nil:
		preview.Kind = "wait"
	default:
		preview.Kind = "end"
	}
	return nil
}
