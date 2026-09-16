package handler

import (
	"errors"
	"net/http"
	"testing"
	"time"

	keelhandler "github.com/nauticana/keel/handler"

	keellimiter "github.com/nauticana/keel/limiter"
	"github.com/nauticana/scout/domain"
)

// A rate-limit rejection must reach the client with its retry advice: the
// status mapping used to replace the typed error and drop it.
func TestMapStudioError_KeepsRetryAfterOnRateLimit(t *testing.T) {
	limit := &keellimiter.LimitError{Err: domain.ErrRateLimited, Scope: "tenant", After: 1500 * time.Millisecond}

	_, mapped := mapStudioError(nil, limit)

	var apiErr *keelhandler.APIError
	if !errors.As(mapped, &apiErr) || apiErr.Status != http.StatusTooManyRequests {
		t.Fatalf("mapped = %v, want a 429 APIError", mapped)
	}
	// Retry-After is whole seconds, so 1.5s rounds up.
	if got := apiErr.Header.Get("Retry-After"); got != "2" {
		t.Errorf("Retry-After = %q, want 2", got)
	}
}

func TestMapStudioError_NoRetryHeaderWithoutAdvice(t *testing.T) {
	_, mapped := mapStudioError(nil, domain.ErrRateLimited)

	var apiErr *keelhandler.APIError
	if !errors.As(mapped, &apiErr) {
		t.Fatalf("mapped = %v, want an APIError", mapped)
	}
	if apiErr.Header.Get("Retry-After") != "" {
		t.Error("a limit with no retry advice must not claim one")
	}
}

func TestMapStudioError_KeepsRetryAfterOnCircuitOpen(t *testing.T) {
	limit := &keellimiter.LimitError{Err: domain.ErrCircuitOpen, Scope: "tool", After: time.Second}
	_, mapped := mapStudioError(nil, limit)
	var apiErr *keelhandler.APIError
	if !errors.As(mapped, &apiErr) || apiErr.Status != http.StatusServiceUnavailable || apiErr.Header.Get("Retry-After") != "1" {
		t.Fatalf("mapped = %#v", mapped)
	}
}
