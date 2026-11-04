package companion

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

const snapshotVersion = 1

var (
	ErrNotFound = errors.New("companion item not found")
	ErrConflict = errors.New("companion revision conflict")
)

type Observation struct {
	OccurredAt      time.Time
	Source          string
	TicketID        string
	Condition       string
	Revision        string
	Title           string
	Detail          string
	IdentityLimited bool
}

type Item struct {
	ID              string    `json:"id"`
	Source          string    `json:"source"`
	TicketID        string    `json:"ticket_id"`
	Condition       string    `json:"condition"`
	Revision        uint64    `json:"revision"`
	ChangeRevision  uint64    `json:"change_revision"`
	SourceRevision  string    `json:"source_revision"`
	Title           string    `json:"title"`
	Detail          string    `json:"detail"`
	IdentityLimited bool      `json:"identity_limited"`
	FirstSeen       time.Time `json:"first_seen"`
	ChangedAt       time.Time `json:"changed_at"`
	Active          bool      `json:"active"`
	Read            bool      `json:"read"`
	InactiveReason  string    `json:"inactive_reason"`
}

type State struct {
	Version         int                  `json:"version"`
	Revision        uint64               `json:"revision"`
	CaughtUp        uint64               `json:"caught_up"`
	ObservedAt      time.Time            `json:"observed_at"`
	Items           []Item               `json:"items"`
	SeenCompletions map[string]time.Time `json:"seen_completions,omitempty"`
}

type sourceKey struct {
	source, ticket, condition string
}

func (o Observation) key() sourceKey {
	return sourceKey{o.Source, o.TicketID, o.Condition}
}

func (i Item) key() sourceKey {
	return sourceKey{i.Source, i.TicketID, i.Condition}
}

func (i Item) continuesOccurrence() bool {
	return i.Active || completion(i.Condition) || (i.Condition == "question" && i.InactiveReason != "interrupted")
}

func completion(condition string) bool {
	return condition == "completed" || condition == "completion"
}

