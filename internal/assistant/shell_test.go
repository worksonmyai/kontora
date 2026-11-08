package assistant

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
)

func TestClassifyShell(t *testing.T) {
	tests := []struct {
		name, command string
		want          Decision
	}{
		{"single quotes", `'kontora' 'delete' kon-1 -f`, DecisionDelete},
		{"double quotes", `"kontora" "delete" kon-1 -f`, DecisionDelete},
		{"joined quotes", `kon'to'"ra" de'le'"te" kon-1 -f`, DecisionDelete},
		{"escaped letters", `kon\tora de\lete kon-1 -f`, DecisionDelete},
		{"line continuation", "kon\\\ntora de\\\nlete kon-1 -f", DecisionDelete},
		{"quoted absolute path", `"/usr/local/bin/kontora" delete kon-1 -f`, DecisionDelete},
		{"quoted read", `'kontora' "view" 'kon-1'`, DecisionRead},
		{"quoted write", `'kontora' "note" kon-1 'hello; | && > $(x)'`, DecisionWrite},
		{"double quoted literal dollar", `kontora note kon-1 "\$(not executed)"`, DecisionWrite},
		{"double quotes preserve ordinary escapes", `kontora "de\lete" kon-1`, DecisionUnknown},
		{"ANSI quoting is unsupported", `$'kontora' $'delete' kon-1`, DecisionUnknown},
		{"locale quoting is unsupported", `$"kontora" ls`, DecisionUnknown},
		{"output", `kontora ls > out`, DecisionWrite},
		{"append", `kontora ls >> out`, DecisionWrite},
		{"clobber", `kontora ls >| out`, DecisionWrite},
		{"stderr", `kontora ls 2> out`, DecisionWrite},
		{"both outputs", `kontora ls &> out`, DecisionWrite},
		{"append both outputs", `kontora ls &>> out`, DecisionWrite},
		{"descriptor duplication", `kontora ls 2>&1`, DecisionWrite},
		{"input", `kontora view kon-1 < input`, DecisionRead},
		{"read write redirect", `kontora ls <> out`, DecisionWrite},
		{"input write command", `kontora note kon-1 < input`, DecisionWrite},
		{"redirect only", `> out`, DecisionWrite},
		{"quoted redirect path", `kontora ls > 'out $(literal)'`, DecisionWrite},
		{"dynamic redirect path", `kontora ls > "$out"`, DecisionUnknown},
		{"allocated descriptor", `kontora ls {fd}>out`, DecisionUnknown},
		{"delete output", `kontora delete kon-1 -f > out`, DecisionDelete},
		{"delete append", `kontora delete kon-1 -f >> out`, DecisionDelete},
		{"delete input", `kontora delete kon-1 -f < input`, DecisionDelete},
		{"delete dynamic target", `kontora delete "$id" -f > "$out"`, DecisionDelete},
		{"redirect before delete", `> out kontora delete kon-1 -f`, DecisionDelete},
		{"delete descriptor duplication", `kontora delete kon-1 -f 2>&1`, DecisionDelete},
		{"pipeline", `kontora ls | kontora stats`, DecisionRead},
		{"pipeline delete", `kontora ls | kontora delete kon-1 -f | kontora note kon-2`, DecisionDelete},
		{"and list", `kontora ls && kontora delete kon-1 -f && kontora note kon-2 hi`, DecisionDelete},
		{"or list", `kontora ls || kontora delete kon-1 -f`, DecisionDelete},
		{"background", `kontora ls & kontora delete kon-1 -f`, DecisionDelete},
		{"newline", "kontora ls\nkontora delete kon-1 -f", DecisionDelete},
		{"block", `{ kontora delete kon-1 -f; } > out`, DecisionDelete},
		{"subshell", `(kontora delete kon-1 -f) > out`, DecisionDelete},
		{"comment is not executed", "kontora ls # kontora delete kon-1 -f", DecisionRead},
		{"comment ends at newline", "kontora version # comment\nkontora delete kon-1 -f", DecisionDelete},
		{"comment backslash newline", "kontora version # \\\nkontora delete kon-1 -f", DecisionUnknown},
		{"comment backslash newline before write", "kontora version # \\\nkontora note kon-1 hello", DecisionUnknown},
		{"comment ends with two backslashes", "kontora version # \\\\\nkontora delete kon-1 -f", DecisionDelete},
		{"comment in command substitution", "kontora version $(kontora version # \\\nkontora delete kon-1 -f\n)", DecisionUnknown},
		{"known deletion beats ambiguous comment", "kontora delete kon-1 -f; kontora version # \\\nkontora ls", DecisionDelete},
		{"carriage return before comment marker", "kontora version \r#; kontora delete kon-1 -f", DecisionUnknown},
		{"backslash CRLF", "kontora version \\\r\nkontora delete kon-1 -f", DecisionUnknown},
		{"CRLF between commands", "kontora version\r\nkontora delete kon-1 -f", DecisionDelete},
		{"carriage return in read argument", "kontora view 'kon-1\r'", DecisionUnknown},
		{"quoted hash and line continuation", "kontora view '#' \\\nkon-1", DecisionRead},
		{"quoted comment-like text", "kontora note kon-1 '# \\\ntext'", DecisionWrite},
		{"wrapper", `env kontora delete kon-1 -f`, DecisionUnknown},
		{"interpreter", `sh -c 'kontora delete kon-1 -f'`, DecisionUnknown},
		{"dynamic executable", `"$bin" delete kon-1 -f`, DecisionUnknown},
		{"dynamic verb", `kontora "$verb" kon-1 -f`, DecisionUnknown},
		{"dynamic argument", `kontora note kon-1 "$text"`, DecisionUnknown},
		{"glob", `kontora view kon-*`, DecisionUnknown},
		{"brace expansion", `kontora {view,delete} kon-1`, DecisionUnknown},
		{"tilde expansion", `kontora note kon-1 ~`, DecisionUnknown},
		{"assignment", `PATH=/tmp kontora ls`, DecisionUnknown},
		{"delete assignment", `X=1 kontora delete kon-1 -f`, DecisionDelete},
		{"substitution", `kontora view "$(kontora ls)"`, DecisionUnknown},
		{"nested deletion", `kontora view "$(kontora delete kon-1 -f)"`, DecisionDelete},
		{"backtick deletion", "kontora view `kontora delete kon-1 -f`", DecisionDelete},
		{"process substitution deletion", `kontora ls > >(kontora delete kon-1 -f)`, DecisionDelete},
		{"input process substitution", `kontora ls < <(kontora delete kon-1 -f)`, DecisionDelete},
		{"unknown executable substitution", `kontora note kon-1 "$(rm file)"`, DecisionUnknown},
		{"arithmetic", `kontora note kon-1 "$((x++))"`, DecisionUnknown},
		{"conditional", `if kontora ls; then kontora stats; fi`, DecisionUnknown},
		{"conditional deletion", `if kontora ls; then kontora delete kon-1 -f; fi`, DecisionDelete},
		{"unknown beats write", `kontora note kon-1 hi; mystery`, DecisionUnknown},
		{"delete beats unknown", `mystery; kontora delete kon-1 -f > out`, DecisionDelete},
		{"parse error", `kontora ls &&`, DecisionUnknown},
		{"unterminated quote", `kontora "delete`, DecisionUnknown},
		{"comment only", `# kontora delete kon-1 -f`, DecisionUnknown},
		{"delete help remains conservative", `kontora delete -h`, DecisionDelete},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Classify("Bash", cmd(tt.command)))
		})
	}
}

