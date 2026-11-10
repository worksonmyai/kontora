package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/automation"
	"github.com/worksonmyai/kontora/internal/config"
	"gopkg.in/yaml.v3"
)

func TestAutomationConfigMutations(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		name := "regular file"
		if symlink {
			name = "symlink"
		}
		t.Run(name, func(t *testing.T) {
			h, d, original, _ := automationFixture(t)
			target := d.configPath
			before, err := os.ReadFile(target)
			require.NoError(t, err)
			before = append([]byte("# keep this comment\n"), before...)
			require.NoError(t, os.WriteFile(target, before, 0o640))
			if symlink {
				link := filepath.Join(t.TempDir(), "config-link.yaml")
				require.NoError(t, os.Symlink(target, link))
				d.configPath = link
			}
			input := original.Spec
			input.Name = "Weekly report"
			input.Prompt = "Read the changes.\nWrite a report.\n"
			input.Enabled = false
			created, err := d.CreateAutomation("create-weekly", "weekly-report", input)
			require.NoError(t, err)
			assert.Equal(t, input, created.Spec)
			assert.Equal(t, "weekly-report", created.Key)
			assert.NotEqual(t, original.ID, created.ID)
			saved, err := os.ReadFile(target)
			require.NoError(t, err)
			assert.Contains(t, string(saved), "# keep this comment")
			assert.Contains(t, string(saved), "# operator configuration")
			assert.Contains(t, string(saved), "prompt: |")
			info, err := os.Stat(target)
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o640), info.Mode().Perm())
			if symlink {
				info, err = os.Lstat(d.configPath)
				require.NoError(t, err)
				assert.NotZero(t, info.Mode()&os.ModeSymlink)
			}
			loaded, err := config.Load(d.configPath)
			require.NoError(t, err)
			assert.Equal(t, input, loaded.Automations["weekly-report"])
			assert.Equal(t, original.Spec, loaded.Automations[original.Key])
			replay, err := d.CreateAutomation("create-weekly", "weekly-report", input)
			require.NoError(t, err)
			assert.Equal(t, created, replay)
			unchanged, err := os.ReadFile(target)
			require.NoError(t, err)
			assert.Equal(t, saved, unchanged)
			changed := input
			changed.Prompt = "Changed"
			_, err = d.CreateAutomation("create-weekly", "weekly-report", changed)
			require.ErrorIs(t, err, automation.ErrConflict)
			_, err = d.CreateAutomation("another-request", "weekly-report", input)
			require.ErrorIs(t, err, automation.ErrConflict)
			updated, err := d.UpdateAutomation(created.ID, created.Revision, changed)
			require.NoError(t, err)
			assert.Greater(t, updated.Revision, created.Revision)
			_, err = d.UpdateAutomation(created.ID, created.Revision, input)
			require.ErrorIs(t, err, automation.ErrConflict)
			err = d.DeleteAutomation(created.ID, created.Revision)
			require.ErrorIs(t, err, automation.ErrConflict)
			require.NoError(t, d.DeleteAutomation(created.ID, updated.Revision))
			_, err = d.GetAutomation(created.ID)
			require.ErrorIs(t, err, automation.ErrNotFound)
			loaded, err = config.Load(d.configPath)
			require.NoError(t, err)
			assert.NotContains(t, loaded.Automations, "weekly-report")
			assert.Contains(t, loaded.Automations, original.Key)
			// A completed creation receipt remains a receipt after deletion.
			replay, err = d.CreateAutomation("create-weekly", "weekly-report", input)
			require.NoError(t, err)
			assert.Equal(t, created, replay)
			automationAssertNoTickets(t, h)
		})
	}
}

func TestAutomationConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*automation.Spec)
		key    string
	}{
		{"empty prompt", func(s *automation.Spec) { s.Prompt = "" }, "new-report"},
		{"subminute", func(s *automation.Spec) { s.Schedule = "30s" }, "new-report"},
		{"fractional minute", func(s *automation.Spec) { s.Schedule = "90s" }, "new-report"},
		{"unknown agent", func(s *automation.Spec) { s.Agent = "missing" }, "new-report"},
		{"invalid prompt", func(s *automation.Spec) { s.Prompt = "{{" }, "new-report"},
		{"unsupported model", func(s *automation.Spec) { s.Model = "model-override" }, "new-report"},
		{"invalid key", func(*automation.Spec) {}, "Not A Key"},
		{"invalid workspace", func(s *automation.Spec) { s.Workspace = "shared" }, "new-report"},
		{"invalid overlap", func(s *automation.Spec) { s.Overlap = "parallel" }, "new-report"},
		{"invalid timezone", func(s *automation.Spec) { s.Timezone = "Missing/Zone" }, "new-report"},
		{"invalid timeout", func(s *automation.Spec) { s.Timeout = "0s" }, "new-report"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, d, def, _ := automationFixture(t)
			before, err := os.ReadFile(d.configPath)
			require.NoError(t, err)
			input := def.Spec
			tc.mutate(&input)
			_, err = d.CreateAutomation("invalid", tc.key, input)
			require.Error(t, err)
			after, err := os.ReadFile(d.configPath)
			require.NoError(t, err)
			assert.Equal(t, before, after)
			rows, err := d.ListAutomations()
			require.NoError(t, err)
			assert.Len(t, rows, 1)
		})
	}
}

