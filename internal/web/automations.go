package web

import "github.com/worksonmyai/kontora/internal/automation"

type AutomationService interface {
	ListAutomations() ([]automation.Definition, error)
	GetAutomation(id string) (automation.Definition, error)
	CreateAutomation(requestID, key string, input automation.Spec) (automation.Definition, error)
	UpdateAutomation(id string, revision uint64, input automation.Spec) (automation.Definition, error)
	DeleteAutomation(id string, revision uint64) error
	RunAutomation(id, requestID string) (automation.Run, error)
	StopAutomation(id string, n uint64) (automation.Run, error)
	AutomationRuns(id string) ([]automation.Run, error)
	AutomationLog(id string, n uint64) ([]automation.LogLine, string, error)
}

type AutomationsResponse struct {
	Available   bool                    `json:"available"`
	Definitions []automation.Definition `json:"definitions"`
	Error       string                  `json:"error,omitempty"`
}

type AutomationRunsResponse struct {
	Runs  []automation.Run `json:"runs"`
	Total int              `json:"total"`
}
