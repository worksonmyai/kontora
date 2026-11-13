package ticket

import (
	"slices"
	"time"

	"gopkg.in/yaml.v3"
)

// Clone detaches typed fields and the preserved YAML graph, including alias targets.
func (t *Ticket) Clone() *Ticket {
	if t == nil {
		return nil
	}
	out := *t
	out.GraphState = t.GraphState.Clone()
	out.StartedAt = cloneTime(t.StartedAt)
	out.CompletedAt = cloneTime(t.CompletedAt)
	out.Created = cloneTime(t.Created)
	out.ArchivedAt = cloneTime(t.ArchivedAt)
	out.History = slices.Clone(t.History)
	for i := range out.History {
		out.History[i].StartedAt = cloneTime(t.History[i].StartedAt)
		out.History[i].CompletedAt = cloneTime(t.History[i].CompletedAt)
	}
	out.Deps = slices.Clone(t.Deps)
	out.Links = slices.Clone(t.Links)
	out.Children = slices.Clone(t.Children)
	out.Notify.Statuses = slices.Clone(t.Notify.Statuses)
	out.NotifyChannels = slices.Clone(t.NotifyChannels)
	if t.Managed != nil {
		owned := *t.Managed
		out.Managed = &owned
	}
	out.rawNode = cloneNode(t.rawNode, map[*yaml.Node]*yaml.Node{})
	return &out
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	out := *t
	return &out
}

func cloneNode(n *yaml.Node, seen map[*yaml.Node]*yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	if cloned, ok := seen[n]; ok {
		return cloned
	}
	out := *n
	seen[n] = &out
	out.Content = make([]*yaml.Node, len(n.Content))
	for i, child := range n.Content {
		out.Content[i] = cloneNode(child, seen)
	}
	out.Alias = cloneNode(n.Alias, seen)
	return &out
}
