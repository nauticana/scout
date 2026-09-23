package modelgateway

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/jsonschema"
)

// RequiredCapabilities is what a route must declare to serve the request: the
// explicit list plus what offered tools and a constrained output imply.
func RequiredCapabilities(request domain.ModelRequest) []string {
	required := slices.Clone(request.RequiredCapabilities)
	if len(request.Tools) > 0 && !slices.Contains(required, domain.CapabilityTools) {
		required = append(required, domain.CapabilityTools)
	}
	if request.Output.Mode != domain.OutputModeText && !slices.Contains(required, domain.CapabilityStructuredOutput) {
		required = append(required, domain.CapabilityStructuredOutput)
	}
	if request.Search != nil && !slices.Contains(required, domain.CapabilityWebSearch) {
		required = append(required, domain.CapabilityWebSearch)
	}
	if request.Temperature != nil && !slices.Contains(required, domain.CapabilitySampling) {
		required = append(required, domain.CapabilitySampling)
	}
	return required
}

// EstimatedSearches is the number of grounding searches a request may run,
// priced and budgeted before the call like its output tokens are.
func EstimatedSearches(request domain.ModelRequest) int64 {
	switch {
	case request.Search == nil:
		return 0
	case request.Search.MaxSearches > 0:
		return request.Search.MaxSearches
	}
	return 1
}

// modelContract is the compiled tool and output schemas of one request, held so
// every result and stream frame is validated against the same contract.
type modelContract struct {
	tools       map[string]*jsonschema.Schema
	output      *jsonschema.Schema
	usedCallIDs map[string]struct{}
}

func compileModelContract(request domain.ModelRequest) (*modelContract, error) {
	if request.Search != nil && request.Search.MaxSearches < 0 {
		return nil, fmt.Errorf("%w: max searches cannot be negative", domain.ErrValidation)
	}
	if request.Temperature != nil && (math.IsNaN(*request.Temperature) || math.IsInf(*request.Temperature, 0) || *request.Temperature < 0 || *request.Temperature > 2) {
		return nil, fmt.Errorf("%w: temperature must be between 0 and 2", domain.ErrValidation)
	}
	compiled := &modelContract{
		tools:       make(map[string]*jsonschema.Schema, len(request.Tools)),
		usedCallIDs: make(map[string]struct{}),
	}
	for _, tool := range request.Tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" || tool.ToolID == "" || tool.ToolVersion == "" {
			return nil, fmt.Errorf("%w: a model tool needs a name and a pinned tool version", domain.ErrValidation)
		}
		if _, duplicate := compiled.tools[name]; duplicate {
			return nil, fmt.Errorf("%w: model tool %q is offered twice", domain.ErrValidation, name)
		}
		schema, err := jsonschema.Compile(tool.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("%w: input schema of model tool %q: %w", domain.ErrValidation, name, err)
		}
		compiled.tools[name] = schema
	}
	pending := make(map[string]string)
	for _, message := range request.Messages {
		switch message.Role {
		case domain.ModelRoleUser:
			if len(message.ToolCalls) > 0 || len(message.Observations) > 0 {
				return nil, fmt.Errorf("%w: user messages cannot carry tool calls or observations", domain.ErrValidation)
			}
		case domain.ModelRoleAssistant:
			if len(message.Observations) > 0 {
				return nil, fmt.Errorf("%w: assistant messages cannot carry tool observations", domain.ErrValidation)
			}
		case domain.ModelRoleTool:
			if len(message.Text) > 0 || len(message.ToolCalls) > 0 || len(message.Observations) == 0 {
				return nil, fmt.Errorf("%w: tool messages must carry only observations", domain.ErrValidation)
			}
		default:
			return nil, fmt.Errorf("%w: unsupported model message role %q", domain.ErrValidation, message.Role)
		}
		for _, call := range message.ToolCalls {
			if strings.TrimSpace(call.CallID) == "" {
				return nil, fmt.Errorf("%w: conversation tool call %q has no call id", domain.ErrValidation, call.Name)
			}
			if _, duplicate := compiled.usedCallIDs[call.CallID]; duplicate {
				return nil, fmt.Errorf("%w: conversation tool call id %q is repeated", domain.ErrValidation, call.CallID)
			}
			schema, offered := compiled.tools[call.Name]
			if !offered {
				return nil, fmt.Errorf("%w: conversation called tool %q, which was not offered", domain.ErrValidation, call.Name)
			}
			if err := schema.ValidateJSON(call.Arguments); err != nil {
				return nil, fmt.Errorf("%w: conversation arguments of tool %q: %w", domain.ErrValidation, call.Name, err)
			}
			compiled.usedCallIDs[call.CallID] = struct{}{}
			pending[call.CallID] = call.Name
		}
		for _, observation := range message.Observations {
			name, awaiting := pending[observation.CallID]
			if !awaiting || name != observation.Name {
				return nil, fmt.Errorf("%w: observation %q does not match a pending call of tool %q", domain.ErrValidation, observation.CallID, observation.Name)
			}
			delete(pending, observation.CallID)
		}
	}
	if len(pending) > 0 {
		return nil, fmt.Errorf("%w: conversation has tool calls without observations", domain.ErrValidation)
	}
	switch request.Output.Mode {
	case domain.OutputModeText:
	case domain.OutputModeJSONSchema:
		schema, err := jsonschema.Compile(request.Output.Schema)
		if err != nil {
			return nil, fmt.Errorf("%w: output schema: %w", domain.ErrValidation, err)
		}
		compiled.output = schema
	default:
		return nil, fmt.Errorf("%w: output mode %q", domain.ErrValidation, request.Output.Mode)
	}
	return compiled, nil
}

