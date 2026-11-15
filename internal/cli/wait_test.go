package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/session"
)

func waitInvocation(t *testing.T) session.Invocation {
	t.Helper()
	return session.Invocation{
		Version: session.WaitResultVersion, ID: "inv-wait", TicketID: "kon-test", Stage: "research",
		ResultPath: filepath.Join(t.TempDir(), "result.json"), WorkflowExecutionID: "execution-1",
		NodeVisitID: "visit-1", DefinitionFingerprint: "fingerprint-1", WakeMax: 3, CheckTimeout: time.Minute,
	}
}

func TestReportWait(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts WaitOptions
		kind string
	}{
		{"timer default summary", WaitOptions{Until: "2099-01-01T03:00:00+03:00", Next: "Repeat checks."}, "timer"},
		{"check literal text", WaitOptions{Check: "test -f 'DONE'; echo '$HOME 雪'", Poll: "10m", Deadline: "2099-01-02T00:00:00.123456789Z", Next: "Read data.\nReport result.", Summary: "Collecting data."}, "check"},
		{"check default summary", WaitOptions{Check: "test -f DONE", Poll: "10m", Deadline: "2099-01-01T00:00:00Z", Next: "Read data."}, "check"},
		{"past timer", WaitOptions{Until: "2000-01-01T00:00:00Z", Next: "Resume now."}, "timer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := waitInvocation(t)
			env := inv.Env()
			getenv := func(k string) string { return env[k] }
			var out bytes.Buffer
			for range 2 {
				out.Reset()
				require.NoError(t, ReportWait(tc.opts, &out, getenv))
				assert.JSONEq(t, `{"accepted":true,"invocation_id":"inv-wait","outcome":"waiting"}`, out.String())
			}
			result, err := session.ReadResult(inv.ResultPath, inv.Version)
			require.NoError(t, err)
			assert.Equal(t, "waiting", result.Outcome)
			assert.Equal(t, tc.kind, result.Wait.Kind)
			assert.Equal(t, tc.opts.Next, result.NextStep)
			assert.NotEmpty(t, result.Summary)
			assert.Empty(t, result.Verdict)
			if tc.kind == "check" {
				assert.Equal(t, tc.opts.Check, result.Wait.Command)
				assert.Equal(t, 10*time.Minute, result.Wait.Poll)
				if tc.opts.Summary == "" {
					assert.Equal(t, "Waiting for condition check.", result.Summary)
				} else {
					assert.Equal(t, tc.opts.Summary, result.Summary)
				}
			} else {
				assert.Equal(t, "Waiting for timer.", result.Summary)
			}
			before, err := os.ReadFile(inv.ResultPath)
			require.NoError(t, err)
			out.Reset()
			tc.opts.Next = "Conflicting continuation."
			require.ErrorContains(t, ReportWait(tc.opts, &out, getenv), "conflicting")
			assert.Empty(t, out.String())
			after, err := os.ReadFile(inv.ResultPath)
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
}

func TestReportWaitInvalid(t *testing.T) {
	valid := WaitOptions{Check: "test -f DONE", Poll: "10m", Deadline: "2099-01-01T00:00:00Z", Next: "Read data."}
	for _, tc := range []struct {
		name string
		edit func(*WaitOptions)
	}{
		{"neither", func(o *WaitOptions) { o.Check = "" }},
		{"both", func(o *WaitOptions) { o.Until = o.Deadline }},
		{"blank check", func(o *WaitOptions) { o.Check = " \n" }},
		{"missing poll", func(o *WaitOptions) { o.Poll = "" }},
		{"zero poll", func(o *WaitOptions) { o.Poll = "0s" }},
		{"negative poll", func(o *WaitOptions) { o.Poll = "-1m" }},
		{"overflow poll", func(o *WaitOptions) { o.Poll = "999999999999h" }},
		{"missing deadline", func(o *WaitOptions) { o.Deadline = "" }},
		{"relative deadline", func(o *WaitOptions) { o.Deadline = "10m" }},
		{"local deadline", func(o *WaitOptions) { o.Deadline = "2099-01-01T00:00:00" }},
		{"invalid until", func(o *WaitOptions) { *o = WaitOptions{Until: "tomorrow", Next: "Read data."} }},
		{"timer poll", func(o *WaitOptions) { o.Until = o.Deadline; o.Check = ""; o.Deadline = "" }},
		{"timer deadline", func(o *WaitOptions) { o.Until = o.Deadline; o.Check = ""; o.Poll = "" }},
		{"missing next", func(o *WaitOptions) { o.Next = "" }},
		{"blank summary", func(o *WaitOptions) { o.Summary = " \n" }},
		{"NUL", func(o *WaitOptions) { o.Check = "test\x00" }},
		{"invalid UTF-8", func(o *WaitOptions) { o.Next = string([]byte{0xff}) }},
		{"oversize", func(o *WaitOptions) { o.Next = strings.Repeat("雪", session.MaxResultBytes) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := waitInvocation(t)
			env := inv.Env()
			opts := valid
			tc.edit(&opts)
			var out bytes.Buffer
			require.Error(t, ReportWait(opts, &out, func(k string) string { return env[k] }))
			assert.Empty(t, out.String())
			assert.NoFileExists(t, inv.ResultPath)
		})
	}
}

func TestReportWaitRequiresInvocation(t *testing.T) {
	for _, version := range []int{session.ResultVersion, session.GraphResultVersion, session.WaitResultVersion} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			inv := waitInvocation(t)
			inv.Version = version
			env := inv.Env()
			if version == session.WaitResultVersion {
				delete(env, session.InvocationIDEnvVar)
			}
			var out bytes.Buffer
			require.Error(t, ReportWait(WaitOptions{Until: "2099-01-01T00:00:00Z", Next: "Resume."}, &out, func(k string) string { return env[k] }))
			assert.Empty(t, out.String())
			assert.NoFileExists(t, inv.ResultPath)
		})
	}
	require.Error(t, ReportWait(WaitOptions{}, nil, nil))
}

