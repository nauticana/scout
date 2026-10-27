package dataplane

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/stage"
)

// Sources in tie-break order: events recorded at the same instant order journal first,
// then guardrail, approval, audit decision, and Charter action.
const (
	traceSourceJournal = iota
	traceSourceGuardrail
	traceSourceApproval
	traceSourceAudit
	traceSourceCharter
)

// traceKey orders events by time, then source, then the source's own order.
type traceKey struct {
	at     time.Time
	source int
	seq    int64
}

func (key traceKey) before(other traceKey) bool {
	switch {
	case !key.at.Equal(other.at):
		return key.at.Before(other.at)
	case key.source != other.source:
		return key.source < other.source
	}
	return key.seq < other.seq
}

// traceCollector merges the sources into one bounded, ordered trace.
type traceCollector struct {
	maxEvents int
	events    []keyedTraceEvent
	// horizon is the first journal entry left unread; nothing at or after it is complete.
	horizon *traceKey
}

type keyedTraceEvent struct {
	key   traceKey
	event domain.RunTraceEvent
}

func (collector *traceCollector) add(key traceKey, event domain.RunTraceEvent) {
	event.At = key.at
	collector.events = append(collector.events, keyedTraceEvent{key: key, event: event})
}

func (collector *traceCollector) stopAt(key traceKey) {
	collector.horizon = &key
}

// ordered returns at most maxEvents events, cut at the journal horizon, and whether anything was cut.
func (collector *traceCollector) ordered() ([]domain.RunTraceEvent, bool) {
	slices.SortStableFunc(collector.events, func(a, b keyedTraceEvent) int {
		switch {
		case a.key.before(b.key):
			return -1
		case b.key.before(a.key):
			return 1
		}
		return 0
	})
	truncated := false
	kept := collector.events
	if collector.horizon != nil {
		truncated = true
		for index, event := range kept {
			if !event.key.before(*collector.horizon) {
				kept = kept[:index]
				break
			}
		}
	}
	if len(kept) > collector.maxEvents {
		kept, truncated = kept[:collector.maxEvents], true
	}
	events := make([]domain.RunTraceEvent, len(kept))
	for index, event := range kept {
		events[index] = event.event
		events[index].Sequence = index + 1
	}
	return events, truncated
}

// journalEvent classifies one loop entry without carrying its content.
func journalEvent(entry domain.LoopEntry, ref domain.RunTraceJournalRef) (domain.RunTraceEvent, error) {
	ref.Iteration = entry.Iteration
	if !validTraceUsage(entry.Usage) {
		return domain.RunTraceEvent{}, fmt.Errorf("entry has invalid usage")
	}
	switch entry.Kind {
	case domain.LoopEntryModel:
		if entry.Model == nil {
			return domain.RunTraceEvent{}, fmt.Errorf("model entry has no result")
		}
		finish := domain.RunTraceFinishAnswer
		if len(entry.Model.ToolCalls) > 0 {
			finish = domain.RunTraceFinishToolCalls
		}
		return domain.RunTraceEvent{Kind: domain.RunTraceEventModelStep, Model: &domain.RunTraceModelStep{
			Journal: ref, Models: spentModels(entry.Usage), Usage: entry.Usage, Finish: finish, ToolCalls: len(entry.Model.ToolCalls),
		}}, nil
	case domain.LoopEntryObservation:
		if entry.Observation == nil || entry.Tool.ToolID == "" {
			return domain.RunTraceEvent{}, fmt.Errorf("observation entry is incomplete")
		}
		call := &domain.RunTraceToolCall{Journal: ref, CallID: entry.Observation.CallID, Tool: entry.Tool, Outcome: domain.RunTraceToolOK, Usage: entry.Usage}
		if entry.Observation.IsError {
			call.Outcome, call.ErrorClass = domain.RunTraceToolError, observedErrorClass(entry.Observation.Output)
		}
		if entry.Effect != nil {
			if entry.Effect.Status != domain.EffectSatisfied && entry.Effect.Status != domain.EffectViolated && entry.Effect.Status != domain.EffectUnknown {
				return domain.RunTraceEvent{}, fmt.Errorf("observation entry has an invalid effect")
			}
			call.Effect, call.Reconciled = entry.Effect.Status, entry.Effect.Reconciled
		}
		return domain.RunTraceEvent{Kind: domain.RunTraceEventToolCall, Tool: call}, nil
	case domain.LoopEntryApprovalPending, domain.LoopEntryRevision:
		return domain.RunTraceEvent{}, nil
	default:
		return domain.RunTraceEvent{}, fmt.Errorf("unknown entry kind %q", entry.Kind)
	}
}

func validTraceUsage(usage domain.Usage) bool {
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.ToolCalls < 0 || usage.SearchQueries < 0 || usage.CostMinorUnits < 0 ||
		usage.CostMinorUnits > 0 && strings.TrimSpace(usage.Currency) == "" {
		return false
	}
	for _, spend := range usage.Models {
		if spend.Model.ProviderID == "" || spend.Model.ModelID == "" || spend.InputTokens < 0 || spend.OutputTokens < 0 ||
			spend.SearchQueries < 0 || spend.CostMinorUnits < 0 {
			return false
		}
	}
	return true
}

func spentModels(usage domain.Usage) []domain.ModelReference {
	var models []domain.ModelReference
	for _, spend := range usage.Models {
		if !slices.Contains(models, spend.Model) {
			models = append(models, spend.Model)
		}
	}
	return models
}

// observedErrorClass reads the failure class the loop journaled for a failed call; anything
// but a known class is left out rather than echoed.
func observedErrorClass(output []byte) string {
	var body struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(output, &body) != nil || !stage.KnownErrorClass(body.Error) {
		return ""
	}
	return body.Error
}
