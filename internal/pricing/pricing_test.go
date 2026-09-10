package pricing

import (
	"encoding/json"
	"math"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolve(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "exact ID", input: "openai/gpt-5.6-sol", want: "openai/gpt-5.6-sol"},
		{name: "pi thinking suffix", input: "openai/gpt-5.6-sol:high", want: "openai/gpt-5.6-sol"},
		{name: "newest pi thinking suffix", input: "openai/gpt-5.6-sol:max", want: "openai/gpt-5.6-sol"},
		{name: "codex provider", input: "openai-codex/gpt-5.6-sol", want: "openai/gpt-5.6-sol"},
		{name: "codex provider and thinking", input: "openai-codex/gpt-5.6-sol:xhigh", want: "openai/gpt-5.6-sol"},
		{name: "unrelated qualified model is not an Anthropic alias", input: "other/claude-opus-4-6", want: ""},
		{name: "Anthropic direct model", input: "claude-opus-4-6", want: "anthropic/claude-opus-4.6"},
		{name: "qualified Anthropic direct model", input: "anthropic/claude-sonnet-4-6:medium", want: "anthropic/claude-sonnet-4.6"},
		{name: "unique leaf", input: "gemini-2.5-pro", want: "google/gemini-2.5-pro"},
		{name: "unknown qualified model", input: "other/gemini-2.5-pro", want: ""},
		{name: "unknown model", input: "does-not-exist", want: ""},
		{name: "unknown OpenRouter suffix is retained", input: "openai/gpt-5.6-sol:free", want: ""},
		{name: "whitespace is not guessed away", input: " gpt-5.6-sol ", want: ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			model, ok := Resolve(test.input)
			if test.want == "" {
				assert.False(t, ok)
				assert.Zero(t, model)
				return
			}
			require.True(t, ok)
			assert.Equal(t, test.want, model.ID)
		})
	}
}

func TestResolvePrefersExactID(t *testing.T) {
	c := catalog{
		models: map[string]Model{
			"openai/example":            {ID: "openai/example"},
			"openai/example:high":       {ID: "openai/example:high"},
			"openai-codex/example":      {ID: "openai-codex/example"},
			"one/shared":                {ID: "one/shared"},
			"two/shared":                {ID: "two/shared"},
			"anthropic/claude-opus-4.6": {ID: "anthropic/claude-opus-4.6"},
			"another/claude-opus-4.6":   {ID: "another/claude-opus-4.6"},
		},
		leaves: map[string][]string{
			"shared":          {"one/shared", "two/shared"},
			"claude-opus-4.6": {"anthropic/claude-opus-4.6", "another/claude-opus-4.6"},
		},
	}

	model, ok := c.resolve("openai-codex/example")
	require.True(t, ok)
	assert.Equal(t, "openai-codex/example", model.ID)
	model, ok = c.resolve("openai/example:high")
	require.True(t, ok)
	assert.Equal(t, "openai/example:high", model.ID)
	model, ok = c.resolve("shared")
	assert.False(t, ok)
	assert.Zero(t, model)
	model, ok = c.resolve("claude-opus-4.6")
	assert.False(t, ok, "an unqualified Anthropic-shaped leaf remains ambiguous")
	assert.Zero(t, model)
	model, ok = c.resolve("claude-opus-4-6")
	require.True(t, ok, "direct Anthropic punctuation is a provider-specific alias")
	assert.Equal(t, "anthropic/claude-opus-4.6", model.ID)
}

func TestEmbeddedCatalog(t *testing.T) {
	models := Models()
	require.NotEmpty(t, models)

	ids := make([]string, len(models))
	for i, model := range models {
		ids[i] = model.ID
		for name, rate := range map[string]string{
			"prompt":            model.Pricing.Prompt,
			"completion":        model.Pricing.Completion,
			"input_cache_read":  model.Pricing.InputCacheRead,
			"input_cache_write": model.Pricing.InputCacheWrite,
		} {
			if rate != "" {
				assert.True(t, validDecimal(rate), "%s: %s=%q", model.ID, name, rate)
			}
		}
	}
	assert.True(t, slices.IsSorted(ids))
	assert.Contains(t, ids, "openai/gpt-5.6-sol")
}

