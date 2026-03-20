package ticket

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/worksonmyai/kontora/internal/frontmatter"
	"gopkg.in/yaml.v3"
)

type Status string

const (
	StatusOpen       Status = "open"
	StatusTodo       Status = "todo"
	StatusInProgress Status = "in_progress"
	StatusPaused     Status = "paused"
	StatusDone       Status = "done"
	StatusCancelled  Status = "cancelled"
)

type HistoryEntry struct {
	Stage       string     `yaml:"stage"`
	Agent       string     `yaml:"agent"`
	ExitCode    int        `yaml:"exit_code"`
	StartedAt   *time.Time `yaml:"started_at"`
	CompletedAt *time.Time `yaml:"completed_at"`
}

type Ticket struct {
	ID          string         `yaml:"id"`
	Kontora     bool           `yaml:"kontora"`
	Status      Status         `yaml:"status"`
	Pipeline    string         `yaml:"pipeline"`
	Path        string         `yaml:"path"`
	Agent       string         `yaml:"agent"`
	Role        string         `yaml:"role"`
	Attempt     int            `yaml:"attempt"`
	StartedAt   *time.Time     `yaml:"started_at"`
	CompletedAt *time.Time     `yaml:"completed_at"`
	Branch      string         `yaml:"branch"`
	History     []HistoryEntry `yaml:"history"`
	Created     *time.Time     `yaml:"created"`
	LastError   string         `yaml:"last_error"`

	Body     string `yaml:"-"`
	FilePath string `yaml:"-"`

	rawNode *yaml.Node // preserved for round-trip
	rawBody string     // original body bytes
}

func ParseFile(path string) (*Ticket, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	t, err := ParseBytes(data)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	t.FilePath = path
	return t, nil
}

func Parse(r io.Reader) (*Ticket, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return ParseBytes(data)
}

func ParseBytes(data []byte) (*Ticket, error) {
	yamlStr, body, err := frontmatter.Split(string(data))
	if err != nil {
		return nil, err
	}

	// Parse into yaml.Node for round-trip preservation
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(yamlStr), &doc); err != nil {
		return nil, fmt.Errorf("parsing frontmatter YAML: %w", err)
	}

	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, fmt.Errorf("expected YAML document, got kind %d", doc.Kind)
	}

	// Decode known fields directly from the parsed node (single parse)
	var t Ticket
	if err := doc.Content[0].Decode(&t); err != nil {
		return nil, fmt.Errorf("decoding fields: %w", err)
	}
	t.Body = body
	t.rawNode = doc.Content[0]
	t.rawBody = body

	return &t, nil
}

// Marshal re-encodes the ticket as a frontmatter markdown file.
// The body is preserved byte-for-byte.
func (t *Ticket) Marshal() ([]byte, error) {
	var yamlBuf bytes.Buffer
	enc := yaml.NewEncoder(&yamlBuf)
	enc.SetIndent(2)
	if err := enc.Encode(t.rawNode); err != nil {
		return nil, fmt.Errorf("encoding frontmatter: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("closing encoder: %w", err)
	}

	yamlOut := yamlBuf.String()
	yamlOut = strings.TrimSuffix(yamlOut, "...\n")
	yamlOut = strings.TrimSuffix(yamlOut, "\n")

	var buf bytes.Buffer
	buf.WriteString("---\n")
	buf.WriteString(yamlOut)
	buf.WriteString("\n---\n")
	buf.WriteString(t.rawBody)

	return buf.Bytes(), nil
}

// SetField updates or adds a field in the raw YAML node and syncs typed fields.
func (t *Ticket) SetField(key string, value any) error {
	if t.rawNode == nil || t.rawNode.Kind != yaml.MappingNode {
		return fmt.Errorf("raw node is not a mapping")
	}

	var valNode yaml.Node
	if err := valNode.Encode(value); err != nil {
		return fmt.Errorf("encoding value: %w", err)
	}

	// Search for existing key
	content := t.rawNode.Content
	for i := 0; i < len(content)-1; i += 2 {
		if content[i].Value == key {
			*content[i+1] = valNode
			return t.syncFromNode()
		}
	}

	// Append new key-value pair
	keyNode := &yaml.Node{
		Kind:  yaml.ScalarNode,
		Tag:   "!!str",
		Value: key,
	}
	t.rawNode.Content = append(t.rawNode.Content, keyNode, &valNode)
	return t.syncFromNode()
}

// syncFromNode re-decodes the typed fields from the raw node.
func (t *Ticket) syncFromNode() error {
	if t.rawNode == nil {
		return nil
	}
	// Preserve non-YAML fields
	body := t.Body
	filePath := t.FilePath
	rawNode := t.rawNode
	rawBody := t.rawBody

	err := t.rawNode.Decode(t)
	t.Body = body
	t.FilePath = filePath
	t.rawNode = rawNode
	t.rawBody = rawBody
	return err
}

// SetBody replaces the ticket body content.
func (t *Ticket) SetBody(body string) {
	t.Body = body
	t.rawBody = body
}

// AppendNote adds a timestamped note to the body under a ## Notes section.
func (t *Ticket) AppendNote(text string, ts time.Time) {
	if !strings.Contains(t.Body, "\n## Notes\n") && !strings.HasPrefix(t.Body, "## Notes\n") {
		t.Body += "\n## Notes\n"
	}
	t.Body += fmt.Sprintf("\n**%s**\n\n%s\n", ts.UTC().Format(time.RFC3339), text)
	t.rawBody = t.Body
}

// Title returns the first # heading from the body.
func (t *Ticket) Title() string {
	for line := range strings.SplitSeq(t.Body, "\n") {
		line = strings.TrimSpace(line)
		if title, ok := strings.CutPrefix(line, "# "); ok {
			return title
		}
	}
	return ""
}
