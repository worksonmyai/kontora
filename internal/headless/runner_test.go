package headless

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testDecision = `{"version":1,"turn_id":"turn-1","snapshot_id":"snapshot-1","actions":[]}`

func testProfile(kind string) Profile {
	return Profile{Binary: kind, Model: "test-model", Environment: map[string]string{"ANTHROPIC_API_KEY": "test-key"}}
}

func testTurn() Turn {
	return Turn{Input: []byte(`{"snapshot_id":"snapshot-1"}`), Timeout: time.Second, Limits: DefaultLimits()}
}

func fakeAdapter(stream string) Adapter {
	return Adapter{
		LookupBinary: func(s string) (string, error) { return "/resolved/" + filepath.Base(s), nil },
		Runner: func(_ context.Context, in Invocation) (ProcessResult, error) {
			_, err := io.WriteString(in.Stdout, stream)
			return ProcessResult{}, err
		},
	}
}

func TestPlannerInvocation(t *testing.T) {
	t.Setenv("KONTORA_OPERATOR_TOKEN", "must-not-inherit")
	t.Setenv("NODE_OPTIONS", "must-not-inherit")
	for _, kind := range []string{"claude", "pi"} {
		t.Run(kind, func(t *testing.T) {
			profile := testProfile(kind)
			effortFlag := "--effort"
			stream := claudeStream(testDecision)
			if kind == "pi" {
				effortFlag = "--thinking"
				stream = piStream(testDecision)
			}
			profile.Args = []string{"--model=old-model", effortFlag, "low"}
			profile.Effort = "high"
			original := slices.Clone(profile.Args)
			adapter := fakeAdapter(stream)
			var previous, current string
			adapter.Runner = func(ctx context.Context, in Invocation) (ProcessResult, error) {
				if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > time.Second {
					t.Fatal("missing context deadline")
				}
				if in.Binary != "/resolved/"+kind || in.Timeout != time.Second {
					t.Fatalf("wrong execution identity: %+v", in)
				}
				input, err := io.ReadAll(in.Stdin)
				if err != nil || !bytes.Equal(input, testTurn().Input) {
					t.Fatal("input was not sent through stdin")
				}
				if slices.Contains(in.Args, string(input)) {
					t.Fatal("snapshot leaked into argv")
				}
				for _, pair := range []struct{ flag, value string }{{"--model", "test-model"}, {effortFlag, "high"}, {"--system-prompt", SystemPrompt}} {
					i := slices.Index(in.Args, pair.flag)
					if i == -1 || i+1 >= len(in.Args) || in.Args[i+1] != pair.value {
						t.Fatalf("missing %s=%s in %v", pair.flag, pair.value, in.Args)
					}
				}
				for _, forbidden := range []string{"old-model", "low", "--continue", "--resume", "--session-id", "--session", "--settings", "-e"} {
					if slices.Contains(in.Args, forbidden) {
						t.Fatalf("unexpected %s in %v", forbidden, in.Args)
					}
				}
				required := []string{"--bare", "--strict-mcp-config", "--disable-slash-commands", "--no-session-persistence"}
				if kind == "pi" {
					required = []string{"--no-tools", "--no-mcp", "--no-extensions", "--no-skills", "--no-prompt-templates", "--no-themes", "--no-context-files", "--no-session", "--no-approve", "--offline"}
				}
				for _, flag := range required {
					if !slices.Contains(in.Args, flag) {
						t.Fatalf("missing %s", flag)
					}
				}
				if kind == "claude" {
					i := slices.Index(in.Args, "--tools")
					if i == -1 || in.Args[i+1] != "" {
						t.Fatal("built-in tools not disabled")
					}
				}
				assertPlannerIsolation(t, in, kind, previous)
				current = in.Dir
				_, err = io.WriteString(in.Stdout, stream)
				return ProcessResult{}, err
			}
			for range 2 {
				result, err := adapter.Run(context.Background(), profile, testTurn())
				if err != nil || string(result.Decision) != testDecision {
					t.Fatalf("decision: %s, error: %v", result.Decision, err)
				}
				if _, err := os.Stat(current); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("planner directory retained: %v", err)
				}
				previous = current
			}
			if !reflect.DeepEqual(original, profile.Args) {
				t.Fatal("configured arguments mutated")
			}
		})
	}
}

