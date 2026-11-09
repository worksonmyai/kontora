package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/worksonmyai/kontora/internal/ticket"
)

// SecondaryTicketService is optional. Each mutation must hold source routing
// admission for the complete operation, including reaction sidecar writes.
// Mutations return their post-write projection to avoid another source scan.
type SecondaryTicketService interface {
	ListTicketSources() []TicketSourceInfo
	ListSecondaryTickets(source string) ([]TicketInfo, error)
	GetSecondaryTicket(source, id string) (TicketInfo, error)
	UpdateSecondaryTicket(source, id string, req UpdateTicketRequest) (TicketInfo, error)
	ClaimSecondaryTicket(source, id, revision string) (TicketInfo, error)
	MutateSecondaryTicket(source, id string, op SecondaryMutation) (TicketInfo, error)
}

type TicketSourceInfo struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	Available bool   `json:"available"`
	Error     string `json:"error"`
}

type SecondaryMutation struct {
	Action     string
	NoteID     string
	Text       string
	Author     string
	Parent     string
	Emoji      string
	Actor      string
	On         bool
	RelatedIDs []string
	Children   []string
}

const (
	SecondaryAddNote          = "add_note"
	SecondaryEditNote         = "edit_note"
	SecondaryDeleteNote       = "delete_note"
	SecondarySetReaction      = "set_reaction"
	SecondarySetSummary       = "set_summary"
	SecondaryAddDependency    = "add_dependency"
	SecondaryRemoveDependency = "remove_dependency"
	SecondaryLink             = "link"
	SecondaryUnlink           = "unlink"
	SecondarySetParent        = "set_parent"
	SecondaryClearParent      = "clear_parent"
	SecondarySetChildOrder    = "set_child_order"
)

var (
	ErrTicketSourceNotFound      = fmt.Errorf("%w: source not found", ErrTicketNotFound)
	ErrTicketSourceUnavailable   = errors.New("ticket source unavailable")
	ErrSecondaryOperationRefused = errors.New("operation is not allowed for secondary tickets")
)

func (s *Server) registerSecondaryRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/ticket-sources", s.handleTicketSources)
	mux.HandleFunc("GET /api/sources/{source}/tickets", s.handleSecondaryList)
	mux.HandleFunc("GET /api/sources/{source}/tickets/{id}", s.handleSecondaryGet)
	mux.HandleFunc("PUT /api/sources/{source}/tickets/{id}", s.handleSecondaryUpdate)
	mux.HandleFunc("POST /api/sources/{source}/tickets/{id}/claim", s.handleSecondaryClaim)
	for pattern, action := range map[string]string{
		"POST /api/sources/{source}/tickets/{id}/note":                               SecondaryAddNote,
		"PATCH /api/sources/{source}/tickets/{id}/notes/{noteID}":                    SecondaryEditNote,
		"DELETE /api/sources/{source}/tickets/{id}/notes/{noteID}":                   SecondaryDeleteNote,
		"POST /api/sources/{source}/tickets/{id}/notes/{noteID}/reactions":           SecondarySetReaction,
		"DELETE /api/sources/{source}/tickets/{id}/notes/{noteID}/reactions/{emoji}": SecondarySetReaction,
		"POST /api/sources/{source}/tickets/{id}/summary":                            SecondarySetSummary,
		"POST /api/sources/{source}/tickets/{id}/dep":                                SecondaryAddDependency,
		"POST /api/sources/{source}/tickets/{id}/undep":                              SecondaryRemoveDependency,
		"POST /api/sources/{source}/tickets/{id}/link":                               SecondaryLink,
		"POST /api/sources/{source}/tickets/{id}/unlink":                             SecondaryUnlink,
		"POST /api/sources/{source}/tickets/{id}/parent":                             SecondarySetParent,
		"POST /api/sources/{source}/tickets/{id}/unparent":                           SecondaryClearParent,
		"PUT /api/sources/{source}/tickets/{id}/children":                            SecondarySetChildOrder,
	} {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			s.handleSecondaryMutation(w, r, action)
		})
	}
	// The static handler accepts every GET path. Reserve this whole namespace
	// so unsupported secondary requests cannot reach it or primary handlers.
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace} {
		mux.HandleFunc(method+" /api/sources/{source}/{rest...}", s.refuseSecondaryRoute)
		mux.HandleFunc(method+" /api/sources/{source}", s.refuseSecondaryRoute)
		mux.HandleFunc(method+" /api/sources/", s.refuseSecondaryRoute)
		mux.HandleFunc(method+" /api/sources", s.refuseSecondaryRoute)
		mux.HandleFunc(method+" /api/ticket-sources/", s.refuseSecondaryRoute)
		if method != http.MethodGet {
			mux.HandleFunc(method+" /api/ticket-sources", s.refuseSecondaryRoute)
		}
	}
}

