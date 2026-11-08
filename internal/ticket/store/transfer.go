package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/worksonmyai/kontora/internal/frontmatter"
	"github.com/worksonmyai/kontora/internal/ticket"
	"gopkg.in/yaml.v3"
)

const transferDirectory = ".kontora-transfers"
const captureDirectory = ".kontora-transfer-captures"

type TransferRequest struct {
	SourceDir      string
	DestinationDir string
	ID             string
	Revision       string
	Validate       func(*ticket.Ticket) error
}

type transferRecord struct {
	ID             string `json:"id"`
	SourceDir      string `json:"source_dir"`
	DestinationDir string `json:"destination_dir"`
	Token          string `json:"token"`
	MarkdownHash   string `json:"markdown_hash"`
	ReactionsHash  string `json:"reactions_hash,omitempty"`
	PublishedHash  string `json:"published_hash"`
	Committed      bool   `json:"committed"`
}

// Revision includes the absence of a reaction sidecar, not just its contents.
func Revision(dir, id string) (string, error) {
	if !ticket.IsSafeID(id) {
		return "", fmt.Errorf("unsafe ticket id %q", id)
	}
	md, _, err := readTransferFile(filepath.Join(dir, id+".md"), false)
	if err != nil {
		return "", err
	}
	reactions, exists, err := readTransferFile(filepath.Join(dir, id+".notes.json"), true)
	if err != nil {
		return "", err
	}
	return revisionOf(md, reactions, exists), nil
}

