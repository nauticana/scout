package policy

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
)

func TestDecodeLayerRefusesStoredContentThatCanBroadenPolicy(t *testing.T) {
	allow := denial("a")
	allow.Effect = domain.PolicyAllow
	denials, err := json.Marshal([]domain.PolicyStatement{allow})
	if err != nil {
		t.Fatal(err)
	}
	rules := []byte(`{"schema_version":1,"rules":[]}`)
	if _, err = decodeLayer(layerDigest(denials, rules), string(denials), string(rules)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stored allow: want ErrConflict, got %v", err)
	}
}

func TestEncodeLayerIsOrderInsensitive(t *testing.T) {
	ctx := context.Background()
	first, err := encodeLayer(ctx, nil, domain.RestrictionLayer{Denials: []domain.PolicyStatement{denial("b"), denial(" a ")}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := encodeLayer(ctx, nil, domain.RestrictionLayer{Denials: []domain.PolicyStatement{denial("a"), denial("b")}})
	if err != nil || first.layer.Digest != second.layer.Digest || first.layer.Denials[0].ID != "a" {
		t.Fatalf("digests %s / %s, %v", first.layer.Digest, second.layer.Digest, err)
	}
	if _, err = encodeLayer(ctx, nil, domain.RestrictionLayer{Guardrails: []domain.GuardrailRule{noCodename()}}); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("guardrails without a rule compiler: want ErrNotReady, got %v", err)
	}
}

func TestDecodeLayerRefusesTamperedOrForeignContent(t *testing.T) {
	encoded, err := encodeLayer(context.Background(), nil, domain.RestrictionLayer{Denials: []domain.PolicyStatement{denial("a")}})
	if err != nil {
		t.Fatal(err)
	}
	if layer, err := decodeLayer(encoded.layer.Digest, string(encoded.denials), string(encoded.guardrails)); err != nil || len(layer.Denials) != 1 {
		t.Fatalf("round trip = %+v, %v", layer, err)
	}
	if _, err = decodeLayer(encoded.layer.Digest, "[]", string(encoded.guardrails)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("content changed under its digest: want ErrConflict, got %v", err)
	}
	future := []byte(`{"schema_version":2,"rules":[]}`)
	if _, err = decodeLayer(layerDigest(encoded.denials, future), string(encoded.denials), string(future)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("an unknown guardrail schema: want ErrConflict, got %v", err)
	}
	flag := []byte(`{"schema_version":1,"rules":[{"id":"x","kind":"exact_phrase","action":"flag","severity":"soft"}]}`)
	if _, err = decodeLayer(layerDigest(encoded.denials, flag), string(encoded.denials), string(flag)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a stored rule that does not restrict: want ErrConflict, got %v", err)
	}
}