func (k sourceKey) completionID() string {
	data, _ := json.Marshal([3]string{k.source, k.ticket, k.condition})
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func (s *State) Reconcile(observations []Observation, now time.Time, startup bool) bool {
	now = now.UTC()
	s.ObservedAt = now
	changed := s.retainCompletionIdentities(now)
	if s.Version != snapshotVersion {
		changed = true
	}
	if startup && s.prune(now) {
		changed = true
	}
	s.Version = snapshotVersion
	if s.Items == nil {
		s.Items = []Item{}
	}
	next := s.Revision + 1
	stamp := func(item *Item) {
		item.Revision = next
		item.ChangeRevision = next
		item.ChangedAt = now
		changed = true
	}

	matches := make(map[sourceKey]int, len(s.Items))
	for n := range s.Items {
		item := &s.Items[n]
		// A previous process cannot prove that its questions were answered.
		if startup && item.Condition == "question" && item.InactiveReason != "interrupted" {
			item.Active = false
			item.InactiveReason = "interrupted"
			stamp(item)
		}
		if item.continuesOccurrence() {
			matches[item.key()] = n
		}
	}

	seen := make(map[int]bool, len(observations))
	for _, observation := range observations {
		key := observation.key()
		n, found := matches[key]
		if completion(observation.Condition) {
			accept, added := s.acceptCompletion(observation, now, found)
			changed = changed || added
			if !accept {
				continue
			}
		}
		if !found {
			n = len(s.Items)
			identity, _ := json.Marshal([]any{observation.Source, observation.TicketID, observation.Condition, next})
			hash := sha256.Sum256(identity)
			s.Items = append(s.Items, Item{
				ID:        hex.EncodeToString(hash[:16]),
				Source:    observation.Source,
				TicketID:  observation.TicketID,
				Condition: observation.Condition,
				FirstSeen: now,
			})
			matches[key] = n
		}
		seen[n] = true
		item := &s.Items[n]
		active, reason := true, ""
		if completion(observation.Condition) {
			active, reason = false, "completed"
		}
		if !found || item.SourceRevision != observation.Revision || item.Title != observation.Title ||
			item.Detail != observation.Detail || item.IdentityLimited != observation.IdentityLimited ||
			item.Active != active || item.InactiveReason != reason {
			item.SourceRevision = observation.Revision
			item.Title = observation.Title
			item.Detail = observation.Detail
			item.IdentityLimited = observation.IdentityLimited
			item.Active = active
			item.InactiveReason = reason
			stamp(item)
		}
	}
	for n := range s.Items {
		item := &s.Items[n]
		if item.Active && !seen[n] {
			item.Active = false
			item.InactiveReason = "no_longer_observed"
			stamp(item)
		}
	}
	if s.prune(now) {
		changed = true
	}
	if changed {
		s.Revision = next
	}
	return changed
}

func (s *State) retainCompletionIdentities(now time.Time) bool {
	changed := false
	cutoff := now.Add(-30 * 24 * time.Hour)
	// Count pruning must not make an observed completion a new unread occurrence.
	if s.SeenCompletions == nil {
		for _, item := range s.Items {
			if completion(item.Condition) && !item.FirstSeen.Before(cutoff) {
				if s.SeenCompletions == nil {
					s.SeenCompletions = make(map[string]time.Time)
				}
				s.SeenCompletions[item.key().completionID()] = item.FirstSeen
				changed = true
			}
		}
	}
	for key, occurred := range s.SeenCompletions {
		if occurred.Before(cutoff) {
			delete(s.SeenCompletions, key)
			changed = true
		}
	}
	if len(s.SeenCompletions) == 0 {
		s.SeenCompletions = nil
	}
	return changed
}

func (s *State) acceptCompletion(observation Observation, now time.Time, hasItem bool) (accept, changed bool) {
	occurred := observation.OccurredAt
	if occurred.IsZero() {
		occurred = now
	}
	if occurred.Before(now.Add(-30 * 24 * time.Hour)) {
		return false, false
	}
	identity := observation.key().completionID()
	if _, seen := s.SeenCompletions[identity]; seen {
		return hasItem, false
	}
	if s.SeenCompletions == nil {
		s.SeenCompletions = make(map[string]time.Time)
	}
	s.SeenCompletions[identity] = occurred
	return true, true
}

func (s *State) SetRead(id string, revision uint64, read bool) error {
	for n := range s.Items {
		item := &s.Items[n]
		if item.ID != id {
			continue
		}
		if item.Revision != revision {
			return fmt.Errorf("%w: item %s has revision %d", ErrConflict, id, item.Revision)
		}
		if item.Read != read {
			s.Revision++
			item.Revision = s.Revision
			item.Read = read
		}
		return nil
	}
	return fmt.Errorf("%w: %s", ErrNotFound, id)
}

func (s *State) CatchUp(revision uint64) error {
	if revision > s.Revision || revision < s.CaughtUp {
		return fmt.Errorf("%w: checkpoint %d is outside [%d, %d]", ErrConflict, revision, s.CaughtUp, s.Revision)
	}
	if revision != s.CaughtUp {
		s.CaughtUp = revision
		s.Revision++
	}
	return nil
}

func (s *State) prune(now time.Time) bool {
	cutoff := now.Add(-30 * 24 * time.Hour)
	inactive := make([]int, 0, len(s.Items))
	for n, item := range s.Items {
		if !item.Active && !item.ChangedAt.Before(cutoff) {
			inactive = append(inactive, n)
		}
	}
	sort.SliceStable(inactive, func(a, b int) bool {
		i, j := s.Items[inactive[a]], s.Items[inactive[b]]
		if !i.ChangedAt.Equal(j.ChangedAt) {
			return i.ChangedAt.After(j.ChangedAt)
		}
		return i.Revision > j.Revision
	})
	if len(inactive) > 500 {
		inactive = inactive[:500]
	}
	keep := make(map[int]bool, len(inactive))
	for _, n := range inactive {
		keep[n] = true
	}
	items := make([]Item, 0, len(s.Items))
	for n, item := range s.Items {
		if item.Active || keep[n] {
			items = append(items, item)
		}
	}
	if len(items) == len(s.Items) {
		return false
	}
	s.Items = items
	return true
}
