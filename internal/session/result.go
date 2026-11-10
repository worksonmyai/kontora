package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	ResultVersion  = 1
	MaxResultBytes = 32 * 1024

	ResultVersionEnvVar    = "KONTORA_RESULT_VERSION"
	InvocationIDEnvVar     = "KONTORA_INVOCATION_ID"
	ResultTicketEnvVar     = "KONTORA_RESULT_TICKET"
	ResultStageEnvVar      = "KONTORA_RESULT_STAGE"
	ResultFileEnvVar       = "KONTORA_RESULT_FILE"
	ResultAutomationEnvVar = "KONTORA_RESULT_AUTOMATION"
	ResultRunNumberEnvVar  = "KONTORA_RESULT_RUN_NUMBER"
)

// Result describes only the assigned stage, not whole-ticket acceptance.
// A blocked result requires a concrete next step; complete may omit it.
type Result struct {
	Outcome  string `json:"outcome"`
	Summary  string `json:"summary"`
	NextStep string `json:"next_step,omitempty"`
}

// Invocation is daemon-owned context for one physical launch. ResultPath must
// be a fresh, absolute artifact path for that invocation, including on retries.
// The result payload cannot override any of this context.
type Invocation struct {
	Version      int    `json:"version"`
	ID           string `json:"id"`
	TicketID     string `json:"ticket_id,omitempty"`
	Stage        string `json:"stage,omitempty"`
	AutomationID string `json:"automation_id,omitempty"`
	RunNumber    uint64 `json:"run_number,omitempty"`
	ResultPath   string `json:"result_path"`
}

// SubjectID is the daemon-bound ticket or automation, never model input.
func (inv Invocation) SubjectID() string {
	if inv.AutomationID != "" {
		return inv.AutomationID
	}
	return inv.TicketID
}

// Env returns the context to overlay AFTER configured agent environment values.
// Empty values clear inherited context belonging to the other subject kind.
func (inv Invocation) Env() map[string]string {
	runNumber := ""
	if inv.RunNumber != 0 {
		runNumber = strconv.FormatUint(inv.RunNumber, 10)
	}
	return map[string]string{
		ResultAutomationEnvVar: inv.AutomationID,
		ResultRunNumberEnvVar:  runNumber,
		ResultVersionEnvVar:    strconv.Itoa(inv.Version),
		InvocationIDEnvVar:     inv.ID,
		ResultTicketEnvVar:     inv.TicketID,
		ResultStageEnvVar:      inv.Stage,
		ResultFileEnvVar:       inv.ResultPath,
	}
}

// InvocationFromEnv fails closed outside an invocation or on incompatible
// versions. It does not infer context from the working directory or ticket store.
func InvocationFromEnv(getenv func(string) string) (Invocation, error) {
	if getenv == nil {
		return Invocation{}, fmt.Errorf("result invocation environment is unavailable")
	}
	if version := getenv(ResultVersionEnvVar); version != strconv.Itoa(ResultVersion) {
		return Invocation{}, fmt.Errorf("%s must be %d, got %q", ResultVersionEnvVar, ResultVersion, version)
	}
	inv := Invocation{Version: ResultVersion, ID: getenv(InvocationIDEnvVar), TicketID: getenv(ResultTicketEnvVar), Stage: getenv(ResultStageEnvVar), AutomationID: getenv(ResultAutomationEnvVar), ResultPath: getenv(ResultFileEnvVar)}
	if !safeInvocationComponent(inv.ID) {
		return Invocation{}, fmt.Errorf("%s must be a nonempty safe identifier", InvocationIDEnvVar)
	}
	run := getenv(ResultRunNumberEnvVar)
	if inv.AutomationID != "" || run != "" {
		if inv.TicketID != "" || inv.Stage != "" {
			return Invocation{}, fmt.Errorf("result invocation cannot mix ticket and automation context")
		}
		if !safeInvocationComponent(inv.AutomationID) {
			return Invocation{}, fmt.Errorf("%s must be a nonempty safe identifier", ResultAutomationEnvVar)
		}
		n, err := strconv.ParseUint(run, 10, 64)
		if err != nil || n == 0 || strconv.FormatUint(n, 10) != run {
			return Invocation{}, fmt.Errorf("%s must be a positive canonical run number", ResultRunNumberEnvVar)
		}
		inv.RunNumber = n
	} else {
		if !safeInvocationComponent(inv.TicketID) {
			return Invocation{}, fmt.Errorf("%s must be a nonempty safe identifier", ResultTicketEnvVar)
		}
		if !validResultText(inv.Stage) || strings.TrimSpace(inv.Stage) == "" || !SafeStage(inv.Stage) {
			return Invocation{}, fmt.Errorf("%s must be a nonblank UTF-8 stage name without NUL or path separators", ResultStageEnvVar)
		}
	}
	if err := validateResultPath(inv.ResultPath); err != nil {
		return Invocation{}, fmt.Errorf("%s: %w", ResultFileEnvVar, err)
	}
	return inv, nil
}

