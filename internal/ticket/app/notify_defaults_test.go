package app

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func TestApplyNotifyDefaults(t *testing.T) {
	for _, tc := range []struct {
		name, field            string
		defaults, global, want []string
		changed                bool
	}{
		{name: "unset", defaults: []string{"paused", "waiting"}, want: []string{"paused", "waiting"}, changed: true},
		{name: "null", field: "notify: null\n", defaults: []string{"done"}, want: []string{"done"}, changed: true},
		{name: "anchored null", field: "notify: &choice null\ncustom: *choice\n", defaults: []string{"done"}, want: []string{"done"}, changed: true},
		{name: "anchored blank", field: "notify: &choice\ncustom: *choice\n", defaults: []string{}, want: []string{}, changed: true},
		{name: "aliased null", field: "custom: &choice null\nnotify: *choice\n", defaults: []string{"done"}, want: []string{"done"}, changed: true},
		{name: "merged anchored null", field: "<<: {notify: &choice null}\ncustom: *choice\n", defaults: []string{"done"}, want: []string{"done"}, changed: true},
		{name: "blank", field: "notify:\n", defaults: []string{"done"}, want: []string{"done"}, changed: true},
		{name: "no default"},
		{name: "global fallback", global: []string{"waiting"}, want: []string{"waiting"}, changed: true},
		{name: "project wins", defaults: []string{"done"}, global: []string{"waiting"}, want: []string{"done"}, changed: true},
		{name: "project Off wins", defaults: []string{}, global: []string{"waiting"}, want: []string{}, changed: true},
		{name: "explicit default Off", defaults: []string{}, want: []string{}, changed: true},
		{name: "saved Off", field: "notify: []\n", defaults: []string{"done"}, want: []string{}},
		{name: "saved statuses", field: "notify: [human_review]\n", defaults: []string{"done"}, want: []string{"human_review"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testCfg()
			cfg.Notifications.DefaultStatuses = tc.global
			cfg.Projects = map[string]config.Project{"repo": {Path: "/repo", Notify: tc.defaults}}
			tk, err := ticket.ParseBytes([]byte("---\nid: tst-001\npath: /repo\nnotify_channels: [none]\n" + tc.field + "---\n# Test\n"))
			require.NoError(t, err)
			changed, err := ApplyNotifyDefaults(cfg, tk)
			require.NoError(t, err)
			assert.Equal(t, tc.changed, changed)
			assert.Equal(t, tc.want, tk.Notify.Statuses)
			data, err := tk.Marshal()
			require.NoError(t, err)
			got, err := ticket.ParseBytes(data)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.Notify.Statuses)
			assert.Equal(t, []string{"none"}, got.NotifyChannels)
			changed, err = ApplyNotifyDefaults(cfg, tk)
			require.NoError(t, err)
			assert.False(t, changed)
		})
	}
}

func TestSetNotifyFieldsPreservesIntent(t *testing.T) {
	for _, tc := range []struct {
		name                                 string
		field                                string
		notify, channels, want, wantChannels []string
	}{
		{name: "Off clears route", field: "notify: [done]\nnotify_channels: [ops]\n", notify: []string{}, channels: []string{}, want: []string{}},
		{name: "channel-only keeps unset", channels: []string{"ops"}, wantChannels: []string{"ops"}},
		{name: "channel-only keeps Off", field: "notify: []\n", channels: []string{"ops"}, want: []string{}, wantChannels: []string{"ops"}},
		{name: "omitted keeps statuses", field: "notify: [done]\nnotify_channels: [ops]\n", channels: []string{}, want: []string{"done"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tk, err := ticket.ParseBytes([]byte("---\nid: tst-001\n" + tc.field + "---\n# Test\n"))
			require.NoError(t, err)
			require.NoError(t, SetNotifyFields(tk, tc.notify, tc.channels))
			data, err := tk.Marshal()
			require.NoError(t, err)
			got, err := ticket.ParseBytes(data)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.Notify.Statuses)
			assert.Equal(t, tc.wantChannels, got.NotifyChannels)
			if tc.channels != nil && len(tc.channels) == 0 {
				assert.NotContains(t, string(data), "notify_channels:")
			}
		})
	}
}