func assertPlannerIsolation(t *testing.T, in Invocation, kind, previous string) {
	t.Helper()
	for _, entry := range in.Env {
		if strings.Contains(entry, "must-not-inherit") || strings.HasPrefix(entry, "NODE_OPTIONS=") || strings.HasPrefix(entry, "KONTORA_") {
			t.Fatal("inherited process environment")
		}
	}
	if !slices.Contains(in.Env, "ANTHROPIC_API_KEY=test-key") {
		t.Fatal("explicit provider credential missing")
	}
	if kind == "pi" && !slices.Contains(in.Env, "PI_CODING_AGENT_DIR="+in.Dir) {
		t.Fatal("Pi agent directory is not isolated")
	}
	if in.Dir == previous {
		t.Fatal("planner reused a working directory")
	}
	info, err := os.Stat(in.Dir)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("planner directory permissions: %v %v", info, err)
	}
	entries, err := os.ReadDir(in.Dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("planner working directory was not empty")
	}
}

func TestPlannerRejectsProfilesBeforeLaunch(t *testing.T) {
	cases := []struct {
		name, kind string
		args       []string
	}{
		{"tools", "claude", []string{"--tools", "Read"}},
		{"empty tools still explicit", "claude", []string{"--tools="}},
		{"allowed tools", "claude", []string{"--allowedTools=Read"}},
		{"disallowed tools", "claude", []string{"--disallowed-tools", "Bash"}},
		{"MCP", "claude", []string{"--mcp-config=servers.json"}},
		{"settings hooks sandbox", "claude", []string{"--settings", "restricted.json"}},
		{"setting sources", "claude", []string{"--setting-sources=user"}},
		{"plugin", "claude", []string{"--plugin-dir", "sandbox-plugin"}},
		{"agent", "claude", []string{"--agent=planner"}},
		{"session", "claude", []string{"--session-id=saved"}},
		{"resume", "claude", []string{"-r", "saved"}},
		{"continue", "claude", []string{"-c"}},
		{"permission restriction", "claude", []string{"--permission-mode=plan"}},
		{"permission bypass", "claude", []string{"--dangerously-skip-permissions"}},
		{"system prompt", "claude", []string{"--append-system-prompt=other"}},
		{"output", "claude", []string{"--output-format=json"}},
		{"hook init", "claude", []string{"--init"}},
		{"bare", "claude", []string{"--bare"}},
		{"unknown", "claude", []string{"--future-flag"}},
		{"Pi tools", "pi", []string{"--tools=read"}},
		{"Pi tools alias", "pi", []string{"-t", "read"}},
		{"Pi tools disable", "pi", []string{"--no-tools"}},
		{"Pi MCP", "pi", []string{"--extension=builtin:mcp"}},
		{"Pi sandbox extension", "pi", []string{"-e", "sandbox.ts"}},
		{"Pi skill", "pi", []string{"--skill=resource"}},
		{"Pi prompt", "pi", []string{"--prompt-template=resource"}},
		{"Pi theme", "pi", []string{"--theme=resource"}},
		{"Pi session", "pi", []string{"--session=saved"}},
		{"Pi continue", "pi", []string{"--continue"}},
		{"Pi session directory", "pi", []string{"--session-dir=saved"}},
		{"Pi fork", "pi", []string{"--fork=saved"}},
		{"Pi trust", "pi", []string{"--approve"}},
		{"Pi multiple models", "pi", []string{"--models=a,b"}},
		{"Pi unknown extension flag", "pi", []string{"--sandbox-mode"}},
		{"positional prompt", "pi", []string{"do work"}},
		{"file attachment", "pi", []string{"@secret.txt"}},
		{"separator", "pi", []string{"--", "prompt"}},
		{"missing model", "pi", []string{"--model"}},
		{"missing effort", "claude", []string{"--effort"}},
		{"option value", "pi", []string{"--model", "--tools=read"}},
		{"empty model", "pi", []string{"--model="}},
		{"duplicate model", "pi", []string{"--model=a", "--model", "b"}},
		{"duplicate effort", "claude", []string{"--effort=low", "--effort=high"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := testProfile(tc.kind)
			p.Args = tc.args
			adapter := Adapter{Runner: func(context.Context, Invocation) (ProcessResult, error) {
				t.Fatal("unsafe profile launched")
				return ProcessResult{}, nil
			}, LookupBinary: func(string) (string, error) { t.Fatal("unsafe profile reached binary lookup"); return "", nil }}
			result, err := adapter.Run(context.Background(), p, testTurn())
			if err == nil || len(result.Decision) != 0 {
				t.Fatalf("unsafe profile accepted: %+v, %v", p, err)
			}
		})
	}
}

