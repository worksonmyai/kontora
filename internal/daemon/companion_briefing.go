package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/worksonmyai/kontora/internal/assistant"
	"github.com/worksonmyai/kontora/internal/companion"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/web"
)

const companionEvidenceMax = 32 << 10

type companionEvidence struct {
	Metadata          web.CompanionEvidenceInfo            `json:"metadata"`
	AttentionRevision string                               `json:"attention_revision"`
	CaughtUp          uint64                               `json:"caught_up"`
	Attention         []companion.Item                     `json:"attention"`
	Tickets           []companionEvidenceTicket            `json:"tickets,omitempty"`
	Activities        map[string]companionEvidenceActivity `json:"activities,omitempty"`
}

type companionEvidenceTicket struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	Details   string `json:"details"`
	Summary   string `json:"summary"`
	LastError string `json:"last_error,omitempty"`
	Activity  string `json:"activity,omitempty"`
}

type companionEvidenceActivity struct {
	Query    web.ActivityQuery `json:"query"`
	Revision string            `json:"revision"`
}

func companionClip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func (d *Daemon) collectCompanionEvidence(req web.CompanionMessageRequest) (companionEvidence, error) {
	now := time.Now().UTC()
	snapshot := d.CompanionSnapshot()
	p := companionEvidence{Metadata: web.CompanionEvidenceInfo{ID: assistant.NewID(), Kind: req.Kind, CapturedAt: now, WindowEnd: snapshot.ObservedAt, Revision: snapshot.Revision, Sources: map[string]string{}, Omissions: []string{"Polling can miss transitions. This is recorded evidence, not a complete audit. No patch evidence is included; inspect the Changes tab separately."}}, Attention: snapshot.Items, CaughtUp: snapshot.CaughtUp, AttentionRevision: companionAttentionRevision(snapshot.Items), Activities: map[string]companionEvidenceActivity{}}
	if snapshot.Health != "ok" {
		p.Metadata.Omissions = append(p.Metadata.Omissions, "Companion storage is degraded. Attention may not survive a restart: "+companionClip(snapshot.Error, 512))
	}
	p.Attention = nil
	for _, item := range snapshot.Items {
		if item.Active || item.ChangeRevision > snapshot.CaughtUp {
			p.Attention = append(p.Attention, item)
		}
	}
	slices.SortStableFunc(p.Attention, func(a, b companion.Item) int {
		if a.Active != b.Active {
			if a.Active {
				return -1
			}
			return 1
		}
		return b.ChangedAt.Compare(a.ChangedAt)
	})
	if len(p.Attention) > 100 {
		p.Attention = p.Attention[:100]
		p.Metadata.Omissions = append(p.Metadata.Omissions, "Attention limited to 100 current or recent records.")
	}
	for _, item := range snapshot.Items {
		if p.Metadata.WindowStart.IsZero() || item.FirstSeen.Before(p.Metadata.WindowStart) {
			p.Metadata.WindowStart = item.FirstSeen
		}
	}
	if p.Metadata.WindowStart.IsZero() {
		p.Metadata.WindowStart = snapshot.ObservedAt
	}
	sources := map[string]companionTicket{}
	for _, t := range d.companionTickets() {
		sources[t.ID] = t
	}
	attention := p.Attention[:0]
	for _, item := range p.Attention {
		t, ok := sources[item.TicketID]
		if item.Active && (!ok || t.Revision != item.SourceRevision) {
			p.Metadata.Omissions = append(p.Metadata.Omissions, item.TicketID+": current attention changed since observation and was omitted")
			continue
		}
		if ok {
			p.Metadata.Sources[item.TicketID] = t.Revision
		}
		attention = append(attention, item)
	}
	p.Attention = attention
	for _, id := range req.TicketIDs {
		t, ok := sources[id]
		if !ok {
			return p, web.ErrTicketNotFound
		}
		p.Metadata.Sources[id] = t.Revision
		if req.Kind == "briefing" {
			continue
		}
		d.captureCompanionTicket(&p, t)
	}
	if err := d.revalidateCompanionEvidence(&p); err != nil {
		return p, err
	}
	err := limitCompanionEvidence(&p)
	return p, err
}

