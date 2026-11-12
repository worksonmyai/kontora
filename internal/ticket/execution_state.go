package ticket

import (
	"fmt"
	"regexp"

	"gopkg.in/yaml.v3"
)

var customRunningStatusName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// IsCustomRunningStatus does not consult config: a running snapshot survives a removed declaration.
func IsCustomRunningStatus(status Status) bool {
	switch status {
	case StatusOpen, StatusTodo, StatusInProgress, StatusPaused, StatusHumanReview,
		StatusDone, StatusCancelled, StatusArchived, StatusLegacyClosed, "next", "retry", "back":
		return false
	}
	return customRunningStatusName.MatchString(string(status))
}

func (t *Ticket) IsRunning() bool {
	return t != nil && (t.Status == StatusInProgress ||
		(t.Status == t.RunningStatus && IsCustomRunningStatus(t.Status)))
}

// SetRunningStatus installs the claim's status and snapshot in one YAML mutation.
func (t *Ticket) SetRunningStatus(status Status) error {
	if status != StatusInProgress && !IsCustomRunningStatus(status) {
		return fmt.Errorf("invalid running status %q", status)
	}
	candidate := t.Clone()
	if err := candidate.setFieldNode("status", string(status)); err != nil {
		return err
	}
	if err := candidate.clearRunningStatus(); err != nil {
		return err
	}
	if status != StatusInProgress {
		if err := candidate.setFieldNode("running_status", string(status)); err != nil {
			return err
		}
	}
	if err := candidate.syncFromNode(); err != nil {
		return err
	}
	*t = *candidate
	return nil
}

func (t *Ticket) clearRunningStatus() error {
	if err := t.deleteFieldNode("running_status"); err != nil {
		return err
	}
	// DeleteField may already have removed the anchor. Decode follows alias pointers,
	// but Marshal emits alias names, so materialize references to removed scalars.
	materializeDanglingScalarAliases(t.rawNode)
	var inherited struct {
		RunningStatus Status `yaml:"running_status"`
	}
	if err := t.rawNode.Decode(&inherited); err != nil {
		return err
	}
	// Deleting a direct key can expose an active snapshot from a YAML merge.
	if inherited.RunningStatus != "" {
		return t.setFieldNode("running_status", "")
	}
	return nil
}

func materializeDanglingScalarAliases(root *yaml.Node) {
	nodes := make(map[*yaml.Node]bool)
	var collect func(*yaml.Node)
	collect = func(node *yaml.Node) {
		nodes[node] = true
		for _, child := range node.Content {
			collect(child)
		}
	}
	collect(root)
	for node := range nodes {
		if node.Kind != yaml.AliasNode || nodes[node.Alias] || node.Alias.Kind != yaml.ScalarNode {
			continue
		}
		resolved := *node.Alias
		resolved.Anchor = ""
		resolved.HeadComment = node.HeadComment
		resolved.LineComment = node.LineComment
		resolved.FootComment = node.FootComment
		*node = resolved
	}
}
