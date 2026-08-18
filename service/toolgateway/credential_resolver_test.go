package toolgateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

type stubBindings map[string]domain.CredentialBinding

func (s stubBindings) Binding(_ context.Context, principal domain.Principal, toolID, purpose string) (domain.CredentialBinding, error) {
	binding, found := s[principal.ID]
	if !found {
		return domain.CredentialBinding{}, domain.ErrForbidden
	}
	binding.ToolID, binding.Purpose = toolID, purpose
	return binding, nil
}

type stubSecrets struct {
	issued map[string]int
	ttl    time.Duration
}

func (s *stubSecrets) Resolve(_ context.Context, ref string, _ []string, ttl time.Duration) ([]byte, error) {
	if s.issued == nil {
		s.issued = map[string]int{}
	}
	s.issued[ref]++
	s.ttl = ttl
	return []byte("secret-for-" + ref), nil
}

func principal(id string) domain.Principal {
	return domain.Principal{Kind: domain.PrincipalAgent, ID: id, TenantID: 7, Release: "3"}
}

func TestTwoPrincipalsOnOneToolResolveDifferentCredentials(t *testing.T) {
	secrets := &stubSecrets{}
	provider := &BoundCredentialProvider{
		Bindings: stubBindings{
			"agent-a": {CredentialRef: "identity-a"},
			"agent-b": {CredentialRef: "identity-b"},
		},
		Secrets: secrets, DefaultTTL: time.Minute,
	}
	first, _, err := provider.Credential(context.Background(), principal("agent-a"), "wire", "invoke", "call")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := provider.Credential(context.Background(), principal("agent-b"), "wire", "invoke", "call")
	if err != nil {
		t.Fatal(err)
	}
	if string(first) == string(second) {
		t.Fatal("two agents on one tool must never share an identity")
	}
}

func TestCredentialRecordsDelegatedAuthority(t *testing.T) {
	human := domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}
	provider := &BoundCredentialProvider{
		Bindings: stubBindings{"agent-a": {CredentialRef: "connection-42", DelegatedFrom: human}},
		Secrets:  &stubSecrets{}, DefaultTTL: time.Minute,
	}
	_, authority, err := provider.Credential(context.Background(), principal("agent-a"), "wire", "invoke", "call")
	if err != nil {
		t.Fatal(err)
	}
	if authority.Grantor != human || authority.Subject.ID != "agent-a" {
		t.Fatalf("authority = %+v, want the delegating human recorded", authority)
	}
}

func TestCredentialAppliesTheBindingTTLOverTheDefault(t *testing.T) {
	secrets := &stubSecrets{}
	provider := &BoundCredentialProvider{
		Bindings: stubBindings{"agent-a": {CredentialRef: "identity-a", MaxTTL: 30 * time.Second}},
		Secrets:  secrets, DefaultTTL: time.Hour,
	}
	if _, _, err := provider.Credential(context.Background(), principal("agent-a"), "wire", "invoke", "call"); err != nil {
		t.Fatal(err)
	}
	if secrets.ttl != 30*time.Second {
		t.Fatalf("ttl = %s, want the binding ceiling to bind", secrets.ttl)
	}
}

func TestCredentialFailsClosedWithoutABinding(t *testing.T) {
	provider := &BoundCredentialProvider{Bindings: stubBindings{}, Secrets: &stubSecrets{}}
	if _, _, err := provider.Credential(context.Background(), principal("agent-a"), "wire", "invoke", "call"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("error = %v, want an unbound principal refused", err)
	}
}
