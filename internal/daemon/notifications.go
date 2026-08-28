package daemon

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/notify"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

// Notifier is the seam the daemon sends ticket observations through. The real
// one is a *notify.Dispatcher; tests inject a recorder.
//
// Live reports whether anything is behind the seam. Building an Observation
// resolves the project and scans the ticket body for a title, on every daemon
// write, so the no-op is asked first rather than handed work to discard.
type Notifier interface {
	Live() bool
	Observe(obs notify.Observation)
	Waiting(id string, want, channels []string, f notify.Fields)
	Forget(id string)
}

// noopNotifier is what d.notifier holds until Run builds a dispatcher, and for
// the whole run when no channel is configured. writeTicketFile and
// applyWaitMarker are hot paths, so the seam is never nil and no call site
// carries a check.
type noopNotifier struct{}

func (noopNotifier) Live() bool                                        { return false }
func (noopNotifier) Observe(notify.Observation)                        {}
func (noopNotifier) Waiting(string, []string, []string, notify.Fields) {}
func (noopNotifier) Forget(string)                                     {}

// liveNotifier adds Live to the dispatcher, which has no reason to carry a
// method that is always true.
type liveNotifier struct{ *notify.Dispatcher }

func (liveNotifier) Live() bool { return true }

// notifyTarget is where one ticket's notifications go and what they render
// from. Both the status path and the waiting path need all three.
type notifyTarget struct {
	want     []string
	channels []string
	fields   notify.Fields
}

func (d *Daemon) notifyTarget(t *ticket.Ticket) notifyTarget {
	cfg := d.config()
	repoPath := config.ExpandTilde(t.Path)
	project, _, _ := cfg.ProjectFor(repoPath)
	return notifyTarget{
		want:     t.Notify.Statuses,
		channels: cfg.NotifyChannelsFor(repoPath, t.NotifyChannels),
		fields: notify.Fields{
			Title:    t.Title(),
			Stage:    t.Stage,
			Branch:   t.Branch,
			RepoPath: repoPath,
			Project:  project,
			// summary, not final_summary: this fires from the write that ends
			// the run, and final_summary is written up to two minutes later by
			// a separate agent pass. Moving this to storeFinalSummary would
			// lose every notification for a ticket with too few recorded runs,
			// and every one whose ticket a stage_end hook paused.
			Summary:   t.Summary,
			LastError: t.LastError,
		},
	}
}

// observe reports one look at a ticket to the notifier, resolving the channels
// against the ticket's project first. A ticket that names no status is still
// reported: the dispatcher has to remember the status either way, or the next
// transition would diff against a status nobody recorded.
func (d *Daemon) observe(t *ticket.Ticket, origin notify.Origin) {
	if !d.notifier.Live() {
		return
	}
	target := d.notifyTarget(t)
	d.notifier.Observe(notify.Observation{
		Origin:   origin,
		ID:       t.ID,
		Status:   string(t.Status),
		Want:     target.want,
		Channels: target.channels,
		Fields:   target.fields,
	})
}

// notifyWarning is one thing wrong with a ticket's notify fields.
type notifyWarning struct {
	msg  string
	args []any
}

// warnUnmatchedNotifyLocked reports a notify: entry no status can ever equal, a
// notify_channels: entry no channel answers to, and a ticket that asks for a
// notification but resolves to nowhere to send it. All three are silent at
// delivery time, which is the worst way to learn that a ticket has been quiet.
//
// It runs on every observation, not only at startup: a ticket written after the
// daemon started would otherwise run to completion, send nothing and log
// nothing until the next restart. What each ticket was last warned about is
// remembered, so re-reading an unchanged ticket says nothing twice. Caller
// holds d.mu.
func (d *Daemon) warnUnmatchedNotifyLocked(cfg *config.Config, t *ticket.Ticket) {
	if !d.notifier.Live() {
		return
	}
	warnings := unmatchedNotify(cfg, t)
	if len(warnings) == 0 {
		delete(d.notifyWarned, t.ID)
		return
	}
	key := fmt.Sprint(warnings)
	if d.notifyWarned[t.ID] == key {
		return
	}
	d.notifyWarned[t.ID] = key
	log := d.ticketLog(t.ID)
	for _, w := range warnings {
		log.Warn(w.msg, w.args...)
	}
}

func unmatchedNotify(cfg *config.Config, t *ticket.Ticket) []notifyWarning {
	if len(t.Notify.Statuses) == 0 && len(t.NotifyChannels) == 0 {
		return nil
	}
	var out []notifyWarning
	for _, s := range t.Notify.Statuses {
		if s != notify.StatusWaiting && !cfg.IsKnownStatus(s) {
			out = append(out, notifyWarning{"notify names a status nothing reaches", []any{"status", s}})
		}
	}
	silenced := slices.Contains(t.NotifyChannels, config.NoneSentinel)
	for _, name := range t.NotifyChannels {
		if name == config.NoneSentinel {
			continue
		}
		if _, ok := cfg.Notifications.Channels[name]; !ok {
			out = append(out, notifyWarning{"notify_channels names a channel that is not configured", []any{"channel", name}})
		}
	}
	// The likeliest way to configure this feature and hear nothing: a channel
	// exists, the ticket asks for a status, and neither notifications.default
	// nor the ticket's project routes it anywhere.
	if !silenced && len(t.Notify.Statuses) > 0 &&
		len(cfg.NotifyChannelsFor(config.ExpandTilde(t.Path), t.NotifyChannels)) == 0 {
		out = append(out, notifyWarning{
			"notify names statuses but the ticket resolves to no channel",
			[]any{"hint", "set notifications.default, the project's notify_channels, or the ticket's"},
		})
	}
	return out
}

