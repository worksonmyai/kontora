package remote

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/worksonmyai/kontora/internal/automation"
	"github.com/worksonmyai/kontora/internal/web"
)

type AutomationPreviewResponse struct {
	Occurrences []time.Time `json:"occurrences"`
	Human       string      `json:"human"`
}

type AutomationLogsResponse struct {
	Lines []automation.LogLine `json:"lines"`
	Raw   string               `json:"raw"`
}

func (c *Client) Automations() (web.AutomationsResponse, error) {
	var response web.AutomationsResponse
	err := c.doJSON(http.MethodGet, "/api/automations", nil, &response)
	return response, err
}

func (c *Client) Automation(id string) (automation.Definition, error) {
	var row automation.Definition
	err := c.doJSON(http.MethodGet, automationPath(id), nil, &row)
	return row, err
}

func (c *Client) CreateAutomation(requestID, key string, spec automation.Spec) (automation.Definition, error) {
	var row automation.Definition
	err := c.doJSON(http.MethodPost, "/api/automations", map[string]any{"request_id": requestID, "key": key, "definition": spec}, &row)
	return row, err
}

func (c *Client) UpdateAutomation(id string, revision uint64, spec automation.Spec) (automation.Definition, error) {
	var row automation.Definition
	err := c.doJSON(http.MethodPut, automationPath(id), map[string]any{"revision": revision, "definition": spec}, &row)
	return row, err
}

func (c *Client) DeleteAutomation(id string, revision uint64) error {
	return c.doJSON(http.MethodDelete, automationPath(id), map[string]uint64{"revision": revision}, nil)
}

func (c *Client) PauseAutomation(id string, revision uint64) (automation.Definition, error) {
	var row automation.Definition
	err := c.doJSON(http.MethodPost, automationPath(id)+"/pause", map[string]uint64{"revision": revision}, &row)
	return row, err
}

func (c *Client) ResumeAutomation(id string, revision uint64) (automation.Definition, error) {
	var row automation.Definition
	err := c.doJSON(http.MethodPost, automationPath(id)+"/resume", map[string]uint64{"revision": revision}, &row)
	return row, err
}

func (c *Client) RunAutomation(id, requestID string) (automation.Run, error) {
	var run automation.Run
	err := c.doJSON(http.MethodPost, automationPath(id)+"/run", map[string]string{"request_id": requestID}, &run)
	return run, err
}

func (c *Client) StopAutomation(id string, n uint64) (automation.Run, error) {
	var run automation.Run
	err := c.doJSON(http.MethodPost, automationPath(id)+"/stop", map[string]uint64{"n": n}, &run)
	return run, err
}

func (c *Client) AutomationRuns(id string, offset, limit int) (web.AutomationRunsResponse, error) {
	query := url.Values{"offset": {strconv.Itoa(offset)}, "limit": {strconv.Itoa(limit)}}
	var response web.AutomationRunsResponse
	err := c.doJSON(http.MethodGet, automationPath(id)+"/runs?"+query.Encode(), nil, &response)
	return response, err
}

func (c *Client) PreviewAutomation(schedule, timezone string) (AutomationPreviewResponse, error) {
	var response AutomationPreviewResponse
	err := c.doJSON(http.MethodPost, "/api/automations/preview", map[string]string{"schedule": schedule, "timezone": timezone}, &response)
	return response, err
}

func (c *Client) AutomationLogs(id string, n uint64) (AutomationLogsResponse, error) {
	var response AutomationLogsResponse
	err := c.doJSON(http.MethodGet, automationRunPath(id, n)+"/log", nil, &response)
	return response, err
}

func (c *Client) RawAutomationLogs(id string, n uint64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), unaryRequestTimeout)
	defer cancel()
	req, err := c.newRequest(ctx, http.MethodGet, automationRunPath(id, n)+"/log?raw=1", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, &transportError{base: c.base, err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 || (c.localOnly && resp.StatusCode >= 300) {
		return nil, decodeError(resp)
	}
	return io.ReadAll(resp.Body)
}

func (c *Client) AutomationTerminal(id string, n uint64) (web.AutomationTerminalTarget, error) {
	var target web.AutomationTerminalTarget
	err := c.doJSON(http.MethodGet, automationRunPath(id, n)+"/terminal", nil, &target)
	return target, err
}

func automationPath(id string) string {
	return "/api/automations/" + url.PathEscape(id)
}

func automationRunPath(id string, n uint64) string {
	return fmt.Sprintf("%s/runs/%d", automationPath(id), n)
}