func TestMustLoadCatalogRejectsInvalidData(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{name: "malformed JSON", data: `{`},
		{name: "empty catalog", data: `{"models":[]}`},
		{name: "empty ID", data: `{"models":[{"id":"","pricing":{}}]}`},
		{name: "duplicate ID", data: `{"models":[{"id":"p/m","pricing":{}},{"id":"p/m","pricing":{}}]}`},
		{name: "invalid rate", data: `{"models":[{"id":"p/m","pricing":{"prompt":"1e-6"}}]}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Panics(t, func() { mustLoadCatalog([]byte(test.data)) })
		})
	}
}

func TestEmbeddedCatalogSource(t *testing.T) {
	var file struct {
		Source map[string]string `json:"source"`
	}
	require.NoError(t, json.Unmarshal(catalogJSON, &file))
	assert.Equal(t, map[string]string{
		"name":              "OpenRouter",
		"url":               "https://openrouter.ai/api/v1/models",
		"documentation_url": "https://openrouter.ai/docs/guides/overview/models",
		"attribution":       "Model pricing data provided by OpenRouter (https://openrouter.ai/).",
		"rate_basis":        "Standard top-provider text token prices in USD per token",
	}, file.Source)
}

func TestCalculate(t *testing.T) {
	rates := Rates{
		Prompt:          "0.000001",
		Completion:      "0.000002",
		InputCacheRead:  "0.0000001",
		InputCacheWrite: "0.000003",
	}

	tests := []struct {
		name  string
		rates Rates
		usage Usage
		want  string
		ok    bool
	}{
		{
			name:  "all four categories",
			rates: rates,
			usage: Usage{Input: 1_000_000, Output: 100_000, CacheRead: 1_000_000, CacheWrite: 100_000},
			want:  "1.600000",
			ok:    true,
		},
		{name: "free", rates: Rates{Prompt: "0", Completion: "0", InputCacheRead: "0", InputCacheWrite: "0"}, usage: Usage{Input: math.MaxInt64, Output: 2, CacheRead: 3, CacheWrite: 4}, want: "0.000000", ok: true},
		{name: "measured zero permits missing rates", rates: Rates{}, usage: Usage{}, want: "0.000000", ok: true},
		{name: "zero category permits missing rate", rates: Rates{Prompt: "0.1"}, usage: Usage{Input: 1}, want: "0.100000", ok: true},
		{name: "missing prompt", rates: Rates{}, usage: Usage{Input: 1}, ok: false},
		{name: "missing completion", rates: Rates{Prompt: "1"}, usage: Usage{Output: 1}, ok: false},
		{name: "missing cache read", rates: Rates{Prompt: "1"}, usage: Usage{CacheRead: 1}, ok: false},
		{name: "missing cache write", rates: Rates{Prompt: "1"}, usage: Usage{CacheWrite: 1}, ok: false},
		{name: "negative input", rates: rates, usage: Usage{Input: -1}, ok: false},
		{name: "negative output", rates: rates, usage: Usage{Output: -1}, ok: false},
		{name: "negative cache read", rates: rates, usage: Usage{CacheRead: -1}, ok: false},
		{name: "negative cache write", rates: rates, usage: Usage{CacheWrite: -1}, ok: false},
		{name: "negative sentinel is unusable", rates: Rates{Prompt: "-1"}, usage: Usage{Input: 1}, ok: false},
		{name: "negative unused rate is harmless", rates: Rates{Prompt: "-1"}, usage: Usage{}, want: "0.000000", ok: true},
		{name: "positive sub-micro estimate is not free", rates: Rates{Prompt: "0.000000499"}, usage: Usage{Input: 1}, want: "0.000000499", ok: true},
		{name: "half-micro boundary remains exact", rates: Rates{Prompt: "0.0000005"}, usage: Usage{Input: 1}, want: "0.0000005", ok: true},
		{name: "next display boundary remains exact", rates: Rates{Prompt: "0.000001499"}, usage: Usage{Input: 1}, want: "0.000001499", ok: true},
		{name: "category sum remains exact", rates: Rates{Prompt: "0.00000025", Completion: "0.00000025"}, usage: Usage{Input: 1, Output: 1}, want: "0.0000005", ok: true},
		{name: "large total", rates: Rates{Prompt: "1000000000"}, usage: Usage{Input: math.MaxInt64}, want: "9223372036854775807000000000.000000", ok: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := Calculate(test.rates, test.usage)
			assert.Equal(t, test.ok, ok)
			if test.ok {
				assert.Equal(t, test.want, got.String())
			} else {
				assert.Equal(t, Amount{}, got)
			}
		})
	}
}

func TestEstimateAndAmountString(t *testing.T) {
	cost, ok := Estimate("openai-codex/gpt-5.6-sol:high", Usage{Input: 1, Output: 1})
	require.True(t, ok)
	assert.NotEmpty(t, cost.String())
	left, ok := parseDecimal("0.000000035")
	require.True(t, ok)
	right, ok := parseDecimal("12.345678000")
	require.True(t, ok)
	assert.Equal(t, "0.000000035", left.String())
	assert.Equal(t, "12.345678", right.String())
	assert.Equal(t, "12.345678035", left.Add(right).String())
}
