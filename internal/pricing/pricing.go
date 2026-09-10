// Package pricing resolves recorded model names against the bundled OpenRouter
// catalog and estimates token costs without floating-point arithmetic.
package pricing

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
)

// Rates are OpenRouter's USD-per-token prices. An empty field means that the
// catalog did not provide that category; zero is represented by "0".
type Rates struct {
	Prompt          string `json:"prompt,omitempty"`
	Completion      string `json:"completion,omitempty"`
	InputCacheRead  string `json:"input_cache_read,omitempty"`
	InputCacheWrite string `json:"input_cache_write,omitempty"`
}

type Model struct {
	ID      string `json:"id"`
	Pricing Rates  `json:"pricing"`
}

// Usage contains the four disjoint token categories stored in an activity
// sidecar. CacheWrite corresponds to OpenRouter's input_cache_write rate.
type Usage struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}

// Amount is an exact fixed-point US-dollar value. The catalog contains rates
// finer than a millionth of a dollar, so retaining their decimal scale keeps a
// small positive estimate distinct from a measured free run.
type Amount struct {
	units *big.Int
	scale int
}

func (a Amount) Add(other Amount) Amount {
	scale := max(a.scale, other.scale)
	left := scaledUnits(a, scale)
	right := scaledUnits(other, scale)
	return Amount{units: left.Add(left, right), scale: scale}
}

// String returns a decimal USD value suitable for a cost_usd field. It keeps
// at least six fractional places for a stable API shape and retains finer
// non-zero digits when the catalog needs them.
func (a Amount) String() string {
	units := new(big.Int)
	if a.units != nil {
		units.Set(a.units)
	}
	sign := ""
	if units.Sign() < 0 {
		sign = "-"
		units.Neg(units)
	}

	digits := units.String()
	scale := a.scale
	if scale > 0 && len(digits) <= scale {
		digits = strings.Repeat("0", scale-len(digits)+1) + digits
	}
	whole, fraction := digits, ""
	if scale > 0 {
		whole = digits[:len(digits)-scale]
		fraction = digits[len(digits)-scale:]
	}
	fraction = strings.TrimRight(fraction, "0")
	if len(fraction) < 6 {
		fraction += strings.Repeat("0", 6-len(fraction))
	}
	return sign + whole + "." + fraction
}

func scaledUnits(amount Amount, scale int) *big.Int {
	units := new(big.Int)
	if amount.units != nil {
		units.Set(amount.units)
	}
	if scale > amount.scale {
		units.Mul(units, pow10(scale-amount.scale))
	}
	return units
}

func pow10(power int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(power)), nil)
}

//go:embed catalog.json
var catalogJSON []byte

var embeddedCatalog = mustLoadCatalog(catalogJSON)

type catalogFile struct {
	Models []Model `json:"models"`
}

type catalog struct {
	models map[string]Model
	leaves map[string][]string
}

func mustLoadCatalog(data []byte) catalog {
	var file catalogFile
	if err := json.Unmarshal(data, &file); err != nil {
		panic(fmt.Sprintf("pricing: invalid embedded catalog: %v", err))
	}
	if len(file.Models) == 0 {
		panic("pricing: embedded catalog has no models")
	}

	c := catalog{
		models: make(map[string]Model, len(file.Models)),
		leaves: make(map[string][]string, len(file.Models)),
	}
	for _, model := range file.Models {
		if model.ID == "" {
			panic("pricing: empty model ID in embedded catalog")
		}
		if _, exists := c.models[model.ID]; exists {
			panic(fmt.Sprintf("pricing: duplicate model ID %q in embedded catalog", model.ID))
		}
		rates := []struct{ name, value string }{
			{"prompt", model.Pricing.Prompt},
			{"completion", model.Pricing.Completion},
			{"input_cache_read", model.Pricing.InputCacheRead},
			{"input_cache_write", model.Pricing.InputCacheWrite},
		}
		for _, rate := range rates {
			if rate.value != "" && !validDecimal(rate.value) {
				panic(fmt.Sprintf("pricing: invalid %s rate for %q", rate.name, model.ID))
			}
		}
		c.models[model.ID] = model
		leaf := model.ID[strings.LastIndex(model.ID, "/")+1:]
		c.leaves[leaf] = append(c.leaves[leaf], model.ID)
	}
	return c
}

// Models returns a copy of the embedded normalized catalog in snapshot order.
func Models() []Model {
	var file catalogFile
	if err := json.Unmarshal(catalogJSON, &file); err != nil {
		panic(fmt.Sprintf("pricing: invalid embedded catalog: %v", err))
	}
	return file.Models
}

