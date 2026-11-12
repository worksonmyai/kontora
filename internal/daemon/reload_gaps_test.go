package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/automation"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/notify"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestReload_SaveRawConfig(t *testing.T) {
	for _, tc := range []struct {
		name        string
		symlink     bool
		relative    bool
		chained     bool
		deleted     bool
		wantErr     bool
		conditional bool
	}{
		{name: "ordinary"},
		{name: "conditional ordinary", conditional: true},
		{name: "conditional symlink", symlink: true, conditional: true},
		{name: "deleted ordinary", deleted: true},
		{name: "symlink", symlink: true},
		{name: "relative symlink", symlink: true, relative: true},
		{name: "chained symlink", symlink: true, chained: true},
		{name: "dangling symlink", symlink: true, deleted: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newReloadHarness(t)
			d := h.newDaemonWithConfig(t, h.yaml(configOpts{}))
			target := h.configPath
			var linkTarget string
			if tc.symlink {
				linkTarget = target
				if tc.relative {
					linkTarget = filepath.Base(target)
				}
				if tc.chained {
					linkTarget = filepath.Join(filepath.Dir(target), "intermediate.yaml")
					require.NoError(t, os.Symlink(target, linkTarget))
				}
				d.configPath = filepath.Join(filepath.Dir(target), "link.yaml")
				require.NoError(t, os.Symlink(linkTarget, d.configPath))
			}
			if tc.deleted {
				require.NoError(t, os.Remove(target))
			}

			before := d.config()
			updated := h.yaml(configOpts{branchPrefix: "updated"})
			var err error
			if tc.conditional {
				err = d.PutRawConfigIf(updated, h.yaml(configOpts{}))
			} else {
				err = d.PutRawConfig(updated)
			}
			if tc.wantErr {
				require.ErrorIs(t, err, os.ErrNotExist)
				assert.NoFileExists(t, target)
				assert.Same(t, before, d.config())
			} else {
				require.NoError(t, err)
				data, err := os.ReadFile(target)
				require.NoError(t, err)
				assert.Equal(t, updated, string(data))
				assert.Equal(t, "updated", d.config().BranchPrefix)
			}
			if tc.symlink {
				got, err := os.Readlink(d.configPath)
				require.NoError(t, err)
				assert.Equal(t, linkTarget, got)
				if tc.chained {
					got, err := os.Readlink(linkTarget)
					require.NoError(t, err)
					assert.Equal(t, target, got)
				}
			}
		})
	}
}

func TestReload_SaveRawConfig_ReloadFailure(t *testing.T) {
	for _, mode := range []string{"ordinary", "conditional", "deleted ordinary", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			h := newReloadHarness(t)
			original := h.yaml(configOpts{})
			d := h.newDaemonWithConfig(t, original)
			before := d.config()
			if mode == "deleted ordinary" {
				require.NoError(t, os.Remove(h.configPath))
			}
			if mode == "symlink" {
				d.configPath = filepath.Join(filepath.Dir(h.configPath), "link.yaml")
				require.NoError(t, os.Symlink(h.configPath, d.configPath))
			}
			secondary := t.TempDir()
			updated := h.yaml(configOpts{branchPrefix: "updated", extra: fmt.Sprintf("secondary_tickets_dirs: [%q, %q]\n", secondary, secondary)})
			candidate, err := config.LoadReader(strings.NewReader(updated))
			require.NoError(t, err, "document validation must succeed before writing")
			_, reloadErr := candidate.SecondarySources()
			require.Error(t, reloadErr)

			if mode == "conditional" {
				err = d.PutRawConfigIf(updated, original)
			} else {
				err = d.PutRawConfig(updated)
			}
			require.EqualError(t, err, "config saved but reload failed: "+reloadErr.Error())
			var savedErr *web.ConfigSavedReloadError
			require.ErrorAs(t, err, &savedErr)
			assert.EqualError(t, savedErr.Err, reloadErr.Error())
			assert.ErrorIs(t, err, savedErr.Err)
			onDisk, readErr := os.ReadFile(h.configPath)
			require.NoError(t, readErr)
			assert.Equal(t, updated, string(onDisk))
			assert.Same(t, before, d.config(), "reload failure must not publish the saved configuration")
			assert.Equal(t, "kontora", d.config().BranchPrefix)
			if mode == "symlink" {
				target, readErr := os.Readlink(d.configPath)
				require.NoError(t, readErr)
				assert.Equal(t, h.configPath, target)
			}
		})
	}
}

func TestReload_SaveRawConfig_PreWriteFailure(t *testing.T) {
	for _, tc := range []struct {
		name        string
		conditional bool
		deleted     bool
		stale       bool
		want        error
	}{
		{name: "invalid ordinary", want: web.ErrInvalidConfig},
		{name: "invalid conditional", conditional: true, want: web.ErrInvalidConfig},
		{name: "invalid deleted ordinary", deleted: true, want: web.ErrInvalidConfig},
		{name: "stale conditional", conditional: true, stale: true, want: automation.ErrConflict},
		{name: "deleted conditional", conditional: true, deleted: true, want: os.ErrNotExist},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newReloadHarness(t)
			original := h.yaml(configOpts{})
			d := h.newDaemonWithConfig(t, original)
			before := d.config()
			if tc.deleted {
				require.NoError(t, os.Remove(h.configPath))
			}
			content, expected := "unknown_field: true\n", original
			if tc.stale {
				content = h.yaml(configOpts{branchPrefix: "updated"})
				expected = "stale"
			}
			var err error
			if tc.conditional {
				err = d.PutRawConfigIf(content, expected)
			} else {
				err = d.PutRawConfig(content)
			}
			require.ErrorIs(t, err, tc.want)
			var savedErr *web.ConfigSavedReloadError
			assert.False(t, errors.As(err, &savedErr), "pre-write failure must not claim persistence")
			if tc.deleted {
				assert.NoFileExists(t, h.configPath)
			} else {
				onDisk, readErr := os.ReadFile(h.configPath)
				require.NoError(t, readErr)
				assert.Equal(t, original, string(onDisk))
			}
			assert.Same(t, before, d.config())
		})
	}
}

