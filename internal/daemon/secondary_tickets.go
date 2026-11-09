package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/ticket/store"
	"github.com/worksonmyai/kontora/internal/watcher"
	"github.com/worksonmyai/kontora/internal/web"
)

type secondaryCatalog struct {
	source      web.TicketSourceInfo
	tickets     map[string]*ticketState
	projections map[string][sha256.Size]byte
	observer    secondaryObserver
}

type secondaryGeneration struct {
	done    chan struct{}
	tickets map[string]web.TicketInfo
	err     error
}

func (g *secondaryGeneration) result() (map[string]web.TicketInfo, error) {
	<-g.done
	return g.tickets, g.err
}

type secondaryObserver struct {
	mu      sync.Mutex
	pending *secondaryGeneration
	idle    chan struct{}
	observe func() (map[string]web.TicketInfo, error)
}

func (o *secondaryObserver) request() *secondaryGeneration {
	o.mu.Lock()
	defer o.mu.Unlock()
	// An active scan may already have read a changed path; join its successor.
	if o.pending == nil {
		o.pending = &secondaryGeneration{done: make(chan struct{})}
	}
	g := o.pending
	if o.idle == nil {
		o.idle = make(chan struct{})
		go o.run()
	}
	return g
}

func (o *secondaryObserver) run() {
	for {
		o.mu.Lock()
		g := o.pending
		if g == nil {
			close(o.idle)
			o.idle = nil
			o.mu.Unlock()
			return
		}
		o.pending = nil
		o.mu.Unlock()
		g.tickets, g.err = o.observe()
		close(g.done)
	}
}

func (o *secondaryObserver) wait() {
	o.mu.Lock()
	idle := o.idle
	o.mu.Unlock()
	if idle != nil {
		<-idle
	}
}

type secondaryObservation struct {
	stored      *app.StoredTicket
	modTime     time.Time
	revision    string
	revisionErr error
	reactions   ticket.NoteReactions
}

type secondarySnapshot struct {
	tickets      map[string]*secondaryObservation
	blocks       map[string][]string
	children     map[string][]string
	related      map[string][]string
	admissionErr error
}

// Read each document and sidecar once under the lock used by cooperating edits
// and Claim. Hash those bytes, not a later disk read that may describe another edit.
func observeSecondary(dir string, read func(string) ([]byte, os.FileInfo, error)) (*secondarySnapshot, error) {
	if _, err := os.ReadDir(dir); err != nil {
		return nil, err
	}
	snapshot := &secondarySnapshot{
		tickets: make(map[string]*secondaryObservation), blocks: make(map[string][]string),
		children: make(map[string][]string), related: make(map[string][]string),
	}
	err := ticket.WithBoardMutation(dir, func() error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		snapshot.admissionErr = ticket.CheckBoardControl(dir)
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			md, stat, err := read(path)
			if err != nil {
				if !errors.Is(err, os.ErrNotExist) && snapshot.admissionErr == nil {
					snapshot.admissionErr = err
				}
				continue
			}
			t, err := ticket.ParseBytes(md)
			if err != nil {
				continue
			}
			if t.HasOwnershipMetadata() && snapshot.admissionErr == nil {
				snapshot.admissionErr = fmt.Errorf("%w: %s", ticket.ErrManaged, t.ID)
			}
			if !ticket.IsSafeID(t.ID) || !ticket.IsCanonicalPath(path, t.ID) {
				continue
			}
			observed := &secondaryObservation{stored: &app.StoredTicket{Ticket: t, FilePath: path}, modTime: stat.ModTime()}
			reactions, _, err := read(filepath.Join(dir, t.ID+".notes.json"))
			exists := !errors.Is(err, os.ErrNotExist)
			if err != nil && exists {
				observed.revisionErr = err
			} else {
				observed.revision = secondaryRevision(md, reactions, exists)
				if exists {
					if err := json.Unmarshal(reactions, &observed.reactions); err != nil {
						observed.revisionErr = err
					}
				}
			}
			snapshot.tickets[t.ID] = observed
			deps := slices.Clone(t.Deps)
			slices.Sort(deps)
			for _, id := range slices.Compact(deps) {
				snapshot.blocks[id] = append(snapshot.blocks[id], t.ID)
				snapshot.related[id] = append(snapshot.related[id], t.ID)
			}
			for _, id := range t.Links {
				snapshot.related[id] = append(snapshot.related[id], t.ID)
			}
			if t.Parent != "" {
				snapshot.children[t.Parent] = append(snapshot.children[t.Parent], t.ID)
				snapshot.related[t.Parent] = append(snapshot.related[t.Parent], t.ID)
			}
		}
		return nil
	})
	return snapshot, err
}

