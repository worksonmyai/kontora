package config

import (
	"fmt"
	"maps"
	"slices"

	"github.com/worksonmyai/kontora/internal/automation"
	"github.com/worksonmyai/kontora/internal/prompt"
)

func (c *Config) applyAutomationDefaults() {
	for key, spec := range c.Automations {
		spec = spec.Normalize()
		if spec.Agent == "" {
			spec.Agent = c.DefaultAgent
		}
		c.Automations[key] = spec
	}
}

func (c *Config) validateAutomations() error {
	for _, key := range slices.Sorted(maps.Keys(c.Automations)) {
		spec := c.Automations[key]
		if err := automation.ValidateKey(key); err != nil {
			return fmt.Errorf("automations.%s: %w", key, err)
		}
		if err := spec.Validate(); err != nil {
			return fmt.Errorf("automations.%s: %w", key, err)
		}
		agent, ok := c.Agents[spec.Agent]
		if !ok {
			return fmt.Errorf("automations.%s: unknown agent %q", key, spec.Agent)
		}
		if spec.Model != "" && agent.Kind() == "" {
			return fmt.Errorf("automations.%s: agent %q does not support a model override", key, spec.Agent)
		}
		if _, err := prompt.CompileAutomation(spec.Prompt); err != nil {
			return fmt.Errorf("automations.%s.prompt: %w", key, err)
		}
	}
	return nil
}