func revisionOf(md, reactions []byte, exists bool) string {
	return digest([]byte(fmt.Sprintf("%s:%t:%s", digest(md), exists, digest(reactions))))
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Transfer takes no daemon mutex. Validate runs under both board locks and routing admissions.
func Transfer(req TransferRequest) (*ticket.Ticket, error) {
	return transfer(req, nil)
}

// A per-call checkpoint lets tests interrupt durable steps without shared hooks.
func transfer(req TransferRequest, checkpoint func(string) error) (*ticket.Ticket, error) {
	if !ticket.IsSafeID(req.ID) || req.Revision == "" {
		return nil, fmt.Errorf("safe ticket id and reviewed revision are required")
	}
	source, err := transferIdentity(req.SourceDir)
	if err != nil {
		return nil, err
	}
	destination, err := transferIdentity(req.DestinationDir)
	if err != nil {
		return nil, err
	}
	var result *ticket.Ticket
	err = withTransferLocks(source, destination, func() error {
		for _, dir := range []string{source, destination} {
			pending, err := HasPendingTransfer(dir, req.ID)
			if err != nil {
				return err
			}
			if pending {
				return fmt.Errorf("ticket %s has a pending transfer in %s; run recovery", req.ID, dir)
			}
		}
		md, _, err := readTransferFile(filepath.Join(source, req.ID+".md"), false)
		if err != nil {
			return err
		}
		reactions, hasReactions, err := readTransferFile(filepath.Join(source, req.ID+".notes.json"), true)
		if err != nil {
			return err
		}
		if revisionOf(md, reactions, hasReactions) != req.Revision {
			return fmt.Errorf("ticket %s revision changed", req.ID)
		}
		t, out, err := prepareTransferTicket(req, md, destination)
		if err != nil {
			return err
		}
		root := filepath.Join(destination, transferDirectory)
		if err := makePrivateDirectory(root); err != nil {
			return err
		}
		txn, err := os.MkdirTemp(root, "transfer-"+hex.EncodeToString([]byte(req.ID))+"-")
		if err != nil {
			return err
		}
		r := transferRecord{ID: req.ID, SourceDir: source, DestinationDir: destination,
			Token: filepath.Base(txn), MarkdownHash: digest(md), PublishedHash: digest(out)}
		if hasReactions {
			r.ReactionsHash = digest(reactions)
		}
		capture := r.capturePath()
		if err := makePrivateDirectory(filepath.Dir(capture)); err != nil {
			return err
		}
		if err := os.Mkdir(capture, 0o700); err != nil {
			return err
		}
		// Before the record exists, neither board has lost a ticket entry.
		recorded := false
		defer func() {
			if !recorded {
				_ = os.RemoveAll(txn)
				_ = os.Remove(capture)
			}
		}()
		if err := writeTransferFile(filepath.Join(txn, "markdown"), out); err != nil {
			return err
		}
		if hasReactions {
			if err := writeTransferFile(filepath.Join(txn, "reactions"), reactions); err != nil {
				return err
			}
		}
		if err := syncTransferDirs(root, filepath.Dir(capture), capture); err != nil {
			return err
		}
		if err := saveTransferRecord(txn, r); err != nil {
			return err
		}
		recorded = true
		if err := finishTransfer(txn, &r, checkpoint); err != nil {
			return fmt.Errorf("transfer %s requires recovery at %s: %w", req.ID, txn, err)
		}
		t.FilePath = filepath.Join(destination, req.ID+".md")
		result = t
		return nil
	})
	return result, err
}

func prepareTransferTicket(req TransferRequest, md []byte, destination string) (*ticket.Ticket, []byte, error) {
	t, err := ticket.ParseBytes(md)
	if err != nil {
		return nil, nil, err
	}
	if t.ID != req.ID {
		return nil, nil, fmt.Errorf("ticket id does not match source filename")
	}
	if t.Status == ticket.StatusInProgress || t.InvocationID != "" || t.AnnotationReturnStatus != "" {
		return nil, nil, fmt.Errorf("ticket %s has running, invocation, or annotation state", req.ID)
	}
	if err := ticket.RejectImportedOwnership(t); err != nil {
		return nil, nil, err
	}
	for _, suffix := range []string{".md", ".notes.json"} {
		if err := requireAbsent(filepath.Join(destination, req.ID+suffix)); err != nil {
			return nil, nil, err
		}
	}
	if req.Validate != nil {
		if err := req.Validate(t.Clone()); err != nil {
			return nil, nil, err
		}
	}
	out, err := normalizeTransfer(t, md)
	return t, out, err
}

func normalizeTransfer(t *ticket.Ticket, md []byte) ([]byte, error) {
	fm, body, err := frontmatter.Split(string(md))
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(fm), &doc); err != nil {
		return nil, err
	}
	root := doc.Content[0]
	var expected map[string]any
	if err := root.Decode(&expected); err != nil {
		return nil, err
	}
	var fields map[string]yaml.Node
	if err := root.Decode(&fields); err != nil {
		return nil, err
	}
	for _, field := range []string{"stage", "attempt", "started_at", "completed_at", "claimed_by", "invocation_id",
		"scheduled_at", "annotation_return_status", "archived_from", "archived_at", "archived_by", "archive_note", "last_error", "last_log"} {
		delete(fields, field)
		delete(expected, field)
	}
	for key, value := range map[string]any{"status": string(ticket.StatusOpen), "imported_history_count": len(t.History)} {
		var node yaml.Node
		if err := node.Encode(value); err != nil {
			return nil, err
		}
		fields[key] = node
		expected[key] = value
	}
	// Materialize root merges so deleted lifecycle keys cannot be inherited again.
	content := root.Content
	root.Content = nil
	for i := 0; i < len(content); i += 2 {
		key := content[i]
		if key.Tag == "!!merge" {
			continue
		}
		if value, ok := fields[key.Value]; ok {
			root.Content = append(root.Content, key, &value)
			delete(fields, key.Value)
		}
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := fields[key]
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &value)
	}
	if err := repairTransferAliases(root, make(map[string]bool), make(map[*yaml.Node]bool), false); err != nil {
		return nil, err
	}
	var yamlOut bytes.Buffer
	enc := yaml.NewEncoder(&yamlOut)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	out := []byte("---\n" + yamlOut.String() + "---\n" + body)
	normalized, err := ticket.ParseBytes(out)
	if err != nil {
		return nil, fmt.Errorf("invalid normalized ticket: %w", err)
	}
	var actual map[string]any
	if err := yaml.Unmarshal(yamlOut.Bytes(), &actual); err != nil {
		return nil, fmt.Errorf("invalid normalized fields: %w", err)
	}
	expectedFields, err := yaml.Marshal(expected)
	if err != nil {
		return nil, err
	}
	actualFields, err := yaml.Marshal(actual)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(expectedFields, actualFields) {
		return nil, fmt.Errorf("normalized ticket changed preserved fields")
	}
	*t = *normalized
	return out, nil
}

