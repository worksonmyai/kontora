package cli

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/worksonmyai/kontora/internal/automation"
	"github.com/worksonmyai/kontora/internal/cli/remote"
)

func NewAutomation(client *remote.Client, key, requestID string, spec automation.Spec, diagnostics io.Writer) (automation.Definition, error) {
	if key != "" {
		if err := automation.ValidateKey(key); err != nil {
			return automation.Definition{}, err
		}
	}
	spec = spec.Normalize()
	if err := spec.Validate(); err != nil {
		return automation.Definition{}, err
	}
	requestID, err := automationRequestID(requestID)
	if err != nil {
		return automation.Definition{}, err
	}
	if err := automationDiagnostic(diagnostics, "request_id: %s\n", requestID); err != nil {
		return automation.Definition{}, err
	}
	return client.CreateAutomation(requestID, key, spec)
}

func automationRequestID(id string) (string, error) {
	if id != "" {
		if strings.TrimSpace(id) == "" || len(id) > 200 {
			return "", fmt.Errorf("%w: request_id must contain 1 to 200 bytes", automation.ErrValidation)
		}
		return id, nil
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(nonce[:]), nil
}

func automationDiagnostic(w io.Writer, format string, args ...any) error {
	if w == nil {
		w = os.Stderr
	}
	_, err := fmt.Fprintf(w, format, args...)
	return err
}

func ResolveAutomation(client *remote.Client, ref string) (string, error) {
	// Retained runs remain addressable after their definition has been deleted.
	if automation.IsID(ref) {
		return ref, nil
	}
	response, err := client.Automations()
	if err != nil {
		return "", err
	}
	if !response.Available {
		if response.Error != "" {
			return "", fmt.Errorf("%w: %s", automation.ErrUnavailable, response.Error)
		}
		return "", automation.ErrUnavailable
	}
	for _, row := range response.Definitions {
		if row.Key == ref {
			return row.ID, nil
		}
	}
	return "", fmt.Errorf("%w: no active automation with key %q", automation.ErrNotFound, ref)
}

type AutomationUpdate struct {
	Name      *string
	Prompt    *string
	Path      *string
	Agent     *string
	Model     *string
	Schedule  *string
	Timezone  *string
	Workspace *string
	Overlap   *string
	Timeout   *string
	Enabled   *bool
}

func UpdateAutomation(client *remote.Client, id string, revision uint64, changes AutomationUpdate) (automation.Definition, error) {
	if changes == (AutomationUpdate{}) {
		return automation.Definition{}, fmt.Errorf("%w: no automation changes supplied", automation.ErrValidation)
	}
	row, err := client.Automation(id)
	if err != nil {
		return automation.Definition{}, err
	}
	spec := row.Spec
	for _, field := range []struct {
		value *string
		dest  *string
	}{
		{changes.Name, &spec.Name}, {changes.Prompt, &spec.Prompt},
		{changes.Path, &spec.Path}, {changes.Agent, &spec.Agent},
		{changes.Model, &spec.Model}, {changes.Schedule, &spec.Schedule},
		{changes.Timezone, &spec.Timezone}, {changes.Workspace, &spec.Workspace},
		{changes.Overlap, &spec.Overlap}, {changes.Timeout, &spec.Timeout},
	} {
		if field.value != nil {
			*field.dest = *field.value
		}
	}
	if changes.Enabled != nil {
		spec.Enabled = *changes.Enabled
	}
	spec = spec.Normalize()
	if err := spec.Validate(); err != nil {
		return automation.Definition{}, err
	}
	if revision == 0 {
		revision = row.Revision
	}
	return client.UpdateAutomation(id, revision, spec)
}

// A zero revision means that the caller did not supply a precondition.
func automationRevision(client *remote.Client, id string, revision uint64) (uint64, error) {
	if revision != 0 {
		return revision, nil
	}
	row, err := client.Automation(id)
	return row.Revision, err
}

func DeleteAutomation(client *remote.Client, id string, revision uint64) error {
	revision, err := automationRevision(client, id, revision)
	if err != nil {
		return err
	}
	return client.DeleteAutomation(id, revision)
}

func PauseAutomation(client *remote.Client, id string, revision uint64) (automation.Definition, error) {
	revision, err := automationRevision(client, id, revision)
	if err != nil {
		return automation.Definition{}, err
	}
	return client.PauseAutomation(id, revision)
}

func ResumeAutomation(client *remote.Client, id string, revision uint64) (automation.Definition, error) {
	revision, err := automationRevision(client, id, revision)
	if err != nil {
		return automation.Definition{}, err
	}
	return client.ResumeAutomation(id, revision)
}

func RunAutomationRequest(client *remote.Client, ref, requestID string, diagnostics io.Writer) (automation.Run, error) {
	if requestID != "" && !automation.IsID(ref) {
		return automation.Run{}, fmt.Errorf("%w: --request-id requires the original full automation ID", automation.ErrValidation)
	}
	requestID, err := automationRequestID(requestID)
	if err != nil {
		return automation.Run{}, err
	}
	id, err := ResolveAutomation(client, ref)
	if err != nil {
		return automation.Run{}, err
	}
	if err := automationDiagnostic(diagnostics, "automation_id: %s\nrequest_id: %s\n", id, requestID); err != nil {
		return automation.Run{}, err
	}
	return client.RunAutomation(id, requestID)
}

type AutomationFilter struct {
	State   string
	Query   string
	Project string
	Agent   string
}

func FilterAutomations(rows []automation.Definition, filter AutomationFilter) []automation.Definition {
	out := make([]automation.Definition, 0, len(rows))
	query := strings.ToLower(filter.Query)
	for _, row := range rows {
		switch filter.State {
		case "", "all":
		case "enabled":
			if !row.Enabled {
				continue
			}
		case "paused":
			if row.Enabled {
				continue
			}
		default:
			continue
		}
		if filter.Project != "" && row.Project != filter.Project || filter.Agent != "" && row.Agent != filter.Agent {
			continue
		}
		search := strings.Join([]string{row.ID, row.Key, row.Name, row.Schedule, row.ScheduleHuman, row.Path, row.Agent, row.Project}, " ")
		if !strings.Contains(strings.ToLower(search), query) {
			continue
		}
		out = append(out, row)
	}
	slices.SortStableFunc(out, func(a, b automation.Definition) int { return strings.Compare(a.Key, b.Key) })
	return out
}

func WriteAutomation(w io.Writer, row automation.Definition) error {
	var b strings.Builder
	fmt.Fprintf(&b, "id: %s\nkey: %s\nrevision: %d\n", row.ID, row.Key, row.Revision)
	writeAutomationSpec(&b, row.Spec, "")
	fmt.Fprintf(&b, "project: %s\ncreated_at: %s\nschedule_human: %s\nnext: %s\n", row.Project, automationTime(row.CreatedAt), row.ScheduleHuman, automationTime(row.Next))
	fmt.Fprintln(&b, "next_runs:")
	for _, at := range row.NextRuns {
		fmt.Fprintf(&b, "  %s\n", automationTime(at))
	}
	fmt.Fprintf(&b, "running: %t\nruns_total: %d\n", row.Running, row.RunsTotal)
	fmt.Fprintf(&b, "success_rate: %s\navg_wall: %s\nspend_30d: %s\npriced_runs: %d\ntracked_runs: %d\n",
		automationFloat(row.SuccessRate, "%"), automationFloat(row.AvgWall, "s"), automationNullable(row.Spend30d), row.PricedRuns, row.TrackedRuns)
	for _, entry := range []struct {
		label string
		run   *automation.Run
	}{{"current_run", row.CurrentRun}, {"last_run", row.LastRun}} {
		if entry.run == nil {
			fmt.Fprintf(&b, "%s: none\n", entry.label)
		} else {
			fmt.Fprintf(&b, "%s:\n", entry.label)
			writeAutomationRun(&b, *entry.run, "  ")
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func WriteAutomations(w io.Writer, rows []automation.Definition) error {
	if len(rows) == 0 {
		_, err := io.WriteString(w, "No automations.\n")
		return err
	}
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tKEY\tNAME\tSTATE\tPHASE\tSCHEDULE\tTIMEZONE\tNEXT\tAGENT\tLAST OUTCOME")
	for _, row := range FilterAutomations(rows, AutomationFilter{}) {
		state, phase, outcome := "paused", "none", "unknown"
		if row.Enabled {
			state = "enabled"
		}
		if row.CurrentRun != nil {
			phase = automationKnown(row.CurrentRun.Phase)
		} else if row.Running {
			phase = "unknown"
		}
		if row.LastRun != nil {
			outcome = automationKnown(row.LastRun.Outcome)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", row.ID, row.Key, row.Name, state, phase, row.Schedule, row.Timezone, automationTime(row.Next), row.Agent, outcome)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func WriteAutomationRun(w io.Writer, run automation.Run) error {
	var b strings.Builder
	writeAutomationRun(&b, run, "")
	_, err := io.WriteString(w, b.String())
	return err
}

func WriteAutomationRuns(w io.Writer, runs []automation.Run) error {
	if len(runs) == 0 {
		_, err := io.WriteString(w, "No automation runs.\n")
		return err
	}
	for i, run := range runs {
		if i > 0 {
			if _, err := io.WriteString(w, "\n"); err != nil {
				return err
			}
		}
		if err := WriteAutomationRun(w, run); err != nil {
			return err
		}
	}
	return nil
}

func WriteAutomationLogs(w io.Writer, lines []automation.LogLine) error {
	for _, line := range lines {
		if _, err := fmt.Fprintf(w, "%s [%s] [%s] %s\n", automationTime(line.At), line.Source, line.Kind, line.Text); err != nil {
			return err
		}
	}
	return nil
}

func writeAutomationSpec(b *strings.Builder, spec automation.Spec, prefix string) {
	for _, field := range []struct{ name, value string }{
		{"name", spec.Name}, {"prompt", spec.Prompt}, {"path", spec.Path},
		{"agent", spec.Agent}, {"model", spec.Model}, {"schedule", spec.Schedule},
		{"timezone", spec.Timezone}, {"workspace", spec.Workspace}, {"overlap", spec.Overlap}, {"timeout", spec.Timeout},
	} {
		fmt.Fprintf(b, "%s%s: %s\n", prefix, field.name, strings.ReplaceAll(field.value, "\n", "\n"+prefix+"  "))
	}
	fmt.Fprintf(b, "%senabled: %t\n", prefix, spec.Enabled)
}

func writeAutomationRun(b *strings.Builder, run automation.Run, prefix string) {
	fmt.Fprintf(b, "%sautomation_id: %s\n%skey: %s\n%sn: %d\n%srevision: %d\n", prefix, run.AutomationID, prefix, run.Key, prefix, run.N, prefix, run.Revision)
	for _, field := range []struct{ name, value string }{
		{"invocation_id", run.InvocationID}, {"phase", automationKnown(run.Phase)}, {"outcome", automationKnown(run.Outcome)},
		{"trigger", run.Trigger}, {"scheduled_for", automationTime(run.ScheduledFor)}, {"accepted_at", automationTime(run.AcceptedAt)},
		{"started_at", automationTime(run.StartedAt)}, {"completed_at", automationTime(run.CompletedAt)},
		{"exit_code", automationNullable(run.ExitCode)}, {"observed_exit_code", automationNullable(run.ObservedExitCode)},
		{"result", run.Result}, {"cost", automationNullable(run.Cost)}, {"stop_reason", run.StopReason}, {"termination", run.Termination},
		{"session", run.Session}, {"window", run.Window}, {"window_id", run.WindowID}, {"session_id", run.SessionID},
		{"session_dir", run.SessionDir}, {"worktree", run.Worktree}, {"branch", run.Branch}, {"base_commit", run.BaseCommit},
		{"log_path", run.LogPath}, {"result_path", run.ResultPath},
	} {
		fmt.Fprintf(b, "%s%s: %s\n", prefix, field.name, strings.ReplaceAll(field.value, "\n", "\n"+prefix+"  "))
	}
	fmt.Fprintf(b, "%sspec:\n", prefix)
	writeAutomationSpec(b, run.Spec, prefix+"  ")
}

func automationTime(at time.Time) string {
	if at.IsZero() {
		return "unknown"
	}
	return at.Format(time.RFC3339Nano)
}

func automationNullable[T any](value *T) string {
	if value == nil {
		return "unknown"
	}
	return fmt.Sprint(*value)
}

func automationFloat(value *float64, unit string) string {
	if value == nil {
		return "unknown"
	}
	return fmt.Sprintf("%g%s", *value, unit)
}

func automationKnown(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}