func TestReportWaitAcknowledgementFailure(t *testing.T) {
	inv := waitInvocation(t)
	env := inv.Env()
	opts := WaitOptions{Until: "2099-01-01T00:00:00Z", Next: "Resume."}
	getenv := func(k string) string { return env[k] }
	require.Error(t, ReportWait(opts, nil, getenv))
	assert.NoFileExists(t, inv.ResultPath)
	require.Error(t, ReportWait(opts, resultErrorWriter{}, getenv))
	assert.FileExists(t, inv.ResultPath)
}

func TestReportWaitThroughResult(t *testing.T) {
	inv := waitInvocation(t)
	env := inv.Env()
	payload := `{"outcome":"waiting","summary":"Waiting","next_step":"Resume.","wait":{"kind":"check","command":"test -f DONE","poll":600000000000,"deadline":"2099-01-01T00:00:00Z"}}`
	var out bytes.Buffer
	require.NoError(t, ReportResult(inv.TicketID, "", strings.NewReader(payload), &out, func(k string) string { return env[k] }))
	var ack map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &ack))
	assert.Equal(t, "waiting", ack["outcome"])
	out.Reset()
	err := ReportResult(inv.TicketID, "", strings.NewReader(payload), &out, func(k string) string { return env[k] }, "approve")
	require.Error(t, err)
	assert.Empty(t, out.String())
}

func TestWaitCommandMetadata(t *testing.T) {
	for _, command := range Commands {
		if command.Name != "wait" {
			continue
		}
		assert.False(t, command.Config)
		assert.False(t, command.Store)
		assert.False(t, command.Remote)
		assert.False(t, command.TicketID)
		assert.Empty(t, command.Args)
		var flags []string
		for _, flag := range command.Flags {
			flags = append(flags, flag.Name)
			assert.True(t, flag.TakesValue())
		}
		assert.ElementsMatch(t, []string{"check", "poll", "deadline", "until", "next", "summary"}, flags)
		return
	}
	t.Fatal("wait command missing from metadata")
}
