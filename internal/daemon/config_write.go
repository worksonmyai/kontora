package daemon

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/worksonmyai/kontora/internal/automation"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/web"
)

func (d *Daemon) PutRawConfigIf(content, expected string) error {
	d.configMutationMu.Lock()
	defer d.configMutationMu.Unlock()
	return d.replaceConfig(content, &expected)
}

// PutThemePreference patches the current document rather than replacing it with a UI draft.
func (d *Daemon) PutThemePreference(theme string) error {
	if err := config.ValidateWebTheme(theme); err != nil {
		return fmt.Errorf("%w: %v", web.ErrInvalidConfig, err)
	}
	d.configMutationMu.Lock()
	defer d.configMutationMu.Unlock()
	_, before, _, err := d.configDocument()
	if err != nil {
		return err
	}
	after, err := themeYAML(before, theme)
	if err != nil {
		return fmt.Errorf("%w: %v", web.ErrInvalidConfig, err)
	}
	expected := string(before)
	return d.replaceConfig(string(after), &expected)
}

func (d *Daemon) replaceConfig(content string, expected *string) error {
	path, before, mode, err := d.configDocument()
	if err != nil {
		return err
	}
	if expected != nil && *expected != string(before) {
		return fmt.Errorf("%w: config changed; reload before saving", automation.ErrConflict)
	}
	if _, err := config.LoadReader(strings.NewReader(content)); err != nil {
		return fmt.Errorf("%w: %s", web.ErrInvalidConfig, err)
	}
	if err := rejectExtraYAML([]byte(content)); err != nil {
		return fmt.Errorf("%w: %s", web.ErrInvalidConfig, err)
	}
	currentPath, current, _, err := d.configDocument()
	if err != nil {
		return err
	}
	if path != currentPath || !bytes.Equal(before, current) {
		return fmt.Errorf("%w: config changed during save", automation.ErrConflict)
	}
	if err := durableConfigWrite(path, []byte(content), mode); err != nil {
		return err
	}
	if err := d.reloadConfig(); err != nil {
		return &web.ConfigSavedReloadError{Err: err}
	}
	return nil
}

func (d *Daemon) configDocument() (string, []byte, os.FileMode, error) {
	if d.configPath == "" {
		return "", nil, 0, web.ErrConfigPathNotSet
	}
	path, err := filepath.EvalSymlinks(d.configPath)
	if err != nil {
		return "", nil, 0, err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", nil, 0, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return "", nil, 0, errors.New("config must be a regular file")
	}
	b, err := os.ReadFile(path)
	return path, b, info.Mode().Perm(), err
}

func rejectExtraYAML(b []byte) error {
	_, err := parseConfigYAML(b)
	return err
}

func parseConfigYAML(b []byte) (*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	if err := dec.Decode(new(yaml.Node)); !errors.Is(err, io.EOF) {
		return nil, errors.New("config must contain one YAML document")
	}
	return &doc, nil
}

func durableConfigWrite(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".kontora-config-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func themeYAML(content []byte, theme string) ([]byte, error) {
	doc, err := parseConfigYAML(content)
	if err != nil {
		return nil, err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode || doc.Content[0].Anchor != "" {
		return nil, errors.New("use a top-level YAML mapping without an anchor")
	}
	root := doc.Content[0]
	var webNode *yaml.Node
	rootMerge := false
	for i := 0; i < len(root.Content); i += 2 {
		key, value := root.Content[i], root.Content[i+1]
		switch {
		case key.ShortTag() == "!!merge":
			rootMerge = true
		case key.Value == "web":
			isNull := value.Kind == yaml.ScalarNode && value.ShortTag() == "!!null"
			if webNode != nil || yamlShared(key) || value.Anchor != "" || (value.Kind != yaml.MappingNode && !isNull) {
				return nil, errors.New("define `web` once as a mapping without anchors or aliases on the key or value")
			}
			webNode = value
		}
	}
	if webNode == nil {
		if rootMerge {
			return nil, errors.New("edit `config.yaml` directly; if a YAML merge supplies `web`, copy all inherited web settings before defining `web` directly")
		}
		webNode = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "web"}, webNode)
	} else if webNode.Kind == yaml.ScalarNode {
		webNode.Kind, webNode.Tag, webNode.Value, webNode.Style = yaml.MappingNode, "!!map", "", 0
	}
	var current *yaml.Node
	for i := 0; i < len(webNode.Content); i += 2 {
		key, value := webNode.Content[i], webNode.Content[i+1]
		switch {
		case key.ShortTag() == "!!merge":
			return nil, errors.New("theme edits do not support YAML merges in web")
		case key.Value == "theme":
			if current != nil || yamlShared(key) || value.Kind != yaml.ScalarNode || value.Anchor != "" {
				return nil, errors.New("define `web.theme` once as a scalar without anchors or aliases on the key or value")
			}
			current = value
		}
	}
	if current == nil {
		webNode.Content = append(webNode.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "theme"},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: theme},
		)
	} else {
		if current.ShortTag() == "!!str" && current.Value == theme {
			return content, nil
		}
		current.Tag = "!!str"
		current.Value = theme
	}
	return yaml.Marshal(doc)
}

