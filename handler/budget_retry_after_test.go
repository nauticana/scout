package handler

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	keelhandler "github.com/nauticana/keel/handler"
	keellimiter "github.com/nauticana/keel/limiter"

	"github.com/nauticana/scout/domain"
)

func TestAnExhaustedReleaseBudgetKeepsRetryAfterThroughIngress(t *testing.T) {
	exhausted := fmt.Errorf("%w: agent %q has spent its day budget", domain.ErrBudgetExceeded, "writer-a")
	for after, header := range map[time.Duration]string{90*time.Minute + 500*time.Millisecond: "5401", 0: ""} {
		// The ingress wraps the ledger's refusal; the hint must survive it.
		wrapped := fmt.Errorf("reserve turn budget: %w", &keellimiter.LimitError{Err: exhausted, Scope: "agent_budget", After: after})
		_, mapped := mapConversationError(nil, wrapped)
		var apiErr *keelhandler.APIError
		if !errors.As(mapped, &apiErr) || apiErr.Status != http.StatusTooManyRequests || apiErr.Header.Get("Retry-After") != header {
			t.Errorf("after %s: mapped = %#v, want 429 with Retry-After %q", after, mapped, header)
		}
	}
}
