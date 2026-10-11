package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	keelhandler "github.com/nauticana/keel/handler"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// StudioFeatureGovernance is the Studio feature StudioGovernanceHandler serves.
const StudioFeatureGovernance = "governance"

// StudioGovernanceHandler serves the governance fields of an agent's Charter definition.
type StudioGovernanceHandler struct {
	keelhandler.AbstractHandler
	Backend contract.StudioGovernanceBackend
}

// Feature is the manifest entry a product adds to StudioHandler.Features when it mounts this route.
func (h *StudioGovernanceHandler) Feature() api.StudioFeature {
	return api.StudioFeature{ID: StudioFeatureGovernance, Version: 1, Actions: []string{string(domain.StudioView), string(domain.StudioEdit)}}
}

// Routes serves GET governance?agent_name= and POST governance on one path.
func (h *StudioGovernanceHandler) Routes() map[string]http.HandlerFunc {
	read, save := query(&h.AbstractHandler, h.governance), command(&h.AbstractHandler, h.save)
	return map[string]http.HandlerFunc{api.StudioGovernancePath: func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			save(w, r)
			return
		}
		read(w, r)
	}}
}

func (h *StudioGovernanceHandler) governance(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	return apiGovernance(h.Backend.Governance(ctx, actor, query.Get("agent_name")))
}

func (h *StudioGovernanceHandler) save(ctx context.Context, actor domain.StudioActor, request api.AgentGovernanceChange) (any, error) {
	governance, err := decodeGovernance(request.Draft)
	if err != nil {
		return nil, badRequest("draft must be one Charter governance object: " + err.Error())
	}
	return apiGovernance(h.Backend.SaveGovernance(ctx, actor, domain.AgentGovernanceChange{
		AgentID: request.AgentName, Governance: governance, ExpectedDraftRevision: request.ExpectedAgentRevision,
	}))
}

// decodeGovernance refuses a missing draft and an unknown member, so neither a lost body nor a
// misspelled field saves a draft without it.
func decodeGovernance(raw json.RawMessage) (domain.AgentGovernance, error) {
	var governance domain.AgentGovernance
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return governance, errors.New("draft is required")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&governance); err != nil {
		return domain.AgentGovernance{}, err
	}
	return governance, nil
}

func apiGovernance(view domain.AgentGovernanceView, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	effective, err := json.Marshal(view.Effective)
	if err != nil {
		return nil, err
	}
	draft, err := json.Marshal(view.Draft)
	if err != nil {
		return nil, err
	}
	problems := make([]api.AgentFieldError, 0, len(view.Problems))
	for _, problem := range view.Problems {
		problems = append(problems, api.AgentFieldError{Field: problem.Field, Message: problem.Message})
	}
	return api.AgentGovernance{
		AgentName: view.AgentID, Governance: effective, Draft: draft, EnterpriseLink: view.Link,
		Problems: problems, ExpectedAgentRevision: view.DraftRevision,
	}, nil
}