func automationYAML(content []byte, key string, spec *automation.Spec) ([]byte, error) {
	doc, err := parseConfigYAML(content)
	if err != nil {
		return nil, err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("config must be a mapping")
	}
	root := doc.Content[0]
	var entries *yaml.Node
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value == "automations" {
			entries = root.Content[i+1]
			break
		}
	}
	if entries == nil {
		for i := 0; i < len(root.Content); i += 2 {
			if root.Content[i].Value == "<<" {
				return nil, errors.New("automation edits cannot shadow a root YAML merge; define automations explicitly first")
			}
		}
		if spec == nil {
			return content, nil
		}
		entries = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "automations"}, entries)
	}
	if entries.Kind != yaml.MappingNode || entries.Anchor != "" {
		return nil, errors.New("automations must be an unaliased mapping")
	}
	// Deleting an explicit entry can expose an inherited entry from a merge anywhere in the mapping.
	for i := 0; i < len(entries.Content); i += 2 {
		if entries.Content[i].Value == "<<" {
			return nil, errors.New("automation edits do not support YAML merges")
		}
	}
	for i := 0; i < len(entries.Content); i += 2 {
		if entries.Content[i].Value != key {
			continue
		}
		if yamlShared(entries.Content[i+1]) {
			return nil, errors.New("automation entry contains an anchor or alias; edit config directly")
		}
		if spec == nil {
			entries.Content = append(entries.Content[:i], entries.Content[i+2:]...)
			return yaml.Marshal(doc)
		}
		var node yaml.Node
		if err := node.Encode(spec); err != nil {
			return nil, err
		}
		preserveYAMLComments(entries.Content[i+1], &node)
		literalPrompt(&node)
		entries.Content[i+1] = &node
		return yaml.Marshal(doc)
	}
	if spec == nil {
		return content, nil
	}
	var node yaml.Node
	if err := node.Encode(spec); err != nil {
		return nil, err
	}
	literalPrompt(&node)
	entries.Content = append(entries.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &node)
	return yaml.Marshal(doc)
}

func preserveYAMLComments(before, after *yaml.Node) {
	after.HeadComment, after.LineComment, after.FootComment = before.HeadComment, before.LineComment, before.FootComment
	if before.Kind != yaml.MappingNode || after.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(after.Content); i += 2 {
		for j := 0; j+1 < len(before.Content); j += 2 {
			if after.Content[i].Value == before.Content[j].Value {
				preserveYAMLComments(before.Content[j], after.Content[i])
				preserveYAMLComments(before.Content[j+1], after.Content[i+1])
				break
			}
		}
	}
}

func yamlShared(n *yaml.Node) bool {
	if n.Kind == yaml.AliasNode || n.Anchor != "" || n.Value == "<<" {
		return true
	}
	return slices.ContainsFunc(n.Content, yamlShared)
}

func literalPrompt(n *yaml.Node) {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == "prompt" {
			n.Content[i+1].Style = yaml.LiteralStyle
		}
	}
}

func (d *Daemon) automationControlDir() (string, error) {
	path, _, _, err := d.configDocument()
	if err != nil {
		return "", err
	}
	root := d.automationRoot
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".kontora", "automations")
	}
	hash := sha256.Sum256([]byte(path))
	dir := filepath.Join(root, fmt.Sprintf("%x", hash), "controls")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}