// Aliases to removed or replaced anchors must retain their original values.
func repairTransferAliases(node *yaml.Node, anchors map[string]bool, active map[*yaml.Node]bool, expand bool) error {
	if active[node] {
		return fmt.Errorf("cyclic YAML alias in transfer")
	}
	active[node] = true
	defer delete(active, node)
	if node.Kind == yaml.AliasNode {
		if !expand && anchors[node.Value] {
			return nil
		}
		if active[node.Alias] {
			return fmt.Errorf("cyclic YAML alias in transfer")
		}
		active[node.Alias] = true
		defer delete(active, node.Alias)
		resolved := *node.Alias
		if err := repairTransferAliases(&resolved, anchors, active, true); err != nil {
			return err
		}
		*node = resolved
		return nil
	}
	if expand {
		node.Anchor = ""
		if node.Kind == yaml.ScalarNode && node.Tag != "!!str" {
			node.Style |= yaml.TaggedStyle
		}
	} else if node.Anchor != "" {
		anchors[node.Anchor] = true
	}
	content := make([]*yaml.Node, len(node.Content))
	for i, child := range node.Content {
		cloned := *child
		if err := repairTransferAliases(&cloned, anchors, active, expand); err != nil {
			return err
		}
		content[i] = &cloned
	}
	node.Content = content
	return nil
}

func transferIdentity(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", fmt.Errorf("transfer board is not a directory: %s", resolved)
	}
	return resolved, nil
}

func withTransferLocks(source, destination string, fn func() error) error {
	src, err := os.Stat(source)
	if err != nil {
		return err
	}
	dst, err := os.Stat(destination)
	if err != nil {
		return err
	}
	if source == destination || os.SameFile(src, dst) {
		return fmt.Errorf("source and destination are the same board")
	}
	first, second := source, destination
	if second < first {
		first, second = second, first
	}
	return ticket.WithBoardMutation(first, func() error {
		return ticket.WithBoardMutation(second, func() error {
			releaseSource, err := ticket.LockTransferSource(source)
			if err != nil {
				return err
			}
			defer releaseSource()
			releaseDestination, err := ticket.LockOrdinaryRouting(destination)
			if err != nil {
				return err
			}
			defer releaseDestination()
			return fn()
		})
	})
}

func requireCapturedSourceAbsent(r transferRecord) error {
	for _, suffix := range []string{".md", ".notes.json"} {
		if err := requireAbsent(filepath.Join(r.SourceDir, r.ID+suffix)); err != nil {
			return fmt.Errorf("source recreated during transfer: %w", err)
		}
	}
	return nil
}

func (r transferRecord) capturePath() string {
	return filepath.Join(r.SourceDir, captureDirectory, r.Token)
}

func finishTransfer(txn string, r *transferRecord, checkpoint func(string) error) error {
	step := func(name string) error {
		if checkpoint != nil {
			return checkpoint(name)
		}
		return nil
	}
	if !r.Committed {
		if err := step("prepared"); err != nil {
			return err
		}
		for _, entry := range []struct{ source, backup, hash string }{
			{r.ID + ".md", "markdown", r.MarkdownHash},
			{r.ID + ".notes.json", "reactions", r.ReactionsHash},
		} {
			source := filepath.Join(r.SourceDir, entry.source)
			backup := filepath.Join(r.capturePath(), entry.backup)
			if entry.hash == "" {
				if err := requireAbsent(source); err != nil {
					return err
				}
				continue
			}
			_, exists, err := readTransferFile(backup, true)
			if err != nil {
				return err
			}
			if !exists {
				if err := verifyTransferHash(source, entry.hash); err != nil {
					return err
				}
				if err := os.Rename(source, backup); err != nil {
					return err
				}
			}
			if err := verifyTransferHash(backup, entry.hash); err != nil {
				return err
			}
			if err := requireAbsent(source); err != nil {
				return fmt.Errorf("source recreated during transfer: %w", err)
			}
			if err := syncTransferDirs(r.SourceDir, r.capturePath()); err != nil {
				return err
			}
			if err := step("captured-" + entry.backup); err != nil {
				return err
			}
		}
		for _, entry := range []struct{ suffix, backup, hash string }{
			{".md", "markdown", r.MarkdownHash},
			{".notes.json", "reactions", r.ReactionsHash},
		} {
			if err := requireAbsent(filepath.Join(r.SourceDir, r.ID+entry.suffix)); err != nil {
				return fmt.Errorf("source recreated during capture: %w", err)
			}
			if entry.hash != "" {
				if err := verifyTransferHash(filepath.Join(r.capturePath(), entry.backup), entry.hash); err != nil {
					return err
				}
			}
		}
		// Publish reactions first so a newly visible markdown file is complete.
		if r.ReactionsHash != "" {
			if err := publishTransferEntry(filepath.Join(txn, "reactions"), filepath.Join(r.DestinationDir, r.ID+".notes.json"), r.ReactionsHash); err != nil {
				return err
			}
		} else if err := requireAbsent(filepath.Join(r.DestinationDir, r.ID+".notes.json")); err != nil {
			return err
		}
		if err := syncTransferDirs(r.DestinationDir); err != nil {
			return err
		}
		if err := step("published-reactions"); err != nil {
			return err
		}
		if err := requireCapturedSourceAbsent(*r); err != nil {
			return err
		}
		if err := publishTransferEntry(filepath.Join(txn, "markdown"), filepath.Join(r.DestinationDir, r.ID+".md"), r.PublishedHash); err != nil {
			return err
		}
		if err := syncTransferDirs(r.DestinationDir); err != nil {
			return err
		}
		if err := step("published-markdown"); err != nil {
			return err
		}
		if err := requireCapturedSourceAbsent(*r); err != nil {
			return err
		}
		r.Committed = true
		if err := saveTransferRecord(txn, *r); err != nil {
			return err
		}
	}
	return cleanupTransfer(txn, r, step)
}

