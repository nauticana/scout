package charter

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/charter/sdk/corpus"
	"github.com/nauticana/charter/sdk/model"

	"github.com/nauticana/scout/domain"
)

const tenantNS = "tenant.example"

func separationCorpus(t *testing.T, documents ...string) *corpus.Corpus {
	t.Helper()
	c := corpus.New()
	for _, raw := range documents {
		d, err := corpus.Parser{}.Parse([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if err = c.Add(d); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

const publishConstraint = `{"charterSpecVersion":"1.3.0","namespace":"tenant.example","id":"SOD-AGENT-PUBLISH","kind":"SodConstraint",
 "constrainedActions":["CAP-AGENT-AUTHOR","CAP-AGENT-PUBLISH"],"scope":"agent"}`

func exception(id, beneficiary, grantor, to, revoked string) string {
	revokedAt := ""
	if revoked != "" {
		revokedAt = `,"revokedAt":"` + revoked + `"`
	}
	return `{"charterSpecVersion":"1.3.0","namespace":"tenant.example","id":"` + id + `","kind":"SodException",
 "validity":{"from":"2026-01-01","to":"` + to + `"},"sodConstraintId":"SOD-AGENT-PUBLISH",
 "beneficiary":{"kind":"HumanIdentity","id":"` + beneficiary + `"},"grantor":{"kind":"HumanIdentity","id":"` + grantor + `"},
 "reason":"one-person team","compensatingReview":"monthly audit"` + revokedAt + `}`
}

func separation(documents corpus.Source) *PublishSeparation {
	return &PublishSeparation{
		Documents: documents, Namespace: func(int64) string { return tenantNS },
		Author: model.Ref{ID: "CAP-AGENT-AUTHOR"}, Publish: model.Ref{ID: "CAP-AGENT-PUBLISH"},
		Actor: func(_ context.Context, _ int64, person domain.PrincipalRef) (model.ObjectRef, error) {
			return model.ObjectRef{Kind: model.KindHumanIdentity, ID: "user-" + person.ID}, nil
		},
	}
}

var separationAt = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

func TestPublishSeparationIsRequiredOnlyByAConstraintOnBothCapabilities(t *testing.T) {
	required, err := separation(separationCorpus(t, publishConstraint)).Required(context.Background(), 7, "writer-a", separationAt)
	if err != nil || !required {
		t.Fatalf("required = %v, %v", required, err)
	}
	other := `{"charterSpecVersion":"1.3.0","namespace":"tenant.example","id":"SOD-OTHER","kind":"SodConstraint","constrainedActions":["CAP-AGENT-AUTHOR","CAP-PAY"]}`
	required, err = separation(separationCorpus(t, other)).Required(context.Background(), 7, "writer-a", separationAt)
	if err != nil || required {
		t.Fatalf("an unrelated constraint required = %v, %v", required, err)
	}
	foreign := `{"charterSpecVersion":"1.3.0","namespace":"other.example","id":"SOD-AGENT-PUBLISH","kind":"SodConstraint","constrainedActions":["CAP-AGENT-AUTHOR","CAP-AGENT-PUBLISH"]}`
	required, err = separation(separationCorpus(t, foreign)).Required(context.Background(), 7, "writer-a", separationAt)
	if err != nil || required {
		t.Fatalf("another namespace's constraint required = %v, %v", required, err)
	}
	expired := `{"charterSpecVersion":"1.3.0","namespace":"tenant.example","id":"SOD-OLD","kind":"SodConstraint","validity":{"from":"2025-01-01","to":"2025-12-31"},"constrainedActions":["CAP-AGENT-AUTHOR","CAP-AGENT-PUBLISH"]}`
	required, err = separation(separationCorpus(t, expired)).Required(context.Background(), 7, "writer-a", separationAt)
	if err != nil || required {
		t.Fatalf("an expired constraint required = %v, %v", required, err)
	}
	if _, err = (&PublishSeparation{}).Required(context.Background(), 7, "writer-a", separationAt); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("unconfigured = %v", err)
	}
}

func TestPublishSeparationExcusesOnlyABoundedUnrevokedExceptionForItsBeneficiary(t *testing.T) {
	person := domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "9"}
	documents := separationCorpus(t, publishConstraint,
		exception("EXC-1", "user-9", "user-1", "2026-12-31", ""),
		exception("EXC-SELF", "user-9", "user-9", "2026-12-31", ""),
		exception("EXC-LAPSED", "user-9", "user-1", "2026-03-31", ""),
		exception("EXC-REVOKED", "user-9", "user-1", "2026-12-31", "2026-05-01T00:00:00Z"),
	)
	p := separation(documents)
	if err := p.Excuse(context.Background(), 7, "writer-a", "EXC-1", person, separationAt); err != nil {
		t.Fatalf("a valid exception: %v", err)
	}
	for _, refused := range []struct {
		id     string
		person domain.PrincipalRef
	}{
		{"EXC-1", domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "10"}},
		{"EXC-SELF", person}, {"EXC-LAPSED", person}, {"EXC-REVOKED", person}, {"EXC-MISSING", person}, {"", person},
	} {
		if err := p.Excuse(context.Background(), 7, "writer-a", refused.id, refused.person, separationAt); !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("exception %q for %s = %v, want forbidden", refused.id, refused.person.ID, err)
		}
	}
	second := `{"charterSpecVersion":"1.3.0","namespace":"tenant.example","id":"SOD-SECOND","kind":"SodConstraint","constrainedActions":["CAP-AGENT-AUTHOR","CAP-AGENT-PUBLISH"]}`
	both := separation(separationCorpus(t, publishConstraint, second, exception("EXC-1", "user-9", "user-1", "2026-12-31", "")))
	if err := both.Excuse(context.Background(), 7, "writer-a", "EXC-1", person, separationAt); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a second constraint must still separate: %v", err)
	}
}