func readSecondaryFile(path string) ([]byte, os.FileInfo, error) {
	stat, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !stat.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("unsafe nonregular ticket file %s", path)
	}
	// Recheck the opened inode; a replacement between Lstat and Open must not
	// follow a symlink or wait on a FIFO.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	stat, err = f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !stat.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("unsafe nonregular ticket file %s", path)
	}
	data, err := io.ReadAll(f)
	return data, stat, err
}

// Keep the wire revision identical to store.Revision, including sidecar absence.
func secondaryRevision(md, reactions []byte, exists bool) string {
	digest := func(data []byte) string {
		sum := sha256.Sum256(data)
		return hex.EncodeToString(sum[:])
	}
	return digest([]byte(fmt.Sprintf("%s:%t:%s", digest(md), exists, digest(reactions))))
}

func (d *Daemon) isTransferBlocked(id string) bool {
	d.mu.Lock()
	blocked := d.transferBlocked[id]
	d.mu.Unlock()
	if blocked {
		return true
	}
	pending, err := store.HasPendingTransfer(expandTilde(d.config().TicketsDir), id)
	if err != nil {
		d.log.Error("checking pending transfer", "ticket", id, "err", err)
		return true
	}
	return pending
}

func (d *Daemon) startSecondary(ctx context.Context) (func(), error) {
	sources, err := d.config().SecondarySources()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	d.secondaryMu.Lock()
	d.secondary = make(map[string]*secondaryCatalog, len(sources))
	for _, source := range sources {
		catalog := &secondaryCatalog{source: web.TicketSourceInfo{ID: source.ID, Path: source.Path}, tickets: make(map[string]*ticketState)}
		catalog.observer.observe = func() (map[string]web.TicketInfo, error) {
			return d.observeAndPublishSecondary(source.ID, source.Path)
		}
		d.secondary[source.ID] = catalog
	}
	d.secondaryMu.Unlock()
	for _, source := range sources {
		w, identity := d.openSecondaryWatcher(source.ID, source.Path)
		wg.Go(func() { d.watchSecondary(ctx, source.ID, source.Path, w, identity) })
	}
	return func() {
		cancel()
		wg.Wait()
		for _, source := range sources {
			d.secondary[source.ID].observer.wait()
		}
	}, nil
}

func (d *Daemon) openSecondaryWatcher(id, dir string) (*watcher.Watcher, os.FileInfo) {
	identity, err := os.Stat(dir)
	if err != nil {
		d.setSecondaryUnavailable(id, err)
		return nil, nil
	}
	w, err := watcher.New(dir, d.debounce, func(path string) bool {
		return filepath.Clean(path) == filepath.Clean(dir) || watcher.MarkdownFilter(path) || strings.HasSuffix(path, ".notes.json")
	})
	if err != nil {
		d.setSecondaryUnavailable(id, err)
		return nil, nil
	}
	current, err := os.Stat(dir)
	if err != nil || !os.SameFile(identity, current) {
		_ = w.Close()
		if err == nil {
			err = fmt.Errorf("source directory changed while attaching watcher")
		}
		d.setSecondaryUnavailable(id, err)
		return nil, nil
	}
	d.refreshSecondary(id)
	return w, identity
}

func (d *Daemon) watchSecondary(ctx context.Context, id, dir string, w *watcher.Watcher, identity os.FileInfo) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	defer func() {
		if w != nil {
			_ = w.Close()
		}
	}()
	for {
		var events <-chan watcher.Event
		var errs <-chan error
		if w != nil {
			events, errs = w.Events(), w.Errors()
		}
		select {
		case <-ctx.Done():
			return
		case event := <-events:
			if filepath.Clean(event.Path) == filepath.Clean(dir) {
				_ = w.Close()
				w, identity = d.openSecondaryWatcher(id, dir)
			} else {
				d.requestSecondaryObservation(id)
			}
		case err := <-errs:
			d.setSecondaryUnavailable(id, err)
			_ = w.Close()
			w = nil
		case <-ticker.C:
			if w == nil {
				w, identity = d.openSecondaryWatcher(id, dir)
				continue
			}
			current, err := os.Stat(dir)
			if err == nil {
				_, err = os.ReadDir(dir)
			}
			if err != nil {
				d.setSecondaryUnavailable(id, err)
				_ = w.Close()
				w = nil
			} else if !os.SameFile(identity, current) {
				_ = w.Close()
				w, identity = d.openSecondaryWatcher(id, dir)
			}
		}
	}
}

