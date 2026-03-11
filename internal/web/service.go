package web

import (
	"errors"
	"time"
)

var (
	ErrTicketNotFound = errors.New("ticket not found")
	ErrInvalidState   = errors.New("invalid state transition")
	ErrLogNotFound    = errors.New("log not found")
	ErrUnknownAgent   = errors.New("unknown agent")
	ErrDeleteRejected = errors.New("delete rejected")
)

// TicketService defines the contract between the web layer and the daemon.
// The daemon implements this interface; the web package tests use mocks.
type TicketService interface {
	ListTickets() []TicketInfo
	RunningAgents() int
	GetTicket(id string) (TicketInfo, error)
	CreateTicket(req CreateTicketRequest) (TicketInfo, error)
	GetConfig() ConfigInfo
	DeleteTicket(id string) error
	PauseTicket(id string) error
	RetryTicket(id string) error
	SkipStage(id string) error
	SetStage(id string, stage string) error
	MoveTicket(id string, newStatus string) error
	InitTicket(id string, req InitTicketRequest) error
	UpdateTicket(id string, req UpdateTicketRequest) error
	UploadTicket(content []byte) (TicketInfo, error)
	GetLogs(id string, stage string) (string, error)
	Subscribe() (ch <-chan TicketEvent, unsubscribe func())
	HasTerminalSession(id string) bool
}

type CreateTicketRequest struct {
	Title    string `json:"title"`
	Path     string `json:"path"`
	Pipeline string `json:"pipeline,omitempty"`
	Agent    string `json:"agent,omitempty"`
	Status   string `json:"status,omitempty"`
	Body     string `json:"body,omitempty"`
}

type InitTicketRequest struct {
	Pipeline string `json:"pipeline"`
	Path     string `json:"path"`
	Agent    string `json:"agent,omitempty"`
}

type UpdateTicketRequest struct {
	Body     *string `json:"body,omitempty"`
	Pipeline *string `json:"pipeline,omitempty"`
	Path     *string `json:"path,omitempty"`
	Agent    *string `json:"agent,omitempty"`
}

type PipelineInfo struct {
	Name   string   `json:"name"`
	Stages []string `json:"stages"`
}

type ConfigInfo struct {
	Pipelines     []string       `json:"pipelines"`
	PipelineInfos []PipelineInfo `json:"pipeline_infos"`
	Agents        []string       `json:"agents"`
}

type TicketInfo struct {
	ID            string        `json:"id"`
	Title         string        `json:"title"`
	Status        string        `json:"status"`
	Kontora       bool          `json:"kontora"`
	Stage         string        `json:"stage"`
	Pipeline      string        `json:"pipeline"`
	Path          string        `json:"path"`
	Agent         string        `json:"agent"`
	AgentOverride bool          `json:"agent_override,omitempty"`
	Attempt       int           `json:"attempt"`
	CreatedAt     *time.Time    `json:"created_at,omitempty"`
	StartedAt     *time.Time    `json:"started_at,omitempty"`
	Branch        string        `json:"branch,omitempty"`
	Stages        []string      `json:"stages,omitempty"`
	History       []HistoryInfo `json:"history,omitempty"`
	Body          string        `json:"body,omitempty"`
	LastError     string        `json:"last_error,omitempty"`
}

type HistoryInfo struct {
	Stage       string     `json:"stage"`
	Agent       string     `json:"agent,omitempty"`
	ExitCode    int        `json:"exit_code"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

type TicketEvent struct {
	Type   string     `json:"type"`
	Ticket TicketInfo `json:"ticket"`
}
