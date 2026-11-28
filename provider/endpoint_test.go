package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

const endpointCompletion = `{"id":"1","object":"chat.completion","model":"llama","created":1,
	"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],
	"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`

func endpointFactory(endpoint domain.ModelEndpoint, allowlist ...string) *Factory {
	endpoint.ProviderID = "local"
	if endpoint.Protocol == "" {
		endpoint.Protocol = domain.ModelProtocolOpenAI
	}
	secrets := &secretProviderStub{values: map[string]string{"local_key": "secret"}}
	return NewFactory(secrets, FactoryConfig{Endpoints: fakeEndpointSource{"local": endpoint}, EndpointAllowlist: allowlist})
}

func generateLocal(t *testing.T, factory *Factory, ctx context.Context) (domain.ModelResult, error) {
	t.Helper()
	model, media, err := factory.Build(ctx, domain.ModelReference{ProviderID: "local", ModelID: "llama"})
	if err != nil {
		return domain.ModelResult{}, err
	}
	if media != nil {
		t.Fatal("a registered endpoint has no media adapter")
	}
	return model.Generate(ctx, domain.ModelSelection{Model: "llama"}, domain.ModelRequest{Prompt: []byte("hello")})
}

func TestRegisteredEndpointServesWithOptionalKey(t *testing.T) {
	var authorization []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = append(authorization, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(endpointCompletion))
	}))
	defer server.Close()
	t.Setenv("OPENAI_API_KEY", "must-not-leak")
	for _, ref := range []string{"", "local_key"} {
		result, err := generateLocal(t, endpointFactory(domain.ModelEndpoint{BaseURL: server.URL, CredentialRef: ref}, "127.0.0.0/8"), context.Background())
		if err != nil || string(result.Output) != "hi" || result.Usage.InputTokens != 3 {
			t.Fatalf("key %q: result = %+v err = %v", ref, result, err)
		}
	}
	if len(authorization) != 2 || authorization[0] != "" || authorization[1] != "Bearer secret" {
		t.Fatalf("authorization headers = %q", authorization)
	}
	embedder, err := endpointFactory(domain.ModelEndpoint{BaseURL: server.URL}, "127.0.0.1").BuildEmbedder(context.Background(), domain.ModelReference{ProviderID: "local", ModelID: "e"})
	if adapter, ok := embedder.(*OpenAI); err != nil || !ok || adapter.HTTPClient == nil {
		t.Fatalf("embedder = %v err = %v", embedder, err)
	}
}

func TestRegisteredEndpointEgressIsAllowlisted(t *testing.T) {
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/", http.StatusFound)
	}))
	defer redirect.Close()
	port := redirect.URL[strings.LastIndex(redirect.URL, ":"):]
	for name, test := range map[string]struct {
		url       string
		allowlist []string
	}{
		"empty allowlist":               {redirect.URL, nil},
		"unlisted address":              {redirect.URL, []string{"10.0.0.0/8"}},
		"listed name resolving private": {"http://localhost" + port, []string{"localhost"}},
		"redirect":                      {redirect.URL, []string{"127.0.0.1"}},
		"credentials in URL":            {"http://u:p@127.0.0.1" + port, []string{"127.0.0.1"}},
	} {
		if _, err := generateLocal(t, endpointFactory(domain.ModelEndpoint{BaseURL: test.url}, test.allowlist...), context.Background()); !errors.Is(err, domain.ErrForbidden) && !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, _, err := endpointFactory(domain.ModelEndpoint{BaseURL: redirect.URL}, "bad/entry").Build(context.Background(), domain.ModelReference{ProviderID: "local", ModelID: "m"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("malformed allowlist err = %v", err)
	}
	if _, _, err := endpointFactory(domain.ModelEndpoint{BaseURL: redirect.URL, Protocol: domain.ModelProtocolNative}, "127.0.0.1").Build(context.Background(), domain.ModelReference{ProviderID: "local", ModelID: "m"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("native protocol err = %v", err)
	}
	if _, _, err := NewFactory(nil, FactoryConfig{}).Build(context.Background(), domain.ModelReference{ProviderID: "local", ModelID: "m"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("no endpoint source err = %v", err)
	}
}

func TestRegisteredEndpointBoundsResponseAndConcurrency(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(endpointCompletion))
	}))
	defer server.Close()

	small := endpointFactory(domain.ModelEndpoint{BaseURL: server.URL}, "127.0.0.1")
	small.config.EndpointMaxResponseBytes = 32
	if _, err := generateLocal(t, small, context.Background()); err == nil {
		t.Fatal("an oversized response was accepted")
	}

	factory := endpointFactory(domain.ModelEndpoint{BaseURL: server.URL, MaxConcurrency: 1}, "127.0.0.1")
	slots := factory.endpointSlots("local", 1)
	slots <- struct{}{} // another call holds the only slot
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := generateLocal(t, factory, ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting for a slot err = %v", err)
	}
	<-slots
	if _, err := generateLocal(t, factory, context.Background()); err != nil {
		t.Fatalf("free slot: %v", err)
	}
	if len(slots) != 0 {
		t.Fatal("a finished call kept its slot")
	}
}
