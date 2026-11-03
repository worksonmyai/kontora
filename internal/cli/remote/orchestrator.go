package remote

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/worksonmyai/kontora/internal/web"
)

func (c *Client) WithOperatorToken(token string) *Client {
	client := *c
	client.operatorToken = token
	return &client
}

func (c *Client) Objectives() (web.ObjectiveList, error) {
	var result web.ObjectiveList
	err := c.doJSON(http.MethodGet, "/api/objectives", nil, &result)
	return result, err
}

func (c *Client) Objective(id string) (web.ObjectiveSnapshot, error) {
	if !web.ValidObjectiveID(id) {
		return web.ObjectiveSnapshot{}, fmt.Errorf("invalid objective id")
	}
	var result web.ObjectiveSnapshot
	err := c.doJSON(http.MethodGet, "/api/objectives/"+url.PathEscape(id), nil, &result)
	return result, err
}

// ControlObjective never falls back to ticket files or retries a stale mutation.
func (c *Client) ControlObjective(request web.ObjectiveControlRequest) (web.ObjectiveControlResponse, error) {
	if c.operatorToken == "" || request.RequestID == "" || request.BoardID == "" || !web.ValidObjectiveID(request.ObjectiveID) {
		return web.ObjectiveControlResponse{}, fmt.Errorf("operator credential, board, request, and objective identities are required")
	}
	path := "/api/objectives/" + url.PathEscape(request.ObjectiveID) + "/controls"
	if request.Operation == "assign" {
		path = "/api/objectives"
	}
	var result web.ObjectiveControlResponse
	err := c.doJSON(http.MethodPost, path, request, &result)
	return result, err
}
