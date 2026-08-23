package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The registry and the embedded directory have to agree in both directions: a
// topic with no file cannot be shown, and a file with no topic is unreachable.
func TestSkillTopicsMatchFiles(t *testing.T) {
	entries, err := skillDocs.ReadDir("skills")
	require.NoError(t, err)

	files := map[string]bool{}
	for _, e := range entries {
		files["skills/"+e.Name()] = true
	}

	for _, topic := range skillTopics {
		t.Run(topic.Name, func(t *testing.T) {
			assert.True(t, files[topic.File], "no embedded file for topic")
			assert.NotEmpty(t, topic.Desc)
			doc, err := skillDoc(topic.Name)
			require.NoError(t, err)
			assert.NotEmpty(t, skillSections(doc), "topic has no ## sections")
			delete(files, topic.File)
		})
	}
	assert.Empty(t, files, "embedded files no topic names")
}

func TestSkills(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		want    []string
		notWant []string
		wantErr []string
	}{
		{
			name: "no arguments lists the topics",
			want: []string{"cli", "tickets", "pipelines", "config"},
		},
		{
			name: "list names every topic",
			args: []string{"list"},
			want: []string{"cli", "tickets", "pipelines", "config"},
		},
		{
			name:    "list of one topic prints its headings only",
			args:    []string{"list", "cli"},
			want:    []string{"kontora new", "kontora skills"},
			notWant: []string{"--description-file"},
		},
		{
			name: "show prints the whole topic",
			args: []string{"show", "tickets"},
			want: []string{"# Tickets", "## Status lifecycle", "## Relations"},
		},
		{
			name:    "show of a section prints that section only",
			args:    []string{"show", "cli", "kontora new"},
			want:    []string{"## kontora new", "--description-file"},
			notWant: []string{"## kontora view"},
		},
		{
			name:    "a section matches on a unique substring",
			args:    []string{"show", "cli", "new"},
			want:    []string{"## kontora new"},
			notWant: []string{"## kontora view"},
		},
		{
			name: "a section matches case-insensitively",
			args: []string{"show", "cli", "KONTORA NEW"},
			want: []string{"## kontora new"},
		},
		{
			name:    "the section arguments are joined",
			args:    []string{"show", "cli", "kontora", "set-stage"},
			want:    []string{"## kontora set-stage"},
			notWant: []string{"## kontora skip"},
		},
		{
			name:    "an ambiguous section names the candidates",
			args:    []string{"show", "cli", "kontora s"},
			wantErr: []string{"matches", "kontora search", "kontora skip"},
		},
		{
			name:    "a section that matches nothing says so",
			args:    []string{"show", "cli", "nowhere"},
			wantErr: []string{`no section matching "nowhere"`},
		},
		{
			name:    "an unknown topic lists the topics",
			args:    []string{"show", "nope"},
			wantErr: []string{`unknown topic "nope"`, "cli", "tickets", "pipelines", "config"},
		},
		{
			name:    "show with no topic lists the topics",
			args:    []string{"show"},
			wantErr: []string{"needs a topic", "pipelines"},
		},
		{
			name:    "an unknown subcommand is rejected",
			args:    []string{"install"},
			wantErr: []string{`unknown subcommand "install"`},
		},
		{
			name: "-h lists the topics, like every other verb prints usage",
			args: []string{"-h"},
			want: []string{"cli", "tickets", "pipelines", "config"},
		},
		{
			name: "--help lists the topics",
			args: []string{"--help"},
			want: []string{"cli", "tickets", "pipelines", "config"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := Skills(&buf, tc.args)
			if len(tc.wantErr) > 0 {
				require.Error(t, err)
				for _, want := range tc.wantErr {
					assert.Contains(t, err.Error(), want)
				}
				assert.Empty(t, buf.String())
				return
			}
			require.NoError(t, err)
			for _, want := range tc.want {
				assert.Contains(t, buf.String(), want)
			}
			for _, notWant := range tc.notWant {
				assert.NotContains(t, buf.String(), notWant)
			}
		})
	}
}

// A heading inside a fenced block is text: the topics quote YAML whose comments
// start with a hash, and a config example holding one must not split the
// section it is in.
func TestSkillSections(t *testing.T) {
	doc := strings.Join([]string{
		"# Title",
		"intro",
		"## first",
		"one",
		"```yaml",
		"## not a heading",
		"```",
		"two",
		"### nested",
		"three",
	}, "\n")

	sections := skillSections(doc)
	require.Len(t, sections, 2)
	assert.Equal(t, "first", sections[0].title)
	assert.Contains(t, sections[0].body, "## not a heading")
	assert.NotContains(t, sections[0].body, "three")
	assert.Equal(t, "nested", sections[1].title)
	assert.Contains(t, sections[1].body, "three")
}