// Resolve conservatively maps a model string recorded by supported agents to
// one catalog entry. Exact IDs win over aliases. Qualified unknown names never
// fall back to a similarly named model from another provider.
func Resolve(name string) (Model, bool) {
	return embeddedCatalog.resolve(name)
}

func (c catalog) resolve(name string) (Model, bool) {
	if model, ok := c.models[name]; ok {
		return model, true
	}

	candidate := stripThinkingSuffix(name)
	if model, ok := c.models[candidate]; ok {
		return model, true
	}

	if codexModel, ok := strings.CutPrefix(candidate, "openai-codex/"); ok {
		candidate = "openai/" + codexModel
		if model, ok := c.models[candidate]; ok {
			return model, true
		}
	}

	if anthropic, ok := anthropicAlias(candidate); ok {
		if model, found := c.models[anthropic]; found {
			return model, true
		}
	}

	if strings.Contains(candidate, "/") {
		return Model{}, false
	}
	matches := c.leaves[candidate]
	if len(matches) != 1 {
		return Model{}, false
	}
	return c.models[matches[0]], true
}

var thinkingLevels = map[string]struct{}{
	"off": {}, "minimal": {}, "low": {}, "medium": {}, "high": {}, "xhigh": {}, "max": {},
}

func stripThinkingSuffix(name string) string {
	slash := strings.LastIndex(name, "/")
	colon := strings.LastIndex(name, ":")
	if colon <= slash || colon == len(name)-1 {
		return name
	}
	if _, ok := thinkingLevels[name[colon+1:]]; !ok {
		return name
	}
	return name[:colon]
}

func anthropicAlias(name string) (string, bool) {
	model := name
	qualified := false
	if direct, ok := strings.CutPrefix(model, "anthropic/"); ok {
		model = direct
		qualified = true
	} else if strings.Contains(model, "/") {
		return "", false
	}
	if !strings.HasPrefix(model, "claude-") {
		return "", false
	}

	// Anthropic's direct API uses claude-opus-4-6 while OpenRouter uses
	// anthropic/claude-opus-4.6. Replace only the first numeric separator so
	// any date suffix remains untouched.
	changed := false
	for i := 1; i+1 < len(model); i++ {
		if model[i] == '-' && model[i-1] >= '0' && model[i-1] <= '9' &&
			model[i+1] >= '0' && model[i+1] <= '9' {
			model = model[:i] + "." + model[i+1:]
			changed = true
			break
		}
	}
	if !qualified && !changed {
		return "", false
	}
	return "anthropic/" + model, true
}

// Calculate estimates a usage total at the supplied rates. It returns false
// for negative token counts, a negative applicable rate, or a missing
// applicable rate. Missing rates for empty categories do not prevent a
// measured zero cost.
func Calculate(rates Rates, usage Usage) (Amount, bool) {
	categories := []struct {
		tokens int64
		rate   string
	}{
		{usage.Input, rates.Prompt},
		{usage.Output, rates.Completion},
		{usage.CacheRead, rates.InputCacheRead},
		{usage.CacheWrite, rates.InputCacheWrite},
	}

	var total Amount
	for _, category := range categories {
		if category.tokens < 0 {
			return Amount{}, false
		}
		if category.tokens == 0 {
			continue
		}
		if category.rate == "" || !validDecimal(category.rate) {
			return Amount{}, false
		}
		rate, ok := parseDecimal(category.rate)
		if !ok || rate.units.Sign() < 0 {
			return Amount{}, false
		}
		part := Amount{units: new(big.Int).Mul(rate.units, big.NewInt(category.tokens)), scale: rate.scale}
		total = total.Add(part)
	}

	return total, true
}

func Estimate(model string, usage Usage) (Amount, bool) {
	resolved, ok := Resolve(model)
	if !ok {
		return Amount{}, false
	}
	return Calculate(resolved.Pricing, usage)
}

func parseDecimal(value string) (Amount, bool) {
	if !validDecimal(value) {
		return Amount{}, false
	}
	whole, fraction, dotted := strings.Cut(value, ".")
	scale := 0
	if dotted {
		scale = len(fraction)
	}
	units, ok := new(big.Int).SetString(whole+fraction, 10)
	if !ok {
		return Amount{}, false
	}
	return Amount{units: units, scale: scale}, true
}

func validDecimal(value string) bool {
	if value == "" {
		return false
	}
	if value[0] == '-' {
		value = value[1:]
		if value == "" {
			return false
		}
	}
	whole, fraction, dotted := strings.Cut(value, ".")
	if whole == "" || (len(whole) > 1 && whole[0] == '0') || !allDigits(whole) {
		return false
	}
	return !dotted || fraction != "" && allDigits(fraction)
}

func allDigits(value string) bool {
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}