func TestExecutionNotifyDefaults(t *testing.T) {
	for _, boundary := range []struct {
		name, status, stage string
		apply               bool
		run                 func(*Service) (Result, error)
	}{
		{"init", "open", "code", true, func(s *Service) (Result, error) {
			return s.Init("tst-001", InitRequest{Path: "/final", Pipeline: "default"})
		}},
		{"init open", "open", "code", true, func(s *Service) (Result, error) {
			return s.Init("tst-001", InitRequest{Path: "/final", Pipeline: "default", Status: "open"})
		}},
		{"status todo", "open", "code", true, func(s *Service) (Result, error) { return s.SetStatus("tst-001", ticket.StatusTodo) }},
		{"retry", "paused", "code", true, func(s *Service) (Result, error) { return s.Retry("tst-001") }},
		{"advancing skip", "paused", "code", true, func(s *Service) (Result, error) { return s.Skip("tst-001") }},
		{"final skip", "paused", "review", false, func(s *Service) (Result, error) { return s.Skip("tst-001") }},
		{"set stage", "paused", "code", false, func(s *Service) (Result, error) { return s.SetStage("tst-001", "review") }},
		{"status done", "open", "code", false, func(s *Service) (Result, error) { return s.SetStatus("tst-001", ticket.StatusDone) }},
	} {
		for _, choice := range []struct {
			name, field string
			want        []string
		}{
			{"unset", "", nil},
			{"Off", "notify: []\n", []string{}},
			{"saved", "notify: [human_review]\n", []string{"human_review"}},
		} {
			t.Run(boundary.name+"/"+choice.name, func(t *testing.T) {
				repo := newMemRepo()
				repo.add("tst-001", fmt.Sprintf("---\nid: tst-001\nkontora: true\npath: /original\nstatus: %s\npipeline: default\nstage: %s\n%s---\n# Test\n", boundary.status, boundary.stage, choice.field))
				cfg := testCfg()
				cfg.Projects = map[string]config.Project{
					"original": {Path: "/original", Notify: []string{"paused"}},
					"final":    {Path: "/final", Notify: []string{"done"}},
				}
				svc := New(Static(cfg), repo, &spyRuntime{})
				_, err := boundary.run(svc)
				require.NoError(t, err)
				got, err := repo.Get("tst-001")
				require.NoError(t, err)
				want := choice.want
				if boundary.apply && choice.field == "" {
					want = cfg.NotifyDefaultFor(got.Ticket.Path)
				}
				assert.Equal(t, want, got.Ticket.Notify.Statuses)
				if boundary.name == "init" || boundary.name == "init open" {
					assert.Equal(t, "/final", got.Ticket.Path)
				}
				assert.Equal(t, []string{"tst-001"}, repo.saved)
			})
		}
	}
}

func TestInitExplicitNotifyOverridesDefaults(t *testing.T) {
	for _, choice := range [][]string{nil, {}, {"waiting"}} {
		t.Run(fmt.Sprintf("%#v", choice), func(t *testing.T) {
			repo := newMemRepo()
			repo.add("tst-001", "---\nid: tst-001\nstatus: open\npath: /repo\nnotify: [done]\n---\n# Test\n")
			cfg := testCfg()
			cfg.Projects = map[string]config.Project{"repo": {Path: "/repo", Notify: []string{"paused"}}}
			_, err := New(Static(cfg), repo, &spyRuntime{}).Init("tst-001", InitRequest{Notify: choice})
			require.NoError(t, err)
			want := choice
			if want == nil {
				want = []string{"done"}
			}
			assert.Equal(t, want, repo.tickets["tst-001"].Ticket.Notify.Statuses)
		})
	}
}