// checkCapabilities confirms against the tenant's catalog that the selected route
// declares everything the request needs. It fails closed: without a catalog, or
// for a route the catalog does not list, a capability cannot be assumed.
func checkCapabilities(ctx context.Context, catalog contract.ModelCandidateCatalog, selection domain.ModelSelection, request domain.ModelRequest) error {
	required := RequiredCapabilities(request)
	if len(required) == 0 {
		return nil
	}
	_, err := capableCandidate(ctx, catalog, selection, request.TenantContext, required)
	return err
}

// capableCandidate returns the tenant's catalog entry for the selected route,
// confirming it declares every required capability.
func capableCandidate(ctx context.Context, catalog contract.ModelCandidateCatalog, selection domain.ModelSelection, tenant domain.TenantContext, required []string) (domain.ModelCandidate, error) {
	if catalog == nil {
		return domain.ModelCandidate{}, fmt.Errorf("%w: no candidate catalog to confirm %q", domain.ErrCapabilityUnsupported, required)
	}
	candidates, err := catalog.CandidatesFor(ctx, tenant)
	if err != nil {
		return domain.ModelCandidate{}, fmt.Errorf("route capabilities for tenant %d: %w", tenant.TenantID, err)
	}
	for _, candidate := range candidates.Candidates {
		if candidate.Provider != selection.Provider || candidate.Model != selection.Model ||
			selection.RouteID != "" && candidate.RouteID != selection.RouteID {
			continue
		}
		for _, capability := range required {
			if !slices.Contains(candidate.Capabilities, capability) {
				return domain.ModelCandidate{}, fmt.Errorf("%w: route %s/%s does not declare %q", domain.ErrCapabilityUnsupported, selection.Provider, selection.Model, capability)
			}
		}
		return candidate, nil
	}
	return domain.ModelCandidate{}, fmt.Errorf("%w: route %s/%s is not in the tenant catalog", domain.ErrCapabilityUnsupported, selection.Provider, selection.Model)
}

func (compiled *modelContract) checkToolCalls(calls []domain.ModelToolCall) error {
	for _, call := range calls {
		schema, offered := compiled.tools[call.Name]
		if !offered {
			return fmt.Errorf("%w: model called tool %q, which was not offered", domain.ErrInvalidModelOutput, call.Name)
		}
		if strings.TrimSpace(call.CallID) == "" {
			return fmt.Errorf("%w: call of tool %q has no call id", domain.ErrInvalidModelOutput, call.Name)
		}
		if _, duplicate := compiled.usedCallIDs[call.CallID]; duplicate {
			return fmt.Errorf("%w: tool call id %q is repeated", domain.ErrInvalidModelOutput, call.CallID)
		}
		if err := schema.ValidateJSON(call.Arguments); err != nil {
			return fmt.Errorf("%w: arguments of tool %q: %w", domain.ErrInvalidModelOutput, call.Name, err)
		}
		compiled.usedCallIDs[call.CallID] = struct{}{}
	}
	return nil
}

// checkTerminal validates the constrained answer; a turn that stopped to call
// tools has no terminal output yet.
func (compiled *modelContract) checkTerminal(output []byte, calledTools bool) error {
	if compiled.output == nil || calledTools {
		return nil
	}
	if err := compiled.output.ValidateJSON(output); err != nil {
		return fmt.Errorf("%w: constrained output: %w", domain.ErrInvalidModelOutput, err)
	}
	return nil
}

func (compiled *modelContract) checkResult(result domain.ModelResult) error {
	if result.Usage.InputTokens < 0 || result.Usage.OutputTokens < 0 || result.Usage.ToolCalls < 0 || result.Usage.SearchQueries < 0 ||
		result.Usage.CostMinorUnits < 0 || result.Usage.CostMinorUnits > 0 && strings.TrimSpace(result.Usage.Currency) == "" {
		return fmt.Errorf("%w: model returned invalid usage", domain.ErrInvalidModelOutput)
	}
	if err := compiled.checkToolCalls(result.ToolCalls); err != nil {
		return err
	}
	if err := new(citationCheck).add(result.Citations); err != nil {
		return err
	}
	return compiled.checkTerminal(result.Output, len(result.ToolCalls) > 0)
}

// citationCheck holds citations to unique URLs and contiguous positions, across
// every frame of a stream.
type citationCheck struct {
	seen map[string]struct{}
}

func (check *citationCheck) add(citations []domain.Citation) error {
	for _, citation := range citations {
		position := len(check.seen) + 1
		url := strings.TrimSpace(citation.URL)
		if url == "" || citation.Position != position {
			return fmt.Errorf("%w: citation %d has no URL or has an invalid position", domain.ErrInvalidModelOutput, position)
		}
		if _, duplicate := check.seen[url]; duplicate {
			return fmt.Errorf("%w: citation %d repeats URL %q", domain.ErrInvalidModelOutput, position, url)
		}
		if check.seen == nil {
			check.seen = make(map[string]struct{}, len(citations))
		}
		check.seen[url] = struct{}{}
	}
	return nil
}