func TestPlannerSandboxWrappers(t *testing.T) {
	for _, kind := range []string{"claude", "pi"} {
		t.Run(kind, func(t *testing.T) {
			p := testProfile(kind)
			p.Binary = "nono"
			prefix := []string{"run", "--profile", "restricted", "--read", "/readonly", "--block-net", "--memory=512M", "--", kind}
			p.Args = append(slices.Clone(prefix), "--model=configured")
			p.Model = "override"
			original := slices.Clone(p.Args)
			stream := claudeStream(testDecision)
			if kind == "pi" {
				stream = piStream(testDecision)
			}
			a := fakeAdapter(stream)
			a.Runner = func(_ context.Context, in Invocation) (ProcessResult, error) {
				want := slices.Clone(prefix)
				want[len(want)-1] = "/resolved/" + kind
				if in.Binary != "/resolved/nono" || !reflect.DeepEqual(in.Args[:len(prefix)], want) {
					t.Fatalf("sandbox wrapper changed: %+v", in)
				}
				_, err := io.WriteString(in.Stdout, stream)
				return ProcessResult{}, err
			}
			if _, err := a.Run(context.Background(), p, testTurn()); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(p.Args, original) {
				t.Fatal("wrapper configuration mutated")
			}
		})
	}
	for _, tc := range []struct {
		binary string
		args   []string
	}{
		{"nono", []string{"run", "--profile", "restricted", "--", "pi", "-e", "sandbox.ts"}},
		{"nono", []string{"run", "--detached", "--", "pi"}},
		{"nono", []string{"run", "--remote", "--", "pi"}},
		{"nono", []string{"run", "--env-credential-map", "ref", "NODE_OPTIONS", "--", "pi"}},
		{"nono", []string{"run", "--future", "--", "pi"}},
		{"nono", []string{"run", "--profile", "--", "pi"}},
		{"nono", []string{"run", "pi"}},
		{"nono", []string{"run", "--", "op", "run", "--", "pi"}},
		{"op", []string{"run", "--env-file", "secrets", "--", "pi"}},
		{"sh", []string{"-c", "pi"}},
	} {
		p := testProfile("pi")
		p.Binary, p.Args = tc.binary, tc.args
		a := fakeAdapter("")
		a.Runner = func(context.Context, Invocation) (ProcessResult, error) {
			t.Fatal("incompatible wrapper launched")
			return ProcessResult{}, nil
		}
		if _, err := a.Run(context.Background(), p, testTurn()); err == nil {
			t.Fatalf("accepted wrapper %v", p)
		}
	}
}