func cleanupTransfer(txn string, r *transferRecord, step func(string) error) error {
	if err := step("committed"); err != nil {
		return err
	}
	if err := verifyTransferHash(filepath.Join(r.DestinationDir, r.ID+".md"), r.PublishedHash); err != nil {
		return err
	}
	if r.ReactionsHash != "" {
		if err := verifyTransferHash(filepath.Join(r.DestinationDir, r.ID+".notes.json"), r.ReactionsHash); err != nil {
			return err
		}
	}
	// Committed records only clean private backups. A recreated source is never removed.
	for _, name := range []string{"markdown", "reactions"} {
		if err := removeTransferFile(filepath.Join(r.capturePath(), name)); err != nil {
			return err
		}
	}
	if err := os.Remove(r.capturePath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := syncTransferDirs(filepath.Dir(r.capturePath())); err != nil {
		return err
	}
	for _, name := range []string{"markdown", "reactions"} {
		if err := removeTransferFile(filepath.Join(txn, name)); err != nil {
			return err
		}
	}
	if err := syncTransferDirs(txn); err != nil {
		return err
	}
	if err := removeTransferFile(filepath.Join(txn, "record.json")); err != nil {
		return err
	}
	if err := os.Remove(txn); err != nil {
		return err
	}
	return syncTransferDirs(filepath.Dir(txn))
}

// RecoverTransfers must run before destination discovery. Errors retain backups and block admission by ID.
func RecoverTransfers(destinationDir string) (map[string]bool, error) {
	blocked := make(map[string]bool)
	destination, err := transferIdentity(destinationDir)
	if err != nil {
		return blocked, err
	}
	root := filepath.Join(destination, transferDirectory)
	if err := checkTransferDirectory(root); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return blocked, nil
		}
		return blocked, err
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return blocked, nil
	}
	if err != nil {
		return blocked, err
	}
	var errs []error
	for _, entry := range entries {
		txn := filepath.Join(root, entry.Name())
		id := transferTokenID(entry.Name())
		if _, err := os.Lstat(filepath.Join(txn, "record.json")); errors.Is(err, os.ErrNotExist) {
			continue // Staging stopped before the source was captured.
		} else if err != nil {
			if id != "" {
				blocked[id] = true
			}
			errs = append(errs, fmt.Errorf("read transfer %s: %w", txn, err))
			continue
		}
		r, err := loadTransferRecord(txn, destination)
		if err != nil {
			if id != "" {
				blocked[id] = true
			}
			errs = append(errs, fmt.Errorf("read transfer %s: %w", txn, err))
			continue
		}
		err = withTransferLocks(r.SourceDir, destination, func() error {
			// Another recovery may have finished while this caller waited for locks.
			if _, err := os.Lstat(filepath.Join(txn, "record.json")); errors.Is(err, os.ErrNotExist) {
				return nil
			} else if err != nil {
				return err
			}
			fresh, err := loadTransferRecord(txn, destination)
			if err != nil {
				return err
			}
			return finishTransfer(txn, &fresh, nil)
		})
		if err != nil {
			blocked[r.ID] = true
			errs = append(errs, fmt.Errorf("recover transfer %s at %s: %w", r.ID, txn, err))
		}
	}
	return blocked, errors.Join(errs...)
}

