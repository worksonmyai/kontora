package cli

import (
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/ticket/store"
)

// relationService builds the application service the relation verbs run on.
// The verbs never edit YAML themselves: the same service backs local file mode,
// the daemon, and the HTTP API, so a rejection means the same thing everywhere.
func relationService(cfg *config.Config) *app.Service {
	return app.New(app.Static(cfg), store.NewDiskRepo(cfg.TicketsDir), app.NoopRuntime{})
}

// Dep records that taskID waits on dependencyID.
func Dep(cfg *config.Config, taskID, dependencyID string) (app.RelationResult, error) {
	return relationService(cfg).AddDependency(taskID, dependencyID)
}

// Undep drops dependencyID from taskID's dependencies.
func Undep(cfg *config.Config, taskID, dependencyID string) (app.RelationResult, error) {
	return relationService(cfg).RemoveDependency(taskID, dependencyID)
}

// Link relates taskID to each of relatedIDs, on both sides.
func Link(cfg *config.Config, taskID string, relatedIDs []string) (app.RelationResult, error) {
	return relationService(cfg).Link(taskID, relatedIDs...)
}

// Unlink removes the relation between taskID and each of relatedIDs.
func Unlink(cfg *config.Config, taskID string, relatedIDs []string) (app.RelationResult, error) {
	return relationService(cfg).Unlink(taskID, relatedIDs...)
}
