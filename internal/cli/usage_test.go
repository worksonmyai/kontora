package cli

import (
	"bytes"
	"flag"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWriteUsage(t *testing.T) {
	cases := []struct {
		name    string
		noFlags bool
		want    []string
		notWant []string
	}{
		{
			name:    "fmt",
			noFlags: true,
			want:    []string{"Usage: kontora fmt\n\nFormat stream-json from stdin\n\nReference: kontora skills show cli \"kontora fmt\"\n"},
			notWant: []string{"[flags]", "Flags:"},
		},
		{
			name: "dep",
			want: []string{
				"Usage: kontora dep [flags] TICKET_ID DEPENDENCY_ID\n\nMake a ticket wait on another one\n",
				"\nFlags:\n  -config string\n",
				"\nReference: kontora skills show cli \"kontora dep\"\n",
			},
			notWant: []string{"Commands:"},
		},
		{
			name: "new",
			want: []string{"Usage: kontora new [flags] TITLE\n"},
		},
		{
			name: "release",
			want: []string{"Usage: kontora release [flags] [TICKET_ID]\n"},
		},
		{
			name: "config",
			want: []string{"Usage: kontora config [flags]\n", "\nCommands:\n  edit       Edit configuration\n"},
		},
		{
			name: "config edit",
			want: []string{"Usage: kontora config edit [flags]\n\nEdit configuration\n", "Reference: kontora skills show cli \"kontora config\"\n"},
		},
		{
			name:    "not-a-verb",
			want:    []string{"Usage of not-a-verb:\n  -config string\n"},
			notWant: []string{"Reference:"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			fs := flag.NewFlagSet(tc.name, flag.ContinueOnError)
			fs.SetOutput(&out)
			if !tc.noFlags {
				fs.String("config", "", "path to config file")
			}
			WriteUsage(fs)
			for _, want := range tc.want {
				assert.Contains(t, out.String(), want)
			}
			for _, notWant := range tc.notWant {
				assert.NotContains(t, out.String(), notWant)
			}
		})
	}
}