func loadTransferRecord(txn, destination string) (transferRecord, error) {
	var r transferRecord
	st, err := os.Lstat(txn)
	if err != nil {
		return r, err
	}
	if !st.IsDir() {
		return r, fmt.Errorf("unsafe transfer directory %s", txn)
	}
	data, _, err := readTransferFile(filepath.Join(txn, "record.json"), false)
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return r, err
	}
	if !ticket.IsSafeID(r.ID) || r.DestinationDir != destination || r.SourceDir == destination ||
		!filepath.IsAbs(r.SourceDir) || filepath.Clean(r.SourceDir) != r.SourceDir ||
		r.Token != filepath.Base(txn) || transferTokenID(r.Token) != r.ID ||
		len(r.MarkdownHash) != 64 || len(r.PublishedHash) != 64 || (r.ReactionsHash != "" && len(r.ReactionsHash) != 64) {
		return r, fmt.Errorf("invalid transfer record %s", txn)
	}
	if source, err := transferIdentity(r.SourceDir); err != nil || source != r.SourceDir {
		return r, fmt.Errorf("source identity changed for %s", txn)
	}
	for _, dir := range []string{filepath.Join(r.SourceDir, captureDirectory), r.capturePath()} {
		if err := checkTransferDirectory(dir); err != nil && (!r.Committed || !errors.Is(err, os.ErrNotExist)) {
			return r, err
		}
	}
	return r, nil
}

func transferTokenID(token string) string {
	rest, ok := strings.CutPrefix(token, "transfer-")
	if !ok {
		return ""
	}
	encoded, _, ok := strings.Cut(rest, "-")
	if !ok {
		return ""
	}
	decoded, err := hex.DecodeString(encoded)
	if err != nil || !ticket.IsSafeID(string(decoded)) {
		return ""
	}
	return string(decoded)
}

func checkTransferDirectory(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("unsafe transfer directory %s", path)
	}
	return nil
}

func saveTransferRecord(txn string, r transferRecord) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	path := filepath.Join(txn, "record.json")
	if err := writeTransferFile(path+".tmp", data); err != nil {
		return err
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return err
	}
	return syncTransferDirs(txn, filepath.Dir(txn))
}

func publishTransferEntry(stage, destination, hash string) error {
	if err := verifyTransferHash(stage, hash); err != nil {
		return err
	}
	if err := os.Link(stage, destination); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		staged, stageErr := os.Lstat(stage)
		published, publishedErr := os.Lstat(destination)
		if stageErr != nil || publishedErr != nil || !published.Mode().IsRegular() || !os.SameFile(staged, published) {
			return fmt.Errorf("destination collision at %s", destination)
		}
	}
	return nil
}

func readTransferFile(path string, optional bool) ([]byte, bool, error) {
	st, err := os.Lstat(path)
	if optional && errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !st.Mode().IsRegular() {
		return nil, false, fmt.Errorf("unsafe nonregular transfer file %s", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(st, opened) {
		return nil, false, fmt.Errorf("transfer file changed while opening %s", path)
	}
	data, err := io.ReadAll(f)
	return data, true, err
}

func verifyTransferHash(path, hash string) error {
	data, _, err := readTransferFile(path, false)
	if err != nil {
		return err
	}
	if digest(data) != hash {
		return fmt.Errorf("transfer contents changed at %s; retain both versions for recovery", path)
	}
	return nil
}

func requireAbsent(path string) error {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err == nil {
		return fmt.Errorf("destination or source entry already exists: %s: %w", path, os.ErrExist)
	}
	return err
}

func writeTransferFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}

func makePrivateDirectory(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("unsafe transfer directory %s", path)
	}
	return syncTransferDirs(filepath.Dir(path))
}

func syncTransferDirs(paths ...string) error {
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		err = f.Sync()
		_ = f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func removeTransferFile(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