func (d *Daemon) revalidateCompanionEvidence(p *companionEvidence) error {
	after := map[string]companionTicket{}
	for _, t := range d.companionTickets() {
		after[t.ID] = t
	}
	for id, revision := range p.Metadata.Sources {
		if after[id].Revision != revision {
			return fmt.Errorf("%w: ticket %s changed while collecting evidence; request it again", web.ErrInvalidState, id)
		}
		if a, ok := p.Activities[id]; ok {
			current, err := d.GetActivity(a.Query)
			if err != nil || companionActivityRevision(current) != a.Revision {
				delete(p.Activities, id)
				for i := range p.Tickets {
					if p.Tickets[i].ID == id {
						p.Tickets[i].Activity = ""
					}
				}
				p.Metadata.Omissions = append(p.Metadata.Omissions, id+": activity changed during capture and was omitted")
			}
		}
	}
	return nil
}

func limitCompanionEvidence(p *companionEvidence) error {
	truncated := false
	for {
		data, err := json.Marshal(p)
		if err != nil {
			return err
		}
		if len(data) <= companionEvidenceMax {
			return nil
		}
		if !truncated {
			p.Metadata.Omissions = append(p.Metadata.Omissions, "Attention or ticket detail omitted to fit the 32 KiB evidence limit.")
			truncated = true
			continue
		}
		if len(p.Attention) > 0 {
			p.Attention = p.Attention[:len(p.Attention)-1]
			continue
		}
		trimmed := false
		for i := range p.Tickets {
			if p.Tickets[i].Activity != "" {
				p.Tickets[i].Activity = ""
				delete(p.Activities, p.Tickets[i].ID)
				trimmed = true
				break
			}
			if p.Tickets[i].Details != "" {
				p.Tickets[i].Details = ""
				trimmed = true
				break
			}
		}
		if !trimmed {
			return fmt.Errorf("%w: evidence metadata exceeds 32 KiB", web.ErrInvalidState)
		}
	}
}

func (d *Daemon) captureCompanionTicket(p *companionEvidence, t companionTicket) {
	summary := t.FinalSummary
	if summary == "" {
		summary = t.Summary
	}
	ev := companionEvidenceTicket{ID: t.ID, Title: companionClip(t.Title, 256), Status: string(t.Status), Details: companionClip(t.Body, 3072), Summary: companionClip(summary, 2048), LastError: companionClip(t.LastError, 1024)}
	if len(ev.Details) < len(t.Body) || len(ev.Summary) < len(summary) || len(ev.LastError) < len(t.LastError) {
		p.Metadata.Omissions = append(p.Metadata.Omissions, t.ID+": details or summary truncated")
	}
	stage := t.Stage
	run := 0
	if len(t.History) > 0 {
		if stage == "" {
			stage = t.History[len(t.History)-1].Stage
		}
		for _, h := range t.History {
			if h.Stage == stage {
				run++
			}
		}
		if t.Live == "" && run > 0 {
			run--
		}
	}
	if stage == "" {
		stage = "default"
	}
	q := web.ActivityQuery{ID: t.ID, Stage: stage, Run: run}
	activity, err := d.GetActivity(q)
	if err != nil {
		p.Metadata.Omissions = append(p.Metadata.Omissions, t.ID+": recorded activity unavailable")
	} else if activity.Stale {
		p.Metadata.Omissions = append(p.Metadata.Omissions, t.ID+": activity omitted because its fallback may belong to another run")
	} else {
		data, _ := json.Marshal(activity)
		ev.Activity = companionClip(string(data), 6144)
		if len(data) > len(ev.Activity) {
			p.Metadata.Omissions = append(p.Metadata.Omissions, t.ID+": activity truncated")
		}
		p.Activities[t.ID] = companionEvidenceActivity{Query: q, Revision: companionActivityRevision(activity)}
	}
	p.Tickets = append(p.Tickets, ev)
}

func companionActivityRevision(activity web.ActivityInfo) string {
	if activity.Tape != nil && activity.ETag != "" {
		return activity.ETag
	}
	// Fingerprint plaintext bytes, not the shared log's path or modtime,
	// which can stay the same after a rewrite.
	data, _ := json.Marshal(activity)
	return web.ContentETag(data, "")
}

func companionAttentionRevision(items []companion.Item) string {
	type source struct {
		ID, Revision, Reason string
		Active               bool
	}
	values := make([]source, 0, len(items))
	for _, item := range items {
		values = append(values, source{ID: item.ID, Revision: item.SourceRevision, Reason: item.InactiveReason, Active: item.Active})
	}
	data, _ := json.Marshal(values)
	return web.ContentETag(data, "")
}