func TestPlannerSandboxPaths(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, name := range []string{"policy", "policy.json"} {
		if err := os.WriteFile(name, []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, flag := range []string{"--config", "-c", "--allow", "-a", "--read", "-r", "--write", "-w", "--allow-file", "--read-file", "--write-file", "--profile", "-p", "--extends"} {
		for _, value := range []string{"policy", "policy.json", "./policy.json", "../policy.json", "~/policy.json", "/policy.json", "restricted"} {
			for _, inline := range []bool{false, true} {
				t.Run(flag+"/"+value+"/inline="+strconv.FormatBool(inline), func(t *testing.T) {
					p := testProfile("pi")
					p.Binary = "nono"
					p.Args = []string{"run", flag, value, "--", "pi"}
					if inline {
						p.Args = []string{"run", flag + "=" + value, "--", "pi"}
					}
					original := slices.Clone(p.Args)
					a := fakeAdapter(piStream(testDecision))
					launched := false
					a.Runner = func(_ context.Context, in Invocation) (ProcessResult, error) {
						launched = true
						if !reflect.DeepEqual(in.Args[:len(original)-1], original[:len(original)-1]) {
							t.Fatalf("sandbox policy changed: %v", in.Args)
						}
						_, err := io.WriteString(in.Stdout, piStream(testDecision))
						return ProcessResult{}, err
					}
					result, err := a.Run(context.Background(), p, testTurn())
					profileFlag := flag == "--profile" || flag == "-p" || flag == "--extends"
					if filepath.IsAbs(value) || profileFlag && value == "restricted" {
						if err != nil || !launched {
							t.Fatalf("absolute sandbox path or named profile rejected: %v", err)
						}
					} else if err == nil || launched || result.Decision != nil {
						t.Fatalf("relative sandbox path accepted: %v", err)
					}
					if !reflect.DeepEqual(p.Args, original) {
						t.Fatal("configured sandbox arguments mutated")
					}
				})
			}
		}
	}
}

func TestPlannerRelativeExecutables(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		t.Run("wrapped="+strconv.FormatBool(wrapped), func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			script := "#!/bin/sh\nprintf '%s' '" + strings.ReplaceAll(piStream(testDecision), "'", "'\\''") + "'\n"
			if err := os.WriteFile(filepath.Join(dir, "pi"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			p := testProfile("pi")
			p.Binary = "./pi"
			if wrapped {
				wrapper := "#!/bin/sh\nshift\nshift\nexec \"$@\"\n"
				if err := os.WriteFile(filepath.Join(dir, "nono"), []byte(wrapper), 0700); err != nil {
					t.Fatal(err)
				}
				p.Binary, p.Args = "./nono", []string{"run", "--", "./pi"}
			}
			a := Adapter{Runner: func(ctx context.Context, in Invocation) (ProcessResult, error) {
				if !filepath.IsAbs(in.Binary) || in.Dir == dir {
					t.Fatalf("executable was not anchored before changing working directory: %+v", in)
				}
				if wrapped && in.Args[2] != filepath.Join(dir, "pi") {
					t.Fatalf("wrapped executable was not anchored: %v", in.Args)
				}
				return DirectRunner(ctx, in)
			}}
			result, err := a.Run(context.Background(), p, testTurn())
			if err != nil || string(result.Decision) != testDecision {
				t.Fatalf("caller-relative executable failed: %v", err)
			}
		})
	}
}

func TestPlannerModelAndEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Profile)
	}{
		{"unknown binary", func(p *Profile) { p.Binary = "custom" }},
		{"missing model", func(p *Profile) { p.Model = "" }},
		{"invalid effort", func(p *Profile) { p.Effort = "unlimited" }},
		{"conflicting Pi thinking suffix", func(p *Profile) { p.Model = "test:high"; p.Effort = "low" }},
		{"operator token", func(p *Profile) { p.Environment["KONTORA_TOKEN"] = "operator" }},
		{"loader", func(p *Profile) { p.Environment["NODE_OPTIONS"] = "--import=unsafe" }},
		{"Pi directory", func(p *Profile) { p.Environment["PI_CODING_AGENT_DIR"] = "/existing" }},
		{"session", func(p *Profile) { p.Environment["PI_SESSION_ID"] = "existing" }},
		{"Claude config", func(p *Profile) { p.Environment["CLAUDE_CONFIG_DIR"] = "/existing" }},
		{"NUL", func(p *Profile) { p.Environment["PATH"] = "bad\x00path" }},
		{"missing Claude bare authentication", func(p *Profile) { p.Binary = "claude"; delete(p.Environment, "ANTHROPIC_API_KEY") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testProfile("pi")
			tc.change(&p)
			a := fakeAdapter("")
			a.Runner = func(context.Context, Invocation) (ProcessResult, error) {
				t.Fatal("invalid configuration launched")
				return ProcessResult{}, nil
			}
			if _, err := a.Run(context.Background(), p, testTurn()); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	p := testProfile("pi")
	p.Model = ""
	p.Args = []string{"--model=configured", "--thinking=low", "--provider", "anthropic"}
	_, _, args, err := plannerArgs(p)
	if err != nil || !slices.Contains(args, "configured") || !slices.Contains(args, "low") || !slices.Contains(args, "anthropic") {
		t.Fatalf("configured identity not resolved: %v, %v", args, err)
	}
}

func TestPlannerLimitsAndFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Turn)
	}{
		{"no timeout", func(v *Turn) { v.Timeout = 0 }},
		{"negative timeout", func(v *Turn) { v.Timeout = -time.Second }},
		{"empty input", func(v *Turn) { v.Input = nil }},
		{"slash command input", func(v *Turn) { v.Input = []byte("/compact") }},
		{"multiple snapshot objects", func(v *Turn) { v.Input = []byte("{} {}") }},
		{"large input", func(v *Turn) { v.Limits.Input = 1 }},
		{"trailing input padding", func(v *Turn) { v.Limits.Input = 2; v.Input = []byte("{}" + strings.Repeat(" ", 4096)) }},
		{"leading input padding", func(v *Turn) { v.Limits.Input = 2; v.Input = []byte(strings.Repeat("\n", 4096) + "{}") }},
		{"multibyte input", func(v *Turn) { v.Input = []byte(`{"x":"é"}`); v.Limits.Input = len(v.Input) - 1 }},
		{"single byte input overflow", func(v *Turn) { v.Input = []byte("{} "); v.Limits.Input = 2 }},
		{"invalid UTF-8", func(v *Turn) { v.Input = []byte{0xff} }},
		{"zero input limit", func(v *Turn) { v.Limits.Input = 0 }},
		{"zero output limit", func(v *Turn) { v.Limits.Output = 0 }},
		{"zero stderr limit", func(v *Turn) { v.Limits.Stderr = 0 }},
		{"zero decision limit", func(v *Turn) { v.Limits.Decision = 0 }},
		{"raised ceiling", func(v *Turn) { v.Limits.Output++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := testTurn()
			tc.change(&v)
			a := fakeAdapter("")
			a.Runner = func(context.Context, Invocation) (ProcessResult, error) {
				t.Fatal("invalid limits launched")
				return ProcessResult{}, nil
			}
			if _, err := a.Run(context.Background(), testProfile("pi"), v); err == nil {
				t.Fatal("invalid limits accepted")
			}
		})
	}
	for _, input := range []string{"{}", " \n{}\t ", `{"x":"é"}`} {
		t.Run("exact raw input limit/"+input, func(t *testing.T) {
			v := testTurn()
			v.Input, v.Limits.Input = []byte(input), len(input)
			a := fakeAdapter(piStream(testDecision))
			a.Runner = func(_ context.Context, in Invocation) (ProcessResult, error) {
				got, err := io.ReadAll(in.Stdin)
				if err != nil || !bytes.Equal(got, v.Input) {
					t.Fatalf("raw input changed: %q, %v", got, err)
				}
				_, err = io.WriteString(in.Stdout, piStream(testDecision))
				return ProcessResult{}, err
			}
			if _, err := a.Run(context.Background(), testProfile("pi"), v); err != nil {
				t.Fatalf("input at the raw byte limit rejected: %v", err)
			}
		})
	}
	for _, name := range []string{"stdout", "stderr", "decision", "nonzero", "runner error", "timeout", "cancelled", "binary lookup"} {
		t.Run(name, func(t *testing.T) {
			v := testTurn()
			a := fakeAdapter(piStream(testDecision))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch name {
			case "stdout":
				v.Limits.Output = 1
			case "stderr":
				v.Limits.Stderr = 1
				a.Runner = func(ctx context.Context, in Invocation) (ProcessResult, error) {
					_, _ = io.WriteString(in.Stderr, "too large")
					if ctx.Err() == nil {
						t.Fatal("overflow did not cancel runner")
					}
					_, _ = io.WriteString(in.Stdout, piStream(testDecision))
					return ProcessResult{}, nil
				}
			case "decision":
				v.Limits.Decision = 1
			case "nonzero":
				a.Runner = func(_ context.Context, in Invocation) (ProcessResult, error) {
					_, _ = io.WriteString(in.Stdout, piStream(testDecision))
					return ProcessResult{ExitCode: 7}, nil
				}
			case "runner error":
				a.Runner = func(context.Context, Invocation) (ProcessResult, error) {
					return ProcessResult{}, errors.New("spawn failed")
				}
			case "timeout":
				v.Timeout = time.Millisecond
				a.Runner = func(ctx context.Context, in Invocation) (ProcessResult, error) {
					<-ctx.Done()
					_, _ = io.WriteString(in.Stdout, piStream(testDecision))
					return ProcessResult{}, nil
				}
			case "cancelled":
				cancel()
			case "binary lookup":
				a.LookupBinary = func(string) (string, error) { return "", errors.New("not found") }
			}
			result, err := a.Run(ctx, testProfile("pi"), v)
			if err == nil || len(result.Decision) != 0 {
				t.Fatalf("failure authorized decision: %s, %v", result.Decision, err)
			}
			if name == "nonzero" && (len(result.Usage.Reports) == 0 || result.Usage.CostUSD == nil) {
				t.Fatal("failed process lost reported usage")
			}
		})
	}
}

