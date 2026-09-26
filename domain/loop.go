package domain

import (
	"encoding/json"
	"fmt"
	"time"
)

// StepKindToolLoop is the execution step kind that runs a bounded model↔tool loop.
const StepKindToolLoop = "tool_loop"

// ToolLoopConfig is the ExecutionStep.Configuration of a tool_loop step. Every
// limit may only narrow the executor's own; zero inherits it.
type ToolLoopConfig struct {
	MaxIterations     int             `json:"max_iterations,omitempty"`
	MaxToolCalls      int             `json:"max_tool_calls,omitempty"`
	MaxTokens         int64           `json:"max_tokens,omitempty"`
	MaxCostMinorUnits int64           `json:"max_cost_minor_units,omitempty"`
	MaxRepeatedCalls  int             `json:"max_repeated_calls,omitempty"`
	DeadlineSeconds   int64           `json:"deadline_seconds,omitempty"`
	MaxOutputTokens   int64           `json:"max_output_tokens,omitempty"`
	Capabilities      []string        `json:"capabilities,omitempty"`
	OutputSchemaName  string          `json:"output_schema_name,omitempty"`
	OutputSchema      json.RawMessage `json:"output_schema,omitempty"`
	RequireEvidence   bool            `json:"require_evidence,omitempty"`
	// Search permits provider-native web search on the step's model calls and
	// bounds it; a task asks for it and may narrow MaxSearches, never widen it.
	Search     *SearchGrounding `json:"search,omitempty"`
	NextStepID string           `json:"next_step_id,omitempty"`
}

// NarrowedSearch is the grounding a task's model calls run under this step:
// nothing unless the task asks, and never more than the step permits.
func (config ToolLoopConfig) NarrowedSearch(task *SearchGrounding) (*SearchGrounding, error) {
	if task == nil {
		return nil, nil
	}
	if config.Search == nil {
		return nil, fmt.Errorf("%w: the task asks for search grounding the step does not permit", ErrValidation)
	}
	search := *config.Search
	if task.MaxSearches > 0 && (search.MaxSearches == 0 || task.MaxSearches < search.MaxSearches) {
		search.MaxSearches = task.MaxSearches
	}
	return &search, nil
}

// ToolLoopLimits bound one loop step; each is enforced fail closed.
type ToolLoopLimits struct {
	MaxIterations int
	MaxToolCalls  int
	MaxTokens     int64
	// MaxCostMinorUnits is checked against priced usage; zero leaves cost to the turn budget.
	MaxCostMinorUnits int64
	// MaxRepeatedCalls trips ErrLoopDetected when one tool is proposed with identical arguments this often.
	MaxRepeatedCalls int
	Deadline         time.Duration
}

// LoopKey identifies the journal of one loop step within one turn.
type LoopKey struct {
	TenantID        int64
	RequestID       string
	ExecutionStepID int64
}

// LoopEntryKind classifies one journaled loop event.
type LoopEntryKind string

const (
	LoopEntryModel           LoopEntryKind = "model"
	LoopEntryObservation     LoopEntryKind = "observation"
	LoopEntryApprovalPending LoopEntryKind = "approval_pending"
)

// LoopEntry is one durable boundary of a loop. Entries are append-only and
// numbered from 1; replay reads them back instead of repeating the work.
type LoopEntry struct {
	EntryNo   int           `json:"entry_no"`
	Kind      LoopEntryKind `json:"kind"`
	Iteration int           `json:"iteration"`
	// Model is set on a model entry.
	Model *ModelResult `json:"model,omitempty"`
	// Tool and Observation are set on observation and approval entries.
	Tool        ToolReference         `json:"tool,omitzero"`
	Observation *ModelToolObservation `json:"observation,omitempty"`
	// ResourceURIs are the evidence links the tool returned with the observation.
	ResourceURIs []string `json:"resource_uris,omitempty"`
	// Effect is the observed postcondition of a verified mutating call, journaled with its
	// observation so replay neither repeats the mutation nor loses the evidence.
	Effect *EffectObservation `json:"effect,omitempty"`
	Usage  Usage              `json:"usage,omitzero"`
	Offset time.Duration      `json:"offset,omitempty"`
}