func (d *Daemon) companionEvidenceInfo(id string) []web.CompanionEvidenceInfo {
	if !strings.HasPrefix(id, "k") {
		return nil
	}
	store := d.assistantThreadStore(d.config(), id)
	dir, err := store.Dir(id)
	if err != nil {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(dir, "evidence-*.json"))
	sources := map[string]string{}
	for _, t := range d.companionTickets() {
		sources[t.ID] = t.Revision
	}
	out := make([]web.CompanionEvidenceInfo, 0, len(files))
	current := d.CompanionSnapshot()
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		var p companionEvidence
		if json.Unmarshal(data, &p) != nil {
			continue
		}
		meta := p.Metadata
		meta.Outdated = p.AttentionRevision != companionAttentionRevision(current.Items)
		for id, revision := range meta.Sources {
			if sources[id] != revision {
				meta.Outdated = true
			}
		}
		if !meta.Outdated {
			for _, a := range p.Activities {
				v, err := d.GetActivity(a.Query)
				if err != nil || companionActivityRevision(v) != a.Revision {
					meta.Outdated = true
					break
				}
			}
		}
		out = append(out, meta)
	}
	slices.SortFunc(out, func(a, b web.CompanionEvidenceInfo) int { return a.CapturedAt.Compare(b.CapturedAt) })
	return out
}

func saveCompanionEvidence(store *assistant.Store, id string, p companionEvidence) error {
	dir, err := store.Dir(id)
	if err != nil {
		return err
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if len(data) > companionEvidenceMax {
		return fmt.Errorf("evidence exceeds 32 KiB")
	}
	file, err := os.CreateTemp(dir, ".evidence-*")
	if err != nil {
		return err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if err = file.Chmod(0o600); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, "evidence-"+p.Metadata.ID+".json")); err != nil {
		return err
	}
	parent, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = parent.Sync()
	closeErr = parent.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func (d *Daemon) PostCompanionMessage(req web.CompanionMessageRequest) (web.AssistantThreadInfo, error) {
	if !d.config().Assistant.Companion.Enabled {
		return web.AssistantThreadInfo{}, fmt.Errorf("%w: companion observation is disabled", web.ErrInvalidState)
	}
	if strings.TrimSpace(req.Text) == "" || len(req.Text) > 32<<10 || len(req.TicketIDs) > 5 {
		return web.AssistantThreadInfo{}, fmt.Errorf("%w: text must be non-blank and at most 32 KiB; ticket_ids must contain no more than five IDs", web.ErrInvalidConfig)
	}
	switch req.Kind {
	case "briefing", "investigation", "followup":
	default:
		return web.AssistantThreadInfo{}, fmt.Errorf("%w: invalid companion request kind", web.ErrInvalidConfig)
	}
	if req.Kind == "investigation" && len(req.TicketIDs) == 0 {
		return web.AssistantThreadInfo{}, fmt.Errorf("%w: select tickets to investigate", web.ErrInvalidConfig)
	}
	if _, err := assistantAgent(d.config()); err != nil {
		return web.AssistantThreadInfo{}, err
	}
	// Keep implicit creation and rollback in the same lifecycle transaction as
	// admission, so another post or deletion cannot take over a failed chat.
	d.assistant.lifecycle.Lock()
	defer d.assistant.lifecycle.Unlock()
	id := req.ThreadID
	implicit := id == ""
	if implicit {
		info, err := d.createAssistantThreadLocked(web.CreateAssistantThreadRequest{Autonomy: config.AutonomyRead}, true)
		if err != nil {
			return web.AssistantThreadInfo{}, err
		}
		id = info.ID
	}
	if !strings.HasPrefix(id, "k") {
		return web.AssistantThreadInfo{}, fmt.Errorf("%w: use a companion-purpose chat", web.ErrInvalidConfig)
	}
	store := d.assistantThreadStore(d.config(), id)
	err := d.postAssistantMessagePreparedLocked(id, web.AssistantMessageRequest{Text: req.Text}, true, func(store *assistant.Store, thread assistant.Thread, prepared *web.AssistantMessageRequest) error {
		if !d.config().Assistant.Companion.Enabled {
			return fmt.Errorf("%w: companion observation is disabled", web.ErrInvalidState)
		}
		p, err := d.collectCompanionEvidence(req)
		if err != nil {
			return err
		}
		if err = saveCompanionEvidence(store, thread.ID, p); err != nil {
			return err
		}
		data, _ := json.Marshal(p)
		prepared.Context = string(data)
		return nil
	})
	if err != nil {
		if implicit {
			if cleanupErr := store.Delete(id); cleanupErr != nil {
				return web.AssistantThreadInfo{}, fmt.Errorf("%w; removing failed implicit chat: %w", err, cleanupErr)
			}
		}
		return web.AssistantThreadInfo{}, err
	}
	return d.GetAssistantThread(id)
}
