package evaluation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/nauticana/scout/domain"
)

// trajectoryDocument is the JSON a case's trajectory is stored and digested as.
type trajectoryDocument struct {
	Events []trajectoryRecord `json:"events"`
}

// trajectoryRecord is one event; a payload that is not JSON is stored as a JSON string.
type trajectoryRecord struct {
	Sequence  int64            `json:"sequence"`
	Kind      string           `json:"kind"`
	Name      string           `json:"name,omitempty"`
	Payload   json.RawMessage  `json:"payload,omitempty"`
	OffsetMs  int64            `json:"offset_ms"`
	Failed    bool             `json:"failed,omitempty"`
	Recovered bool             `json:"recovered,omitempty"`
	Usage     *trajectoryUsage `json:"usage,omitempty"`
}

type trajectoryUsage struct {
	InputTokens    int64  `json:"input_tokens,omitempty"`
	OutputTokens   int64  `json:"output_tokens,omitempty"`
	ToolCalls      int    `json:"tool_calls,omitempty"`
	SearchQueries  int64  `json:"search_queries,omitempty"`
	CostMinorUnits int64  `json:"cost_minor_units,omitempty"`
	Currency       string `json:"currency,omitempty"`
}

// encodeTrajectory renders events as the stored trajectory document.
func encodeTrajectory(events []domain.TrajectoryEvent) ([]byte, error) {
	events = append([]domain.TrajectoryEvent(nil), events...)
	for i := range events {
		events[i].Sequence = int64(i + 1)
	}
	if err := validateTrajectory(events); err != nil {
		return nil, err
	}
	document := trajectoryDocument{Events: make([]trajectoryRecord, 0, len(events))}
	for _, event := range events {
		record := trajectoryRecord{
			Sequence: event.Sequence, Kind: string(event.Kind), Name: event.Name, OffsetMs: event.Offset.Milliseconds(),
			Failed: event.Failed, Recovered: event.Recovered,
		}
		if len(bytes.TrimSpace(event.Payload)) > 0 {
			if json.Valid(event.Payload) {
				record.Payload = event.Payload
			} else {
				text, err := json.Marshal(string(event.Payload))
				if err != nil {
					return nil, fmt.Errorf("encode trajectory payload: %w", err)
				}
				record.Payload = text
			}
		}
		if usage := event.Usage; usage.InputTokens != 0 || usage.OutputTokens != 0 || usage.ToolCalls != 0 || usage.SearchQueries != 0 || usage.CostMinorUnits != 0 {
			record.Usage = &trajectoryUsage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, ToolCalls: usage.ToolCalls,
				SearchQueries: usage.SearchQueries, CostMinorUnits: usage.CostMinorUnits, Currency: usage.Currency}
		}
		document.Events = append(document.Events, record)
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode trajectory: %w", err)
	}
	return encoded, nil
}

// decodeTrajectory reads a stored trajectory document strictly; payloads stay JSON.
func decodeTrajectory(content []byte) ([]domain.TrajectoryEvent, error) {
	var document trajectoryDocument
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("%w: trajectory: %v", domain.ErrValidation, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trajectory has trailing JSON", domain.ErrValidation)
	}
	events := make([]domain.TrajectoryEvent, 0, len(document.Events))
	for _, record := range document.Events {
		event := domain.TrajectoryEvent{
			Sequence: record.Sequence, Kind: domain.TrajectoryEventKind(record.Kind), Name: record.Name, Payload: record.Payload,
			Offset: time.Duration(record.OffsetMs) * time.Millisecond, Failed: record.Failed, Recovered: record.Recovered,
		}
		if usage := record.Usage; usage != nil {
			event.Usage = domain.Usage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, ToolCalls: usage.ToolCalls,
				SearchQueries: usage.SearchQueries, CostMinorUnits: usage.CostMinorUnits, Currency: usage.Currency}
		}
		events = append(events, event)
	}
	if err := validateTrajectory(events); err != nil {
		return nil, err
	}
	return events, nil
}

func validateTrajectory(events []domain.TrajectoryEvent) error {
	for i, event := range events {
		usage := event.Usage
		validKind := event.Kind == domain.TrajectoryToken || event.Kind == domain.TrajectoryToolCall ||
			event.Kind == domain.TrajectoryObservation || event.Kind == domain.TrajectoryState || event.Kind == domain.TrajectoryPolicy
		if event.Sequence != int64(i+1) || !validKind || event.Offset < 0 || usage.InputTokens < 0 || usage.OutputTokens < 0 ||
			usage.ToolCalls < 0 || usage.SearchQueries < 0 || usage.CostMinorUnits < 0 || usage.CostMinorUnits > 0 && len(usage.Currency) != 3 {
			return fmt.Errorf("%w: trajectory event %d is invalid", domain.ErrValidation, i+1)
		}
	}
	return nil
}