func TestReload_KeepsPendingReview(t *testing.T) {
	h := newReloadHarness(t)
	reviews := t.TempDir()
	d := h.newDaemonWithConfig(t, h.yaml(configOpts{plannotator: "plannotator:\n  reviews_dir: " + reviews + "\n"}))
	const id = "tst-review"
	require.NoError(t, os.WriteFile(filepath.Join(reviews, id+".md"), []byte("Fix the missing check."), 0o644))
	h.writeConfig(t, h.yaml(configOpts{plannotator: "plannotator:\n  reviews_dir: " + t.TempDir() + "\n"}))
	require.NoError(t, d.reloadConfig())
	assert.Equal(t, reviews, d.config().Plannotator.ReviewsDir)
	rendered, err := d.renderTicketPrompt(d.config(), "{{ plannotatorReview }}", &ticket.Ticket{ID: id}, filepath.Join(h.tasksDir, id+".md"), h.repoDir)
	require.NoError(t, err)
	assert.Contains(t, rendered, "Fix the missing check.")
	assert.Contains(t, h.logBuf.String(), "field=plannotator.reviews_dir")
}

func TestReload_EnablesAutoPickUp(t *testing.T) {
	h := newReloadHarness(t)
	d := h.newDaemonWithConfig(t, h.yaml(configOpts{extra: "auto_pick_up: false\n"}))
	h.writeTicket("tst-apreload.md", h.taskMD("tst-apreload", "todo", "one-stage"))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-errCh) })
	require.Eventually(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		_, exists := d.tickets["tst-apreload"]
		return exists
	}, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, ticket.StatusTodo, h.readTask("tst-apreload.md").Status)
	h.writeConfigAtomic(t, h.yaml(configOpts{extra: "auto_pick_up: true\n"}))
	require.NoError(t, d.reloadConfig())
	h.waitForStatus("tst-apreload.md", ticket.StatusDone, 5*time.Second)
}

func TestReload_NotificationChannels(t *testing.T) {
	for _, mode := range []string{"switch", "enable", "disable", "failed secret"} {
		t.Run(mode, func(t *testing.T) {
			h := newReloadHarness(t)
			a, requestsA := notifyWireServer(t, http.StatusOK, "ok")
			b, requestsB := notifyWireServer(t, http.StatusOK, "ok")
			content := func(url string, enabled bool, secretEnv string) string {
				extra := fmt.Sprintf("notifications:\n  enabled: %t\n  channels:\n    hook:\n      type: webhook\n      url: %q\n      secret_env: %q\n  default: [hook]\n", enabled, url, secretEnv)
				return h.yaml(configOpts{extra: extra, branchPrefix: "updated"})
			}
			initial := strings.Replace(content(a.URL, mode != "enable", ""), "branch_prefix: updated", "branch_prefix: initial", 1)
			d := h.newDaemonWithConfig(t, initial)
			runNotificationBuilder(t, d, d.config())
			tkt := &ticket.Ticket{ID: "reload-notify", Status: ticket.StatusTodo,
				Notify: ticket.NotifyList{Statuses: []string{"done"}}}
			d.observe(tkt, notify.OriginObserved)
			secretEnv := ""
			if mode == "failed secret" {
				secretEnv = "KONTORA_TEST_RELOAD_MISSING_SECRET"
				t.Setenv(secretEnv, "")
			}
			h.writeConfig(t, content(b.URL, mode != "disable", secretEnv))
			require.NoError(t, d.reloadConfig())
			require.NoError(t, d.reloadConfig())
			tkt.Status = ticket.StatusDone
			d.observe(tkt, notify.OriginDaemon)
			if mode == "disable" || mode == "failed secret" {
				assert.False(t, d.notifier.Live())
				assert.Never(t, func() bool { return len(requestsA())+len(requestsB()) > 0 }, 100*time.Millisecond, time.Millisecond)
			} else {
				require.Eventually(t, func() bool { return len(requestsB()) == 1 }, 2*time.Second, time.Millisecond)
				assert.Empty(t, requestsA())
				assert.Never(t, func() bool { return len(requestsB()) > 1 }, 50*time.Millisecond, time.Millisecond)
			}
			assert.Equal(t, "updated", d.config().BranchPrefix)
			if mode == "failed secret" {
				assert.Contains(t, h.logBuf.String(), "channel=hook")
				assert.Contains(t, h.logBuf.String(), "notification channel dropped")
			}
			assert.NotContains(t, h.logBuf.String(), "field=notifications.")
		})
	}
}
