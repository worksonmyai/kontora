package cli

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/ticket/store"
)

// Pause sets a ticket's status to paused.
func Pause(tasksDir, taskID string) error {
	return SetStatus(tasksDir, taskID, "paused")
}

// Retry resets a ticket to todo with attempt=0 for re-processing.
func Retry(tasksDir, taskID string) error {
	repo := store.NewDiskRepo(tasksDir)
	svc := app.New(nil, repo, app.NoopRuntime{})
	_, err := svc.Retry(taskID)
	return err
}

// Cancel sets a ticket's status to cancelled.
func Cancel(tasksDir, taskID string) error {
	return SetStatus(tasksDir, taskID, "cancelled")
}

// SetStage moves a ticket to a specific pipeline stage by name.
func SetStage(cfg *config.Config, taskID, targetStage string) error {
	tasksDir := config.ExpandTilde(cfg.TicketsDir)
	resolvedID, err := resolveTaskID(tasksDir, taskID)
	if err != nil {
		return err
	}

	filePath := filepath.Join(tasksDir, resolvedID+".md")
	t, err := ticket.ParseFile(filePath)
	if err != nil {
		return fmt.Errorf("parsing ticket: %w", err)
	}

	pipelineCfg, ok := cfg.Pipelines[t.Pipeline]
	if !ok {
		return fmt.Errorf("unknown pipeline %q for ticket %s", t.Pipeline, resolvedID)
	}

	found := false
	for _, step := range pipelineCfg {
		if step.Stage == targetStage {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("stage %q not found in pipeline %q", targetStage, t.Pipeline)
	}

	if err := t.SetField("stage", targetStage); err != nil {
		return fmt.Errorf("setting stage: %w", err)
	}

	out, err := t.Marshal()
	if err != nil {
		return fmt.Errorf("marshalling ticket: %w", err)
	}
	return os.WriteFile(filePath, out, 0o644)
}

// Run enqueues a ticket for processing via the daemon API.
func Run(cfg *config.Config, taskID string) error {
	addr := "http://" + net.JoinHostPort(cfg.Web.Host, strconv.Itoa(cfg.Web.Port))

	// Resolve ticket ID prefix for the API call.
	tasksDir := config.ExpandTilde(cfg.TicketsDir)
	resolvedID, err := resolveTaskID(tasksDir, taskID)
	if err != nil {
		return err
	}

	resp, err := http.Post(addr+"/api/tickets/"+resolvedID+"/run", "", nil)
	if err != nil {
		return fmt.Errorf("daemon not reachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var body struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		if body.Error != "" {
			return fmt.Errorf("%s", body.Error)
		}
		return fmt.Errorf("daemon returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// Skip advances a ticket to the next pipeline stage, or marks it done
// if it is on the final stage.
func Skip(cfg *config.Config, taskID string) error {
	repo := store.NewDiskRepo(cfg.TicketsDir)
	svc := app.New(cfg, repo, app.NoopRuntime{})
	_, err := svc.Skip(taskID)
	return err
}