func (s *Server) handleTicketSources(w http.ResponseWriter, _ *http.Request) {
	var sources []TicketSourceInfo
	if svc, ok := s.svc.(SecondaryTicketService); ok {
		sources = svc.ListTicketSources()
	}
	if sources == nil {
		sources = []TicketSourceInfo{}
	}
	writeJSON(w, http.StatusOK, sources)
}

func (s *Server) secondaryService(w http.ResponseWriter, r *http.Request) (SecondaryTicketService, bool) {
	svc, ok := s.svc.(SecondaryTicketService)
	if ok {
		for _, source := range svc.ListTicketSources() {
			if source.ID != r.PathValue("source") {
				continue
			}
			if !source.Available {
				writeSecondaryError(w, ErrTicketSourceUnavailable)
				return nil, false
			}
			return svc, true
		}
	}
	writeSecondaryError(w, ErrTicketSourceNotFound)
	return nil, false
}

func (s *Server) refuseSecondaryRoute(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("source") != "" {
		if _, ok := s.secondaryService(w, r); !ok {
			return
		}
	}
	writeSecondaryError(w, ErrSecondaryOperationRefused)
}

func (s *Server) handleSecondaryList(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.secondaryService(w, r)
	if !ok {
		return
	}
	tickets, err := svc.ListSecondaryTickets(r.PathValue("source"))
	if err != nil {
		writeSecondaryError(w, err)
		return
	}
	if tickets == nil {
		tickets = []TicketInfo{}
	}
	writeJSON(w, http.StatusOK, tickets)
}

func (s *Server) handleSecondaryGet(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.secondaryService(w, r)
	if ok {
		writeSecondaryTicket(w, svc, r.PathValue("source"), r.PathValue("id"))
	}
}

func writeSecondaryTicket(w http.ResponseWriter, svc SecondaryTicketService, source, id string) {
	tkt, err := svc.GetSecondaryTicket(source, id)
	if err != nil {
		writeSecondaryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tkt)
}

func (s *Server) handleSecondaryUpdate(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.secondaryService(w, r)
	if !ok {
		return
	}
	var req UpdateTicketRequest
	if err := decodeSecondaryJSON(r, &req, false); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateSecondaryUpdate(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	source, id := r.PathValue("source"), r.PathValue("id")
	tkt, err := svc.UpdateSecondaryTicket(source, id, req)
	if err != nil {
		writeSecondaryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tkt)
}

func validateSecondaryUpdate(req *UpdateTicketRequest) error {
	if err := req.ValidateDescription(); err != nil {
		return err
	}
	for _, field := range []struct {
		name  string
		value *string
	}{{"pipeline", req.Pipeline}, {"path", req.Path}, {"agent", req.Agent}} {
		if field.value != nil && containsNewline(*field.value) {
			return fmt.Errorf("%s must not contain newlines", field.name)
		}
	}
	for _, field := range []struct {
		name  string
		value *string
	}{{"branch", req.Branch}, {"base branch", req.BaseBranch}} {
		if field.value != nil {
			*field.value = strings.TrimSpace(*field.value)
			if *field.value != "" && !validBranchName(*field.value) {
				return fmt.Errorf("invalid %s name", field.name)
			}
		}
	}
	return nil
}

func (s *Server) handleSecondaryClaim(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.secondaryService(w, r)
	if !ok {
		return
	}
	var req struct {
		Revision string `json:"revision"`
	}
	if err := decodeSecondaryJSON(r, &req, false); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Revision) == "" {
		writeJSONError(w, http.StatusBadRequest, "revision is required")
		return
	}
	tkt, err := svc.ClaimSecondaryTicket(r.PathValue("source"), r.PathValue("id"), req.Revision)
	if err != nil {
		writeSecondaryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tkt)
}

