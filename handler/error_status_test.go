package handler

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	keelhandler "github.com/nauticana/keel/handler"

	"github.com/nauticana/scout/domain"
)

func TestKeelAnswersScoutSentinelsWithTheirStatuses(t *testing.T) {
	h := &keelhandler.AbstractHandler{}
	for _, entry := range errorStatuses {
		recorder := httptest.NewRecorder()
		h.WriteServiceError(recorder, httptest.NewRequest(http.MethodGet, "/", nil), fmt.Errorf("%w: detail", entry.err))
		if recorder.Code != entry.status {
			t.Fatalf("%v: status = %d, want %d", entry.err, recorder.Code, entry.status)
		}
	}
	recorder := httptest.NewRecorder()
	h.WriteServiceError(recorder, httptest.NewRequest(http.MethodGet, "/", nil), errors.New("unclassified"))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("unclassified: status = %d", recorder.Code)
	}
}

func TestMapServiceErrorKeepsTheExecutionLimitDetailPrivate(t *testing.T) {
	_, err := mapServiceError(nil, fmt.Errorf("%w: provider said secret", domain.ErrExecutionLimit))
	apiErr, ok := errors.AsType[*keelhandler.APIError](err)
	if !ok || apiErr.Status != http.StatusUnprocessableEntity || apiErr.Msg != domain.ErrExecutionLimit.Error() {
		t.Fatalf("mapped = %v", err)
	}
	_, err = mapServiceError(nil, fmt.Errorf("%w: gone", domain.ErrReplayExpired))
	if apiErr, ok = errors.AsType[*keelhandler.APIError](err); !ok || apiErr.Status != http.StatusGone {
		t.Fatalf("replay expired = %v", err)
	}
	_, err = mapServiceError(nil, fmt.Errorf("%w: %w", domain.ErrExecutionLimit, domain.ErrNotReady))
	if apiErr, ok = errors.AsType[*keelhandler.APIError](err); !ok || apiErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("an unavailable service behind a limit = %v", err)
	}
}
