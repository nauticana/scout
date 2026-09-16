package charter

import (
	"context"
	"errors"
	"fmt"

	"github.com/nauticana/charter/sdk/agent"
	"github.com/nauticana/charter/sdk/binding"
)

// TriggerHandler dispatches a delivered trigger to every definition declaring it; none declaring it is a refusal.
type TriggerHandler struct {
	Agents   agent.Provider
	Dispatch TriggerDispatcher
}

var _ binding.Handler = (*TriggerHandler)(nil)

func (h *TriggerHandler) Handle(ctx context.Context, d binding.Delivery) error {
	if h == nil || h.Agents == nil || h.Dispatch == nil {
		return fmt.Errorf("charter trigger handler is not fully composed")
	}
	definitions, err := h.Agents.Definitions(ctx)
	if err != nil {
		return err
	}
	var errs []error
	matched := 0
	for _, def := range definitions {
		if !agent.Triggered(def, d.Trigger) {
			continue
		}
		matched++
		if err := h.Dispatch.Dispatch(ctx, def, d); err != nil {
			errs = append(errs, fmt.Errorf("definition %s: %w", def.ID, err))
		}
	}
	if matched == 0 {
		return fmt.Errorf("no agent definition declares trigger %q", d.Trigger)
	}
	return errors.Join(errs...)
}