func (d *Daemon) setSecondaryUnavailable(id string, err error) {
	d.secondaryMu.Lock()
	changed := false
	if catalog := d.secondary[id]; catalog != nil {
		changed = catalog.source.Available || catalog.source.Error != err.Error()
		catalog.source.Available = false
		catalog.source.Error = err.Error()
	}
	d.secondaryMu.Unlock()
	if changed {
		d.reconcileSecondaryDependencies()
		d.broker.Broadcast(web.TicketEvent{Type: "ticket_sources_changed"})
	}
}

func (d *Daemon) requestSecondaryObservation(id string) *secondaryGeneration {
	d.secondaryMu.Lock()
	catalog := d.secondary[id]
	d.secondaryMu.Unlock()
	if catalog == nil {
		g := &secondaryGeneration{done: make(chan struct{}), err: web.ErrTicketNotFound}
		close(g.done)
		return g
	}
	return catalog.observer.request()
}

func (d *Daemon) refreshSecondary(id string) {
	_, _ = d.requestSecondaryObservation(id).result()
}

func (d *Daemon) observeAndPublishSecondary(id, dir string) (map[string]web.TicketInfo, error) {
	snapshot, err := observeSecondary(dir, readSecondaryFile)
	if err != nil {
		d.setSecondaryUnavailable(id, err)
		return nil, err
	}
	cfg := *d.config()
	cfg.TicketsDir = dir
	next := make(map[string]*ticketState, len(snapshot.tickets))
	infos := make(map[string]web.TicketInfo, len(snapshot.tickets))
	projections := make(map[string][sha256.Size]byte, len(snapshot.tickets))
	for ticketID, observed := range snapshot.tickets {
		next[ticketID] = newTicketState(observed.stored.Ticket, observed.stored.FilePath)
		info := d.secondaryTicketInfo(&cfg, id, observed, snapshot)
		data, err := json.Marshal(info)
		if err != nil {
			return nil, err
		}
		infos[ticketID], projections[ticketID] = info, sha256.Sum256(data)
	}
	d.secondaryMu.Lock()
	catalog := d.secondary[id]
	if catalog == nil {
		d.secondaryMu.Unlock()
		return nil, web.ErrTicketNotFound
	}
	old := catalog.projections
	wasAvailable := catalog.source.Available
	catalog.tickets, catalog.projections = next, projections
	catalog.source.Available, catalog.source.Error = true, ""
	d.secondaryMu.Unlock()
	d.reconcileSecondaryDependencies()
	if !wasAvailable {
		d.broker.Broadcast(web.TicketEvent{Type: "ticket_sources_changed"})
	}
	for ticketID, info := range infos {
		if previous, ok := old[ticketID]; ok && previous == projections[ticketID] {
			continue
		}
		// Compare the full projection, including bodies, before trimming SSE.
		info.Body, info.Description, info.DescriptionTail = "", nil, nil
		d.broker.Broadcast(web.TicketEvent{Type: "secondary_ticket_updated", Ticket: info})
	}
	for ticketID := range old {
		if next[ticketID] == nil {
			info := web.TicketInfo{Source: id, ID: ticketID}
			d.broker.Broadcast(web.TicketEvent{Type: "secondary_ticket_deleted", Ticket: info})
		}
	}
	return infos, nil
}

func (d *Daemon) ListTicketSources() []web.TicketSourceInfo {
	d.secondaryMu.Lock()
	defer d.secondaryMu.Unlock()
	result := make([]web.TicketSourceInfo, 0, len(d.secondary))
	for _, catalog := range d.secondary {
		result = append(result, catalog.source)
	}
	slices.SortFunc(result, func(a, b web.TicketSourceInfo) int { return strings.Compare(a.Path, b.Path) })
	return result
}

func (d *Daemon) secondaryConfig(source string) (*config.Config, error) {
	d.secondaryMu.Lock()
	catalog := d.secondary[source]
	if catalog == nil {
		d.secondaryMu.Unlock()
		return nil, web.ErrTicketNotFound
	}
	info := catalog.source
	d.secondaryMu.Unlock()
	if !info.Available {
		return nil, fmt.Errorf("%w: source unavailable: %s", web.ErrInvalidState, info.Error)
	}
	cfg := *d.config()
	cfg.TicketsDir = info.Path
	return &cfg, nil
}