func TestDirectRunner(t *testing.T) {
	t.Setenv("KONTORA_OPERATOR_TOKEN", "secret")
	for _, tc := range []struct {
		name, script string
		timeout      time.Duration
		fail         bool
	}{
		{"replacement environment", `printf '%s|%s' "${KONTORA_OPERATOR_TOKEN-unset}" "$EXPLICIT"`, time.Second, false},
		{"nonzero", "exit 9", time.Second, true},
		{"cancel process group", `sleep 30 & wait`, 30 * time.Millisecond, true},
		{"inherited output descriptors", `sleep 30 & exit 0`, time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			start := time.Now()
			result, err := DirectRunner(context.Background(), Invocation{Binary: "/bin/sh", Args: []string{"-c", tc.script}, Env: []string{"EXPLICIT=allowed"}, Stdout: &stdout, Stderr: io.Discard, Timeout: tc.timeout})
			if (err != nil) != tc.fail {
				t.Fatalf("runner error: %v", err)
			}
			if result.StartedAt.IsZero() || result.ExitedAt.Before(result.StartedAt) {
				t.Fatalf("invalid process accounting: %+v", result)
			}
			if time.Since(start) > 2*time.Second {
				t.Fatal("runner did not join promptly")
			}
			if tc.name == "replacement environment" && stdout.String() != "unset|allowed" {
				t.Fatalf("environment inherited: %q", stdout.String())
			}
			if tc.name == "nonzero" && result.ExitCode != 9 {
				t.Fatalf("lost exit code: %+v", result)
			}
		})
	}
	for _, in := range []Invocation{{Timeout: time.Second}, {Env: []string{}}, {Timeout: -time.Second, Env: []string{}}} {
		if _, err := DirectRunner(context.Background(), in); err == nil {
			t.Fatal("unsafe direct invocation accepted")
		}
	}
}

func TestDecisionJSONBoundary(t *testing.T) {
	for _, text := range []string{"", "null", "[]", "{} {}", "```json\n{}\n```", "{"} {
		a := fakeAdapter(piStream(text))
		result, err := a.Run(context.Background(), testProfile("pi"), testTurn())
		if err == nil || result.Decision != nil {
			t.Fatalf("accepted invalid decision %q", text)
		}
	}
	if err := decisionObject([]byte{'{', '"', 0xff, '"', ':', '1', '}'}, 100); err == nil {
		t.Fatal("invalid UTF-8 decision accepted")
	}
	// The protocol owner rejects stale IDs, unknown fields, and duplicate decision fields.
	a := fakeAdapter(piStream(`{"version":1,"unknown":true}`))
	result, err := a.Run(context.Background(), testProfile("pi"), testTurn())
	if err != nil || !json.Valid(result.Decision) {
		t.Fatalf("adapter took over parent protocol validation: %v", err)
	}
}