func TestShellCommandBoundaries(t *testing.T) {
	tests := []struct {
		name, command string
		wantCalls     string
	}{
		{"comment backslash newline", "kontora version # \\\nkontora delete kon-1 -f", "version\ndelete\n"},
		{"carriage return before comment marker", "kontora version \r#; kontora delete kon-1 -f", "version\ndelete\n"},
		{"backslash CRLF", "kontora version \\\r\nkontora delete kon-1 -f", "version\ndelete\n"},
		{"comment ends with two backslashes", "kontora version # \\\\\nkontora delete kon-1 -f", "version\ndelete\n"},
		{"ordinary continuation", "kontora version \\\nkontora delete kon-1 -f", "version\n"},
		{"ordinary comment", "kontora version # kontora delete kon-1 -f", "version\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			// The function shadows the real CLI; only these fixed fixtures reach Bash.
			probe := exec.CommandContext(ctx, "bash", "--noprofile", "--norc", "-c", "kontora() { printf '%s\\n' \"$1\"; };\n"+tt.command)
			probe.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}
			probe.Dir = t.TempDir()
			output, err := probe.CombinedOutput()
			require.NoError(t, err, "%s", output)
			require.Equal(t, tt.wantCalls, string(output))
			decision := Classify("Bash", cmd(tt.command))
			if strings.Contains(tt.wantCalls, "delete\n") {
				assert.Equal(t, VerdictPark, Resolve(config.AutonomyAuto, decision))
				assert.Equal(t, VerdictPark, Resolve(config.AutonomyAsk, decision))
				assert.Equal(t, VerdictDeny, Resolve(config.AutonomyRead, decision))
			} else {
				assert.Equal(t, DecisionRead, decision)
			}
		})
	}
}

func TestClassificationDoesNotExecute(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "executed")
	for _, command := range []string{
		`kontora view "$(touch ` + marker + `)"`,
		"kontora view `touch " + marker + "`",
		`kontora view <(touch ` + marker + `)`,
		`kontora ls > >(touch ` + marker + `)`,
	} {
		t.Run(command, func(t *testing.T) {
			assert.Equal(t, DecisionUnknown, Classify("Bash", cmd(command)))
			_, err := os.Stat(marker)
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func FuzzClassifyShell(f *testing.F) {
	for _, seed := range []string{"kontora ls", "kontora delete kon-1 -f > out", `"kontora" 'delete' kon-1`, "$(touch marker)", "", "\x00", "&&", "kontora version # \\\nkontora delete kon-1 -f", "kontora version \r#; kontora delete kon-1 -f", "kontora version \\\r\nkontora delete kon-1 -f"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, command string) {
		decision := Classify("Bash", cmd(command))
		require.Contains(t, []Decision{DecisionRead, DecisionWrite, DecisionDelete, DecisionUnknown}, decision)
		if strings.ContainsRune(command, '\r') {
			assert.Contains(t, []Decision{DecisionDelete, DecisionUnknown}, decision)
		}
		if decision == DecisionDelete || decision == DecisionUnknown {
			assert.Equal(t, VerdictPark, Resolve(config.AutonomyAuto, decision))
		}
	})
}

func FuzzQuotedDeletion(f *testing.F) {
	for _, seed := range []string{"kon-1", "hello world", "'", "$(mystery)", "> out", "\n", "\x8c", "\x00"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, argument string) {
		quoted := "'" + strings.ReplaceAll(argument, "'", "'\\''") + "'"
		for _, command := range []string{
			`"kontora" 'delete' ` + quoted + " -f > out",
			`kon'tora' de"lete" ` + quoted + " -f 2>&1 | kontora ls",
		} {
			decision := Classify("Bash", cmd(command))
			assert.Equal(t, VerdictPark, Resolve(config.AutonomyAuto, decision))
			if utf8.ValidString(argument) && !strings.ContainsRune(argument, 0) {
				assert.Equal(t, DecisionDelete, decision)
			}
		}
	})
}