func (d *Daemon) ListSecondaryTickets(source string) ([]web.TicketInfo, error) {
	infos, err := d.requestSecondaryObservation(source).result()
	if err != nil {
		return nil, err
	}
	result := make([]web.TicketInfo, 0, len(infos))
	for _, info := range infos {
		info.Body, info.Description, info.DescriptionTail = "", nil, nil
		info.History, info.Notes, info.Blocks, info.Children = nil, nil, nil, nil
		info.LastError, info.LastLog, info.Summary, info.FinalSummary = "", "", "", ""
		result = append(result, info)
	}
	slices.SortFunc(result, func(a, b web.TicketInfo) int { return strings.Compare(a.ID, b.ID) })
	return result, nil
}

func (d *Daemon) GetSecondaryTicket(source, id string) (web.TicketInfo, error) {
	if !ticket.IsSafeID(id) {
		return web.TicketInfo{}, web.ErrTicketNotFound
	}
	return d.secondaryMutationResult(source, id)
}

func (d *Daemon) secondaryMutationResult(source, id string) (web.TicketInfo, error) {
	infos, err := d.requestSecondaryObservation(source).result()
	if err != nil {
		return web.TicketInfo{}, err
	}
	info, ok := infos[id]
	if !ok {
		return web.TicketInfo{}, web.ErrTicketNotFound
	}
	return info, nil
}

func secondaryStored(dir, id string) (*app.StoredTicket, error) {
	if !ticket.IsSafeID(id) {
		return nil, web.ErrTicketNotFound
	}
	path := filepath.Join(dir, id+".md")
	data, _, err := readSecondaryFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, web.ErrTicketNotFound
	}
	if err != nil {
		return nil, err
	}
	t, err := ticket.ParseBytes(data)
	if err != nil {
		return nil, err
	}
	if !ticket.IsCanonicalPath(path, t.ID) {
		return nil, web.ErrTicketNotFound
	}
	return &app.StoredTicket{Ticket: t, FilePath: path}, nil
}