func (s *Server) handleSecondaryMutation(w http.ResponseWriter, r *http.Request, action string) {
	svc, ok := s.secondaryService(w, r)
	if !ok {
		return
	}
	op, err := decodeSecondaryMutation(r, action)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	source, id := r.PathValue("source"), r.PathValue("id")
	tkt, err := svc.MutateSecondaryTicket(source, id, op)
	if err != nil {
		writeSecondaryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tkt)
}

func decodeSecondaryMutation(r *http.Request, action string) (SecondaryMutation, error) {
	op := SecondaryMutation{Action: action, NoteID: r.PathValue("noteID")}
	switch action {
	case SecondaryAddNote:
		var req AddNoteRequest
		if err := decodeSecondaryJSON(r, &req, false); err != nil {
			return op, err
		}
		op.Text, op.Author, op.Parent = req.Text, req.Author, req.Parent
	case SecondaryEditNote, SecondarySetSummary:
		var req struct {
			Text string `json:"text"`
		}
		if err := decodeSecondaryJSON(r, &req, false); err != nil {
			return op, err
		}
		op.Text = req.Text
	case SecondarySetReaction:
		if r.Method == http.MethodDelete {
			if err := decodeSecondaryJSON(r, &struct{}{}, true); err != nil {
				return op, err
			}
			op.Emoji, op.Actor = r.PathValue("emoji"), r.URL.Query().Get("actor")
		} else {
			var req struct {
				Emoji string `json:"emoji"`
				Actor string `json:"actor"`
			}
			if err := decodeSecondaryJSON(r, &req, false); err != nil {
				return op, err
			}
			op.Emoji, op.Actor, op.On = req.Emoji, req.Actor, true
		}
		if op.Emoji == "" {
			return op, errors.New("emoji is required")
		}
	case SecondaryAddDependency, SecondaryRemoveDependency, SecondaryLink, SecondaryUnlink:
		var req RelationRequest
		if err := decodeSecondaryJSON(r, &req, false); err != nil {
			return op, err
		}
		if len(req.Related) == 0 || slices.Contains(req.Related, "") {
			return op, errors.New("related ids must not be empty")
		}
		if (action == SecondaryAddDependency || action == SecondaryRemoveDependency) && len(req.Related) != 1 {
			return op, errors.New("a dependency relates exactly one ticket")
		}
		op.RelatedIDs = req.Related
	case SecondarySetParent:
		var req ParentRequest
		if err := decodeSecondaryJSON(r, &req, false); err != nil {
			return op, err
		}
		if req.Parent == "" {
			return op, errors.New("parent is required")
		}
		op.Parent = req.Parent
	case SecondarySetChildOrder:
		var req ChildOrderRequest
		if err := decodeSecondaryJSON(r, &req, false); err != nil {
			return op, err
		}
		if slices.Contains(req.Children, "") {
			return op, errors.New("child ids must not be empty")
		}
		op.Children = req.Children
	case SecondaryDeleteNote, SecondaryClearParent:
		if err := decodeSecondaryJSON(r, &struct{}{}, true); err != nil {
			return op, err
		}
	default:
		return op, ErrSecondaryOperationRefused
	}
	if (action == SecondaryAddNote || action == SecondaryEditNote || action == SecondarySetSummary) && op.Text == "" {
		return op, errors.New("text is required")
	}
	return op, nil
}

func decodeSecondaryJSON(r *http.Request, dst any, optional bool) error {
	dec := json.NewDecoder(r.Body)
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		if optional && errors.Is(err, io.EOF) {
			return nil
		}
		return errors.New("invalid JSON body")
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return errors.New("body must contain a single JSON value")
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return errors.New("body must be a JSON object")
	}
	strict := json.NewDecoder(bytes.NewReader(raw))
	strict.DisallowUnknownFields()
	if err := strict.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

func writeSecondaryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrTicketSourceUnavailable):
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, ErrSecondaryOperationRefused), errors.Is(err, ticket.ErrManaged):
		writeJSONError(w, http.StatusForbidden, err.Error())
	default:
		writeServiceError(w, err)
	}
}
