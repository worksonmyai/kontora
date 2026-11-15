package config

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestNotifyDefaultFor(t *testing.T) {
	for _, tc := range []struct {
		name    string
		project OptionalStatusList
		global  OptionalStatusList
		want    []string
	}{
		{name: "unset"},
		{name: "global", global: OptionalStatusList{"paused", "human_review", "waiting"}, want: []string{"paused", "human_review", "waiting"}},
		{name: "global off", global: OptionalStatusList{}, want: []string{}},
		{name: "project wins", project: OptionalStatusList{"done"}, global: OptionalStatusList{"paused"}, want: []string{"done"}},
		{name: "project off", project: OptionalStatusList{}, global: OptionalStatusList{"paused"}, want: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{
				Projects:      map[string]Project{"work": {Path: "/repos/work", Notify: tc.project}},
				Notifications: Notifications{DefaultStatuses: tc.global},
			}
			got := cfg.NotifyDefaultFor("/repos/work/")
			assert.Equal(t, tc.want, got)
			if len(got) > 0 {
				got[0] = "changed"
				assert.Equal(t, tc.want, cfg.NotifyDefaultFor("/repos/work"))
			}
			assert.Equal(t, []string(tc.global), cfg.NotifyDefaultFor("/repos/unlisted"))
			got = cfg.NotifyDefaultFor("/repos/unlisted")
			if len(got) > 0 {
				got[0] = "changed"
				assert.Equal(t, []string(tc.global), cfg.NotifyDefaultFor("/repos/unlisted"))
			}
		})
	}
}

func TestLoadNotificationDefaultStatuses(t *testing.T) {
	for _, tc := range []struct {
		name     string
		statuses string
		wantErr  string
	}{
		{name: "known and waiting without channels", statuses: "[done, deployed, waiting]"},
		{name: "off without channels", statuses: "[]"},
		{name: "unknown", statuses: "[unknown_status]", wantErr: `no ticket ever reaches status "unknown_status"`},
		{name: "none even declared custom", statuses: "[none]", wantErr: `"none" cannot be used in notification status lists; use [] to disable subscriptions`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := fmt.Sprintf("agents:\n  claude:\n    binary: claude\nstatuses: [deployed, none]\nnotifications:\n  enabled: false\n  default_statuses: %s\n", tc.statuses)
			cfg, err := LoadReader(strings.NewReader(src))
			if tc.wantErr != "" {
				require.ErrorContains(t, err, "notifications.default_statuses")
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Empty(t, cfg.Notifications.Channels)
			assert.Empty(t, cfg.Notifications.Default)
			assert.False(t, *cfg.Notifications.Enabled)
		})
	}
}

func TestOptionalStatusDefaultsYAMLRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  OptionalStatusList
	}{
		{name: "missing"},
		{name: "null", value: "null"},
		{name: "blank", value: " "},
		{name: "off", value: "[]", want: OptionalStatusList{}},
		{name: "statuses", value: "[done, waiting]", want: OptionalStatusList{"done", "waiting"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := "agents:\n  claude:\n    binary: claude\nprojects:\n  work:\n    path: /repos/work\n"
			if tc.value != "" {
				src += "    notify: " + tc.value + "\nnotifications:\n  default_statuses: " + tc.value + "\n"
			}
			cfg, err := LoadReader(strings.NewReader(src))
			require.NoError(t, err)
			assert.Equal(t, tc.want, cfg.Notifications.DefaultStatuses)
			assert.Equal(t, tc.want, cfg.Projects["work"].Notify)
			out, err := yaml.Marshal(cfg)
			require.NoError(t, err)
			if tc.want == nil {
				assert.NotContains(t, string(out), "default_statuses:")
				assert.NotContains(t, string(out), "notify:")
			} else if len(tc.want) == 0 {
				assert.Contains(t, string(out), "default_statuses: []")
				assert.Contains(t, string(out), "notify: []")
			}
			reloaded, err := LoadReader(strings.NewReader(string(out)))
			require.NoError(t, err)
			assert.Equal(t, tc.want, reloaded.Notifications.DefaultStatuses)
			assert.Equal(t, tc.want, reloaded.Projects["work"].Notify)
		})
	}
}