func (d *Daemon) secondaryTicketInfo(cfg *config.Config, source string, observed *secondaryObservation, snapshot *secondarySnapshot) web.TicketInfo {
	t := observed.stored.Ticket
	info := web.TicketInfoFromView(app.BuildView(cfg, t, true))
	info.Source = source
	info.Project, _, _ = cfg.ProjectFor(t.Path)
	info.UpdatedAt = &observed.modTime
	info.Revision = observed.revision
	if observed.revisionErr != nil {
		info.RefusalReason = observed.revisionErr.Error()
	}
	info.CanEdit = snapshot.admissionErr == nil && observed.revisionErr == nil
	if snapshot.admissionErr != nil {
		info.RefusalReason = snapshot.admissionErr.Error()
	}
	if info.RefusalReason == "" {
		if reason := secondaryClaimRefusal(t); reason != nil {
			info.RefusalReason = reason.Error()
		}
	}
	if info.RefusalReason == "" {
		if err := d.claimDestinationAllowed(t.ID); err != nil {
			info.RefusalReason = err.Error()
		}
	}
	info.CanClaim = info.RefusalReason == ""
	info.CanControl, info.CanAnnotate = false, false
	for i := range info.Notes {
		info.Notes[i].AuthorKind = authorKind(cfg, info.Notes[i].Author)
		for _, chip := range observed.reactions.For(info.Notes[i].ID) {
			info.Notes[i].Reactions = append(info.Notes[i].Reactions, web.ReactionInfo{Emoji: chip.Emoji, Actors: chip.Actors})
		}
	}
	index := snapshot.tickets
	info.EpicChild = index[t.Parent] != nil && index[t.Parent].stored.Ticket.Kind == ticket.KindEpic
	resolve := func(ref *web.TicketRef) {
		if ref != nil {
			if other := index[ref.ID]; other != nil {
				ref.Title, ref.Status = other.stored.Ticket.Title(), string(other.stored.Ticket.Status)
			}
		}
	}
	for i := range info.Deps {
		resolve(&info.Deps[i])
	}
	for i := range info.Links {
		resolve(&info.Links[i])
	}
	resolve(info.Parent)
	for _, id := range snapshot.blocks[t.ID] {
		other := index[id].stored.Ticket
		info.Blocks = append(info.Blocks, web.TicketRef{ID: other.ID, Title: other.Title(), Status: string(other.Status)})
	}
	for _, id := range snapshot.children[t.ID] {
		info.Children = append(info.Children, childInfo(cfg, index[id].stored.Ticket))
	}
	slices.SortFunc(info.Blocks, func(a, b web.TicketRef) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(info.Children, func(a, b web.TicketChild) int { return strings.Compare(a.ID, b.ID) })
	info.ClaimWarnings = d.claimRelationshipWarnings(t, snapshot.related[t.ID])
	return info
}

func secondaryClaimRefusal(t *ticket.Ticket) error {
	switch {
	case t.Status == ticket.StatusInProgress:
		return fmt.Errorf("ticket is in_progress")
	case t.InvocationID != "":
		return fmt.Errorf("ticket has an invocation marker")
	case t.AnnotationReturnStatus != "":
		return fmt.Errorf("ticket has pending annotations")
	case t.HasOwnershipMetadata():
		return ticket.ErrManaged
	default:
		return nil
	}
}

func (d *Daemon) claimDestinationAllowed(id string) error {
	cfg := d.config()
	for _, path := range []string{filepath.Join(expandTilde(cfg.TicketsDir), id+".md"), filepath.Join(expandTilde(cfg.TicketsDir), id+".notes.json"), filepath.Join(expandTilde(cfg.LogsDir), id)} {
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("destination already has ticket data for %s", id)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.tickets[id] != nil || d.executions[id] != nil || d.running[id] != nil || d.queued[id] || d.transferBlocked[id] {
		return fmt.Errorf("primary ticket identity %s is reserved", id)
	}
	if _, ok := d.recoveredReservations[id]; ok {
		return fmt.Errorf("primary execution %s is reserved", id)
	}
	return nil
}

func (d *Daemon) claimRelationshipWarnings(t *ticket.Ticket, incoming []string) []string {
	refs := append(slices.Clone(t.Deps), t.Links...)
	refs = append(refs, t.Parent)
	refs = append(refs, t.Children...)
	refs = append(refs, incoming...)
	slices.Sort(refs)
	refs = slices.Compact(refs)
	var warnings []string
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, id := range refs {
		if id == "" {
			continue
		}
		if d.tickets[id] == nil {
			warnings = append(warnings, fmt.Sprintf("%s is missing in primary. Claim does not move related tickets.", id))
		} else {
			warnings = append(warnings, fmt.Sprintf("%s will resolve to the primary folder's ticket", id))
		}
	}
	return warnings
}

func (d *Daemon) UpdateSecondaryTicket(source, id string, req web.UpdateTicketRequest) (web.TicketInfo, error) {
	if err := req.ValidateDescription(); err != nil {
		return web.TicketInfo{}, err
	}
	cfg, err := d.secondaryConfig(source)
	if err != nil {
		return web.TicketInfo{}, err
	}
	repo := store.NewSecondaryRepo(cfg.TicketsDir)
	svc := app.New(app.Static(cfg), repo, app.NoopRuntime{})
	var prepared *app.PreparedExecutionSettings
	if req.Path != nil || req.Branch != nil || req.BaseBranch != nil {
		proposal, err := secondaryStored(cfg.TicketsDir, id)
		if err != nil {
			return web.TicketInfo{}, err
		}
		if err := applyTicketUpdate(cfg, proposal.Ticket, req); err != nil {
			return web.TicketInfo{}, err
		}
		prepared, err = svc.PrepareExecutionSettings(proposal.Ticket)
		if err != nil {
			return web.TicketInfo{}, mapAppError(err)
		}
	}
	err = repo.WithMutation(func(scoped app.MutationRepository) error {
		st, err := scoped.Get(id)
		if err != nil {
			return err
		}
		if err := applyTicketUpdate(cfg, st.Ticket, req); err != nil {
			return err
		}
		if prepared != nil {
			if err := prepared.Validate(scoped, st.Ticket); err != nil {
				return err
			}
		}
		return scoped.Save(st)
	})
	if err != nil {
		return web.TicketInfo{}, mapAppError(err)
	}
	return d.secondaryMutationResult(source, id)
}

func (d *Daemon) MutateSecondaryTicket(source, id string, op web.SecondaryMutation) (web.TicketInfo, error) {
	cfg, err := d.secondaryConfig(source)
	if err != nil {
		return web.TicketInfo{}, err
	}
	if _, err := secondaryStored(cfg.TicketsDir, id); err != nil {
		return web.TicketInfo{}, err
	}
	// Routing admission spans both markdown and reaction writes.
	err = ticket.WithOrdinaryRouting(cfg.TicketsDir, func() error {
		repo := store.NewSecondaryRepo(cfg.TicketsDir)
		svc := app.New(app.Static(cfg), repo, app.NoopRuntime{})
		switch op.Action {
		case web.SecondaryAddDependency, web.SecondaryRemoveDependency:
			if len(op.RelatedIDs) != 1 {
				return fmt.Errorf("one dependency is required")
			}
			if op.Action == web.SecondaryAddDependency {
				_, err = svc.AddDependency(id, op.RelatedIDs[0])
			} else {
				_, err = svc.RemoveDependency(id, op.RelatedIDs[0])
			}
		case web.SecondaryLink:
			_, err = svc.Link(id, op.RelatedIDs...)
		case web.SecondaryUnlink:
			_, err = svc.Unlink(id, op.RelatedIDs...)
		case web.SecondarySetParent:
			_, err = svc.SetParent(id, op.Parent)
		case web.SecondaryClearParent:
			_, err = svc.ClearParent(id)
		case web.SecondarySetChildOrder:
			_, err = svc.SetChildOrder(id, op.Children)
		default:
			err = repo.WithMutation(func(scoped app.MutationRepository) error {
				st, getErr := scoped.Get(id)
				if getErr != nil {
					return getErr
				}
				t := st.Ticket
				switch op.Action {
				case web.SecondaryAddNote:
					author := op.Author
					if author == "" {
						author = cfg.Author
					}
					_, err = t.AddNote(ticket.AddNoteOptions{Text: op.Text, Author: author, ParentID: op.Parent, At: time.Now()})
				case web.SecondaryEditNote:
					_, err = t.EditNote(op.NoteID, op.Text)
				case web.SecondaryDeleteNote:
					err = t.DeleteNote(op.NoteID)
				case web.SecondarySetSummary:
					err = t.SetField("summary", op.Text)
				case web.SecondarySetReaction:
					op.NoteID, _, err = t.EnsureNoteID(op.NoteID)
				default:
					return fmt.Errorf("%w: secondary lifecycle operations are not allowed", web.ErrInvalidState)
				}
				if err != nil {
					return err
				}
				return scoped.Save(st)
			})
			if err == nil && op.Action == web.SecondarySetReaction {
				actor := op.Actor
				if actor == "" {
					actor = cfg.Author
				}
				err = ticket.SetNoteReaction(cfg.TicketsDir, id, op.NoteID, op.Emoji, actor, op.On)
			}
		}
		return err
	})
	if err != nil {
		return web.TicketInfo{}, mapAppError(err)
	}
	return d.secondaryMutationResult(source, id)
}

func (d *Daemon) ClaimSecondaryTicket(source, id, revision string) (web.TicketInfo, error) {
	cfg, err := d.secondaryConfig(source)
	if err != nil {
		return web.TicketInfo{}, err
	}
	reserved := false
	t, err := store.Transfer(store.TransferRequest{SourceDir: cfg.TicketsDir, DestinationDir: expandTilde(d.config().TicketsDir), ID: id, Revision: revision, Validate: func(_ *ticket.Ticket) error {
		if err := d.claimDestinationAllowed(id); err != nil {
			return err
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.transferBlocked == nil {
			d.transferBlocked = make(map[string]bool)
		}
		d.transferBlocked[id] = true
		reserved = true
		return nil
	}})
	if err != nil {
		if reserved {
			blocked, recoveryErr := store.RecoverTransfers(expandTilde(d.config().TicketsDir))
			if recoveryErr != nil && len(blocked) == 0 {
				blocked[id] = true
			}
			d.mu.Lock()
			if !blocked[id] {
				delete(d.transferBlocked, id)
			}
			d.mu.Unlock()
			if recoveryErr != nil {
				d.log.Error("recovering failed Claim", "ticket", id, "err", recoveryErr)
			}
			if !blocked[id] {
				d.handleFileChanged(filepath.Join(expandTilde(d.config().TicketsDir), id+".md"))
				d.refreshSecondary(source)
			}
		}
		return web.TicketInfo{}, fmt.Errorf("%w: %v", web.ErrInvalidState, err)
	}
	path := filepath.Join(expandTilde(d.config().TicketsDir), id+".md")
	d.mu.Lock()
	delete(d.transferBlocked, id)
	d.tickets[id] = newTicketState(t, path)
	if t.IsPassiveEpic() {
		d.rederiveEpicLocked(id, "")
	}
	d.broadcastTicketUpdate(id)
	info := d.buildTicketInfo(d.config(), d.tickets[id], true)
	d.mu.Unlock()
	d.refreshSecondary(source)
	return info, nil
}