func TestAutomationConfigDefaultsAndCreateReceiptRestart(t *testing.T) {
	h, d, def, _ := automationFixture(t)
	input := automation.Spec{Name: "Nightly report", Prompt: "Report changes", Path: h.repoDir, Agent: "agent1", Schedule: "1m"}
	created, err := d.CreateAutomation("new-request", "", input)
	require.NoError(t, err)
	assert.Equal(t, "nightly-report", created.Key)
	assert.Equal(t, "UTC", created.Timezone)
	assert.Equal(t, "worktree", created.Workspace)
	assert.Equal(t, "skip", created.Overlap)
	assert.Equal(t, "30m", created.Timeout)
	assert.False(t, created.Enabled)
	automationClose(t, d)
	stop := d.startAutomations(h.tasksDir)
	defer stop()
	replay, err := d.CreateAutomation("new-request", "", input)
	require.NoError(t, err)
	assert.Equal(t, created, replay)
	rows, err := d.ListAutomations()
	require.NoError(t, err)
	assert.Len(t, rows, 2)
	current, err := d.GetAutomation(def.ID)
	require.NoError(t, err)
	assert.Equal(t, def.Revision, current.Revision)
}

func TestAutomationConditionalConfigWrites(t *testing.T) {
	for _, action := range []string{"update", "delete", "raw"} {
		t.Run(action, func(t *testing.T) {
			_, d, def, _ := automationFixture(t)
			before, err := os.ReadFile(d.configPath)
			require.NoError(t, err)
			external, err := automationYAML(before, def.Key, nil)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(d.configPath, external, 0o640))
			switch action {
			case "update":
				_, err = d.UpdateAutomation(def.ID, def.Revision, def.Spec)
			case "delete":
				err = d.DeleteAutomation(def.ID, def.Revision)
			case "raw":
				err = d.PutRawConfigIf(string(before), string(before))
			}
			require.ErrorIs(t, err, automation.ErrConflict)
			after, err := os.ReadFile(d.configPath)
			require.NoError(t, err)
			assert.Equal(t, external, after, "an external edit must not be overwritten")
			require.NoError(t, d.PutRawConfigIf(string(before), string(external)))
			_, err = d.GetAutomation(def.ID)
			require.NoError(t, err)
		})
	}
}

func TestAutomationExternalDeletion(t *testing.T) {
	h, d, def, _ := automationFixture(t)
	spec := def.Spec
	spec.Overlap = "queue"
	def, err := d.UpdateAutomation(def.ID, def.Revision, spec)
	require.NoError(t, err)
	first, err := d.RunAutomation(def.ID, "first")
	require.NoError(t, err)
	second, err := d.RunAutomation(def.ID, "second")
	require.NoError(t, err)
	before, err := os.ReadFile(d.configPath)
	require.NoError(t, err)
	after, err := automationYAML(before, def.Key, nil)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(d.configPath, after, 0o640))
	require.NoError(t, d.reloadConfig())
	rows, err := d.ListAutomations()
	require.NoError(t, err)
	assert.Empty(t, rows)
	_, err = d.GetAutomation(def.ID)
	require.ErrorIs(t, err, automation.ErrNotFound)
	second, err = d.automationStore.GetRun(def.ID, second.N)
	require.NoError(t, err)
	assert.Equal(t, "terminal", second.Phase)
	assert.Equal(t, "skipped", second.Outcome)
	assert.True(t, automationTakeJob(t, d).run(context.Background()))
	first, err = d.automationStore.GetRun(def.ID, first.N)
	require.NoError(t, err)
	assert.Equal(t, "terminal", first.Phase)
	_, err = d.RunAutomation(def.ID, "new")
	require.ErrorIs(t, err, automation.ErrNotFound)
	recreated, err := d.CreateAutomation("recreate", def.Key, spec)
	require.NoError(t, err)
	assert.NotEqual(t, def.ID, recreated.ID)
	next, err := d.RunAutomation(recreated.ID, "new")
	require.NoError(t, err)
	assert.Equal(t, uint64(3), next.N)
	automationAssertNoTickets(t, h)
}

