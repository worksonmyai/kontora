package daemon

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/worksonmyai/kontora/internal/automation"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/worktree"
)

type automationCreateIntent struct {
	Fingerprint string                 `json:"fingerprint"`
	Before      string                 `json:"before"`
	After       string                 `json:"after"`
	Key         string                 `json:"key"`
	Result      *automation.Definition `json:"result,omitempty"`
}

func contentHash(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }

func (d *Daemon) CreateAutomation(requestID, key string, input automation.Spec) (automation.Definition, error) {
	d.configMutationMu.Lock()
	defer d.configMutationMu.Unlock()
	input = input.Normalize()
	if err := input.Validate(); err != nil {
		return automation.Definition{}, err
	}
	if strings.TrimSpace(requestID) == "" || len(requestID) > 200 {
		return automation.Definition{}, fmt.Errorf("%w: invalid request_id", automation.ErrValidation)
	}
	if key == "" {
		key = worktree.Slug(input.Name)
		if automation.ValidateKey(key) != nil {
			key = "automation-" + key
		}
	}
	if err := automation.ValidateKey(key); err != nil {
		return automation.Definition{}, err
	}
	dir, err := d.automationControlDir()
	if err != nil {
		return automation.Definition{}, err
	}
	path := filepath.Join(dir, "create-"+contentHash([]byte(requestID))+".json")
	payload, err := json.Marshal(struct {
		Key  string
		Spec automation.Spec
	}{key, input})
	if err != nil {
		return automation.Definition{}, err
	}
	fingerprint := contentHash(payload)
	_, before, _, err := d.configDocument()
	if err != nil {
		return automation.Definition{}, err
	}
	intent := automationCreateIntent{}
	b, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(b, &intent); err != nil {
			return automation.Definition{}, fmt.Errorf("%w: invalid create receipt", automation.ErrUnavailable)
		}
		if intent.Fingerprint != fingerprint {
			return automation.Definition{}, automation.ErrConflict
		}
		if intent.Result != nil {
			return *intent.Result, nil
		}
		if contentHash(before) == intent.After {
			if err := d.reloadConfig(); err != nil {
				return automation.Definition{}, err
			}
			return d.finishAutomationCreate(path, intent)
		}
		if contentHash(before) != intent.Before {
			return automation.Definition{}, fmt.Errorf("%w: config changed after an interrupted creation; inspect key %s", automation.ErrConflict, key)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return automation.Definition{}, err
	}
	current, err := config.LoadReader(strings.NewReader(string(before)))
	if err != nil {
		return automation.Definition{}, err
	}
	if _, exists := current.Automations[key]; exists {
		return automation.Definition{}, fmt.Errorf("%w: automation key %s already exists", automation.ErrConflict, key)
	}
	after, err := automationYAML(before, key, &input)
	if err != nil {
		return automation.Definition{}, fmt.Errorf("%w: %v", automation.ErrValidation, err)
	}
	if _, err := config.LoadReader(strings.NewReader(string(after))); err != nil {
		return automation.Definition{}, fmt.Errorf("%w: %v", automation.ErrValidation, err)
	}
	intent = automationCreateIntent{Fingerprint: fingerprint, Before: contentHash(before), After: contentHash(after), Key: key}
	if err := saveAutomationIntent(path, intent); err != nil {
		return automation.Definition{}, err
	}
	expected := string(before)
	if err := d.replaceConfig(string(after), &expected); err != nil {
		return automation.Definition{}, err
	}
	return d.finishAutomationCreate(path, intent)
}

func saveAutomationIntent(path string, intent automationCreateIntent) error {
	b, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	return durableConfigWrite(path, b, 0o600)
}

func (d *Daemon) finishAutomationCreate(path string, intent automationCreateIntent) (automation.Definition, error) {
	rows, err := d.ListAutomations()
	if err != nil {
		return automation.Definition{}, err
	}
	for _, row := range rows {
		if row.Key != intent.Key {
			continue
		}
		intent.Result = &row
		if err := saveAutomationIntent(path, intent); err != nil {
			return automation.Definition{}, err
		}
		d.automationChanged()
		return row, nil
	}
	return automation.Definition{}, fmt.Errorf("%w: saved automation is not active", automation.ErrUnavailable)
}

func (d *Daemon) UpdateAutomation(id string, revision uint64, input automation.Spec) (automation.Definition, error) {
	d.configMutationMu.Lock()
	defer d.configMutationMu.Unlock()
	row, err := d.GetAutomation(id)
	if err != nil {
		return row, err
	}
	if row.Revision != revision {
		return row, automation.ErrConflict
	}
	input = input.Normalize()
	if err := input.Validate(); err != nil {
		return row, err
	}
	if err := d.mutateAutomationConfig(row, &input); err != nil {
		return row, err
	}
	return d.GetAutomation(id)
}

func (d *Daemon) DeleteAutomation(id string, revision uint64) error {
	d.configMutationMu.Lock()
	defer d.configMutationMu.Unlock()
	row, err := d.GetAutomation(id)
	if err != nil {
		return err
	}
	if row.Revision != revision {
		return automation.ErrConflict
	}
	return d.mutateAutomationConfig(row, nil)
}

func (d *Daemon) mutateAutomationConfig(row automation.Definition, input *automation.Spec) error {
	_, before, _, err := d.configDocument()
	if err != nil {
		return err
	}
	cfg, err := config.LoadReader(strings.NewReader(string(before)))
	if err != nil {
		return err
	}
	actual, exists := cfg.Automations[row.Key]
	if !exists || !reflect.DeepEqual(actual.Normalize(), row.Normalize()) {
		return fmt.Errorf("%w: config changed; refresh before editing", automation.ErrConflict)
	}
	after, err := automationYAML(before, row.Key, input)
	if err != nil {
		return fmt.Errorf("%w: %v", automation.ErrValidation, err)
	}
	dir, err := d.automationControlDir()
	if err != nil {
		return err
	}
	intent := automationCreateIntent{Before: contentHash(before), After: contentHash(after), Key: row.Key}
	path := filepath.Join(dir, fmt.Sprintf("mutation-%s-%d.json", row.ID, time.Now().UnixNano()))
	if err := saveAutomationIntent(path, intent); err != nil {
		return err
	}
	expected := string(before)
	if err := d.replaceConfig(string(after), &expected); err != nil {
		return err
	}
	d.automationChanged()
	return os.Remove(path)
}