func safeInvocationComponent(s string) bool {
	return validResultText(s) && SafeStage(s) && !strings.ContainsAny(s, `*?[]`) && !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}

func validateResultPath(path string) error {
	if !validResultText(path) || !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Dir(path) == path {
		return fmt.Errorf("result path must be a clean absolute file path")
	}
	return nil
}

func validResultText(s string) bool { return utf8.ValidString(s) && !strings.ContainsRune(s, '\x00') }

func validateResult(result Result) error {
	if result.Outcome != "complete" && result.Outcome != "blocked" {
		return fmt.Errorf("outcome must be complete or blocked")
	}
	if !validResultText(result.Summary) || strings.TrimSpace(result.Summary) == "" {
		return fmt.Errorf("summary must be nonempty UTF-8 without NUL")
	}
	if !validResultText(result.NextStep) {
		return fmt.Errorf("next_step must be UTF-8 without NUL")
	}
	if result.Outcome == "blocked" && strings.TrimSpace(result.NextStep) == "" {
		return fmt.Errorf("blocked result requires next_step")
	}
	return nil
}

// DecodeResult rejects fields that encoding/json would otherwise ignore or normalize.
func DecodeResult(data []byte) (Result, error) {
	if len(data) > MaxResultBytes {
		return Result{}, fmt.Errorf("result exceeds %d bytes", MaxResultBytes)
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return Result{}, fmt.Errorf("result must be UTF-8 without NUL")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	token, err := dec.Token()
	if err != nil {
		return Result{}, fmt.Errorf("decoding result: %w", err)
	}
	if token != json.Delim('{') {
		return Result{}, fmt.Errorf("result must be a JSON object")
	}
	fields := make(map[string]string, 3)
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return Result{}, fmt.Errorf("decoding result key: %w", err)
		}
		key, ok := token.(string)
		if !ok || (key != "outcome" && key != "summary" && key != "next_step") {
			return Result{}, fmt.Errorf("unknown result field %q", token)
		}
		if _, exists := fields[key]; exists {
			return Result{}, fmt.Errorf("duplicate result field %q", key)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return Result{}, fmt.Errorf("decoding %s: %w", key, err)
		}
		if len(raw) == 0 || raw[0] != '"' {
			return Result{}, fmt.Errorf("%s must be a string", key)
		}
		if !validJSONUnicode(raw) {
			return Result{}, fmt.Errorf("%s contains an unpaired Unicode surrogate", key)
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return Result{}, fmt.Errorf("decoding %s: %w", key, err)
		}
		fields[key] = value
	}
	if _, err := dec.Token(); err != nil {
		return Result{}, fmt.Errorf("closing result object: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Result{}, fmt.Errorf("unexpected data after result object")
	}
	result := Result{Outcome: fields["outcome"], Summary: fields["summary"], NextStep: fields["next_step"]}
	if err := validateResult(result); err != nil {
		return Result{}, err
	}
	return result, nil
}

// encoding/json replaces lone UTF-16 surrogates with U+FFFD; reject them rather
// than silently changing submitted text. The input is already syntactically JSON.
func validJSONUnicode(raw []byte) bool {
	for i := 1; i < len(raw)-1; i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if raw[i] != 'u' {
			continue
		}
		n, _ := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		i += 4
		if n >= 0xdc00 && n <= 0xdfff {
			return false
		}
		if n < 0xd800 || n > 0xdbff {
			continue
		}
		if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
			return false
		}
		low, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
		if err != nil || low < 0xdc00 || low > 0xdfff {
			return false
		}
		i += 6
	}
	return true
}

func ReadResult(path string) (Result, error) {
	if err := validateResultPath(path); err != nil {
		return Result{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return Result{}, err
	}
	if !info.Mode().IsRegular() {
		return Result{}, fmt.Errorf("result artifact is not a regular file: %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxResultBytes+1))
	if err != nil {
		return Result{}, err
	}
	return DecodeResult(data)
}

// PublishResult atomically links a fully written private temporary file into
// place, never replacing an existing artifact. duplicate is true only when a
// valid existing artifact has identical decoded fields. A conflicting or invalid
// artifact is an error. This is invocation isolation, not an OS security boundary.
func PublishResult(path string, result Result) (duplicate bool, err error) {
	if err := validateResultPath(path); err != nil {
		return false, err
	}
	if err := validateResult(result); err != nil {
		return false, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(result); err != nil {
		return false, err
	}
	// Omit the encoder's newline so a maximum-size JSON object still fits.
	data := bytes.TrimSuffix(buf.Bytes(), []byte{'\n'})
	if len(data) > MaxResultBytes {
		return false, fmt.Errorf("result exceeds %d bytes", MaxResultBytes)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	f, err := os.CreateTemp(dir, ".result-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return false, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return false, err
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	if err := os.Link(f.Name(), path); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return false, fmt.Errorf("publishing result: %w", err)
		}
		existing, err := ReadResult(path)
		if err != nil {
			return false, fmt.Errorf("reading existing result: %w", err)
		}
		if existing != result {
			return false, fmt.Errorf("conflicting result already published")
		}
		return true, nil
	}
	return false, nil
}
