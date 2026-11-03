package process

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSanitizeEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name      string
		inherited []string
		overrides []string
		keys      []string
		want      []string
	}{
		{name: "empty replacement is nonnil", want: []string{}},
		{
			name:      "remove inherited shared and operator credentials",
			inherited: []string{"PATH=/bin", "KONTORA_TOKEN=cli-secret", "KONTORA_WEB_TOKEN=web-secret", "KONTORA_OPERATOR_TOKEN=operator-secret", "CUSTOM_OPERATOR_SECRET=custom-secret", "MODEL_API_KEY=model-key"},
			keys:      []string{"CUSTOM_OPERATOR_SECRET"},
			want:      []string{"PATH=/bin", "MODEL_API_KEY=model-key"},
		},
		{
			name:      "overrides cannot restore secrets",
			inherited: []string{"PATH=/bin", "CUSTOM_OPERATOR_SECRET=inherited"},
			overrides: []string{"KONTORA_WEB_TOKEN=override", "CUSTOM_OPERATOR_SECRET=override", "KONTORA_TOKEN=override", "KONTORA_OPERATOR_TOKEN=override", "ATTEMPT_CAPABILITY=scoped"},
			keys:      []string{"CUSTOM_OPERATOR_SECRET", "CUSTOM_OPERATOR_SECRET"},
			want:      []string{"PATH=/bin", "ATTEMPT_CAPABILITY=scoped"},
		},
		{
			name:      "last value wins without duplicate keys",
			inherited: []string{"KEEP=old", "KEEP=middle", "EMPTY=value", "WITH_EQUALS=a=b"},
			overrides: []string{"KEEP=new", "EMPTY="},
			want:      []string{"KEEP=new", "EMPTY=", "WITH_EQUALS=a=b"},
		},
		{
			name:      "remove all inherited duplicates and empty secrets",
			inherited: []string{"CUSTOM_OPERATOR_SECRET=a", "CUSTOM_OPERATOR_SECRET=b", "KONTORA_TOKEN="},
			keys:      []string{"CUSTOM_OPERATOR_SECRET"},
			want:      []string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inherited := append([]string(nil), tc.inherited...)
			overrides := append([]string(nil), tc.overrides...)
			env, err := SanitizeEnvironment(tc.inherited, tc.overrides, tc.keys)
			require.NoError(t, err)
			require.NotNil(t, env)
			assert.Equal(t, tc.want, env)
			assert.Equal(t, inherited, tc.inherited)
			assert.Equal(t, overrides, tc.overrides)
		})
	}
}

func TestSanitizeEnvironmentRejectsMalformedInputs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		inherited []string
		overrides []string
		keys      []string
	}{
		{name: "empty secret key", keys: []string{""}},
		{name: "secret key containing equals", keys: []string{"KEY=secret-value"}},
		{name: "secret key containing NUL", keys: []string{"KEY\x00secret-value"}},
		{name: "missing equals", inherited: []string{"secret-value"}},
		{name: "empty key", inherited: []string{"=secret-value"}},
		{name: "NUL in inherited value", inherited: []string{"KEY=secret-value\x00"}},
		{name: "NUL in override key", overrides: []string{"KEY\x00=secret-value"}},
		{name: "invalid blocked override", overrides: []string{"KONTORA_TOKEN=secret-value\x00"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, err := SanitizeEnvironment(tc.inherited, tc.overrides, tc.keys)
			require.Error(t, err)
			assert.Nil(t, env)
			assert.NotContains(t, err.Error(), "secret-value")
		})
	}
}

func TestWorkerEnvironmentRemovesActualInheritedSecrets(t *testing.T) {
	t.Setenv("KONTORA_TOKEN", "cli-secret")
	t.Setenv("KONTORA_WEB_TOKEN", "web-secret")
	t.Setenv("KONTORA_OPERATOR_TOKEN", "operator-secret")
	t.Setenv("CUSTOM_OPERATOR_SECRET", "custom-secret")
	t.Setenv("KONTORA_ENV_KEEP", "inherited")
	env, err := WorkerEnvironment([]string{
		"KONTORA_ENV_KEEP=overridden",
		"CUSTOM_OPERATOR_SECRET=must-not-return",
		"ATTEMPT_CAPABILITY=attempt-scoped",
	}, []string{"CUSTOM_OPERATOR_SECRET"})
	require.NoError(t, err)

	// A missing secret is different from one present with an empty value.
	script := `for key in KONTORA_TOKEN KONTORA_WEB_TOKEN KONTORA_OPERATOR_TOKEN CUSTOM_OPERATOR_SECRET; do
		eval 'present=${'"$key"'+yes}'
		if [ "$present" = yes ]; then exit 91; fi
	done
	printf '%s|%s' "$KONTORA_ENV_KEEP" "$ATTEMPT_CAPABILITY"`
	for _, runner := range []string{"process.Run", "exec.Cmd"} {
		t.Run(runner, func(t *testing.T) {
			var output bytes.Buffer
			if runner == "process.Run" {
				result, err := Run(context.Background(), RunParams{
					Binary: "/bin/sh", Args: []string{"-c", script}, Env: env, ReplaceEnv: true, Stdout: &output,
				})
				require.NoError(t, err)
				require.Equal(t, 0, result.ExitCode)
			} else {
				cmd := exec.Command("/bin/sh", "-c", script)
				cmd.Env, cmd.Stdout = env, &output
				require.NoError(t, cmd.Run())
			}
			assert.Equal(t, "overridden|attempt-scoped", output.String())
		})
	}
	assert.Equal(t, "custom-secret", os.Getenv("CUSTOM_OPERATOR_SECRET"), "sanitizing must not mutate the daemon's environment")
}

func TestSanitizedEmptyEnvironmentDoesNotInherit(t *testing.T) {
	t.Setenv("KONTORA_OPERATOR_TOKEN", "inherited-secret")
	env, err := SanitizeEnvironment([]string{"KONTORA_OPERATOR_TOKEN=secret"}, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, env)
	cmd := exec.Command("/bin/sh", "-c", `test "${KONTORA_OPERATOR_TOKEN+present}" != present`)
	cmd.Env = env
	require.NoError(t, cmd.Run())
}