// validateNotifyFields refuses the two notify mistakes a request can make that
// nothing downstream would ever report: a status no ticket reaches, and a
// channel name nothing answers to. unmatchedNotify warns about the same two on
// a ticket already on disk, because a file the daemon merely read is not its to
// reject; a request is, and answering 400 is the only chance the caller gets to
// hear about a typo before the ticket runs silently to the end.
//
// A nil list is a request that said nothing about the field. The third case
// unmatchedNotify warns about — statuses that resolve to no channel — stays a
// warning: it depends on config the caller may be about to write.
func validateNotifyFields(cfg *config.Config, statuses, channels []string) error {
	for _, s := range statuses {
		if s != notify.StatusWaiting && !cfg.IsKnownStatus(s) {
			return fmt.Errorf("%w: no ticket ever reaches status %q", web.ErrInvalidNotify, s)
		}
	}
	for _, name := range channels {
		if name == config.NoneSentinel {
			// "none" silences the ticket. Beside a channel name it is the
			// contradiction the config validator refuses in its own lists.
			if len(channels) > 1 {
				return fmt.Errorf("%w: %q silences the list and cannot be combined with a channel", web.ErrInvalidNotify, config.NoneSentinel)
			}
			continue
		}
		if _, ok := cfg.Notifications.Channels[name]; !ok {
			return fmt.Errorf("%w: no channel named %q is configured", web.ErrInvalidNotify, name)
		}
	}
	return nil
}

// forgetNotifyLocked drops everything remembered about a ticket that is gone.
// Both delete paths call it: the API removes the entry itself, so the watcher
// event that follows finds nothing left to match. Caller holds d.mu.
func (d *Daemon) forgetNotifyLocked(id string) {
	d.notifier.Forget(id)
	delete(d.notifyWarned, id)
	delete(d.waitAnnounced, id)
}

// startNotifications installs the dispatcher the config describes and returns
// its worker loop, or nil when there is nothing to run. A channel whose
// credential will not resolve warns and is dropped, keeping the rest; no
// channel at all leaves the no-op in place.
//
// Installing and running are separate because they belong at different points
// of Run: d.notifier has to be in place before the initial scan seeds it and
// before the web server serves a request that reads it, while the worker needs
// the cancellable context that only exists further down.
func (d *Daemon) startNotifications(cfg *config.Config) func(context.Context) {
	if d.notifierPinned {
		return nil
	}
	if cfg.Notifications.Enabled != nil && !*cfg.Notifications.Enabled {
		d.log.Info("notifications disabled by config")
		return nil
	}
	if len(cfg.Notifications.Channels) == 0 {
		return nil
	}

	var channels []notify.Channel
	for _, name := range slices.Sorted(maps.Keys(cfg.Notifications.Channels)) {
		ch, err := buildNotifyChannel(name, cfg.Notifications.Channels[name])
		if err != nil {
			d.log.Warn("notification channel dropped", "channel", name, "err", err)
			continue
		}
		channels = append(channels, ch)
		d.log.Info("notification channel ready",
			"channel", name, "type", cfg.Notifications.Channels[name].Type,
			"secret_source", cfg.Notifications.Channels[name].SecretSource())
	}
	if len(channels) == 0 {
		d.log.Warn("notifications configured but no channel could be built, continuing without them")
		return nil
	}

	var attempts int
	if a := cfg.Notifications.Attempts; a != nil {
		attempts = *a
	}
	dispatcher := notify.New(notify.Options{
		Channels: channels,
		Attempts: attempts,
		Backoff:  cfg.Notifications.Backoff.Duration,
		Timeout:  cfg.Notifications.Timeout.Duration,
		Log:      d.log,
		OnResult: func(channel, result string) {
			d.metrics.Notification(context.Background(), channel, result)
		},
	})
	d.notifier = liveNotifier{dispatcher}
	// Observe never blocks and the queue is buffered, so anything observed
	// between here and the worker starting waits in the queue rather than being
	// lost.
	return dispatcher.Run
}

func buildNotifyChannel(name string, c config.NotifyChannel) (notify.Channel, error) {
	secret, err := c.ResolveSecret()
	if err != nil {
		return nil, err
	}
	switch c.Type {
	case config.NotifyTelegram:
		return &notify.Telegram{ChannelName: name, Token: secret, ChatID: c.ChatID}, nil
	case config.NotifyMattermost:
		return &notify.Mattermost{ChannelName: name, URL: secret, Channel: c.Channel}, nil
	case config.NotifyWebhook:
		return &notify.Webhook{
			ChannelName: name,
			URL:         c.URL,
			Method:      strings.ToUpper(cmp.Or(c.Method, http.MethodPost)),
			Headers:     c.Headers,
			Token:       secret,
		}, nil
	default:
		return nil, fmt.Errorf("unknown type %q", c.Type)
	}
}