func TestAutomationConfigSharedYAMLIsNotRewritten(t *testing.T) {
	for _, anchor := range []bool{false, true} {
		name := "alias"
		if anchor {
			name = "anchor"
		}
		t.Run(name, func(t *testing.T) {
			_, d, def, _ := automationFixture(t)
			before, err := os.ReadFile(d.configPath)
			require.NoError(t, err)
			var doc yaml.Node
			require.NoError(t, yaml.Unmarshal(before, &doc))
			root := doc.Content[0]
			for i := 0; i < len(root.Content); i += 2 {
				if root.Content[i].Value != "automations" {
					continue
				}
				entry := root.Content[i+1].Content[1]
				if anchor {
					entry.Anchor = "shared"
				} else {
					// A scalar alias inside the entry is valid config but unsafe to rewrite.
					entry.Content[1].Anchor = "shared-name"
					for j := 0; j < len(entry.Content); j += 2 {
						if entry.Content[j].Value == "prompt" {
							entry.Content[j+1] = &yaml.Node{Kind: yaml.AliasNode, Value: "shared-name", Alias: entry.Content[1]}
						}
					}
				}
			}
			shared, err := yaml.Marshal(&doc)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(d.configPath, shared, 0o640))
			require.NoError(t, d.reloadConfig())
			def, err = d.GetAutomation(def.ID)
			require.NoError(t, err)
			_, err = d.UpdateAutomation(def.ID, def.Revision, def.Spec)
			require.ErrorIs(t, err, automation.ErrValidation)
			current, err := os.ReadFile(d.configPath)
			require.NoError(t, err)
			assert.Equal(t, shared, current)
		})
	}
}

func TestAutomationConfigMergeRefused(t *testing.T) {
	for _, position := range []string{"before target", "after target"} {
		for _, action := range []string{"create", "update", "delete"} {
			t.Run(position+"/"+action, func(t *testing.T) {
				_, d, def, _ := automationFixture(t)
				before, err := os.ReadFile(d.configPath)
				require.NoError(t, err)
				var doc yaml.Node
				require.NoError(t, yaml.Unmarshal(before, &doc))
				inherited := def.Spec
				inherited.Prompt = "Inherited automation must not become active"
				var merged yaml.Node
				require.NoError(t, merged.Encode(map[string]automation.Spec{def.Key: inherited}))
				for i := 0; i < len(doc.Content[0].Content); i += 2 {
					if doc.Content[0].Content[i].Value != "automations" {
						continue
					}
					entries := doc.Content[0].Content[i+1]
					merge := []*yaml.Node{{Kind: yaml.ScalarNode, Tag: "!!merge", Value: "<<"}, &merged}
					if position == "before target" {
						entries.Content = append(merge, entries.Content...)
					} else {
						entries.Content = append(entries.Content, merge...)
					}
				}
				before, err = yaml.Marshal(&doc)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(d.configPath, before, 0o640))
				require.NoError(t, d.reloadConfig())
				current, err := d.GetAutomation(def.ID)
				require.NoError(t, err)
				require.Equal(t, def.Spec, current.Spec)
				switch action {
				case "create":
					_, err = d.CreateAutomation("merge-create", "new-report", def.Spec)
				case "update":
					_, err = d.UpdateAutomation(def.ID, def.Revision, def.Spec)
				case "delete":
					err = d.DeleteAutomation(def.ID, def.Revision)
				}
				require.ErrorIs(t, err, automation.ErrValidation)
				require.ErrorContains(t, err, "YAML merges")
				after, err := os.ReadFile(d.configPath)
				require.NoError(t, err)
				assert.Equal(t, before, after)
				current, err = d.GetAutomation(def.ID)
				require.NoError(t, err)
				assert.Equal(t, def, current)
			})
		}
	}
}

func TestAutomationConfigReconcileRevision(t *testing.T) {
	_, d, def, now := automationFixture(t)
	before, err := os.ReadFile(d.configPath)
	require.NoError(t, err)
	changed := def.Spec
	changed.Enabled = false
	changed.Prompt = "External edit"
	after, err := automationYAML(before, def.Key, &changed)
	require.NoError(t, err)
	require.NoError(t, d.PutRawConfigIf(string(after), string(before)))
	current, err := d.GetAutomation(def.ID)
	require.NoError(t, err)
	assert.Equal(t, changed, current.Spec)
	assert.Equal(t, def.Revision+1, current.Revision)
	require.NoError(t, d.automationStore.Reconcile(d.config().Automations, now.Add(time.Minute)))
	current, err = d.GetAutomation(def.ID)
	require.NoError(t, err)
	assert.Equal(t, def.Revision+1, current.Revision)
}
