package dataplane

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

func newObjectStateStore(storage *fake.ObjectStorage) *ObjectStateStore {
	return &ObjectStateStore{Storage: storage, Bucket: "sessions", KeyPrefix: "scout", MaxBytes: 1 << 20}
}

func TestObjectStateStoreRoundTripUsesContentAddressedKeys(t *testing.T) {
	storage := &fake.ObjectStorage{}
	store := newObjectStateStore(storage)
	payload := []byte(`{"state":1}`)

	ref, err := store.Dehydrate(context.Background(), checkpointStateName(7, "conv/one", 2, 3), payload)
	if err != nil {
		t.Fatal(err)
	}
	wantKey := "scout/checkpoint/7/conv%2Fone/2/3/" + DigestBytes(payload)
	if ref.URI != "scout://sessions/"+wantKey || ref.Digest != DigestBytes(payload) || len(ref.Digest) != 64 {
		t.Fatalf("ref = %+v", ref)
	}
	if _, ok := storage.Payload("sessions", wantKey); !ok {
		t.Fatalf("object not stored under %q: %v", wantKey, storage.Uploads)
	}
	got, err := store.Hydrate(context.Background(), ref)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("hydrate = %q, %v", got, err)
	}
	// A retry with identical content overwrites the same key; different content never collides.
	again, _ := store.Dehydrate(context.Background(), checkpointStateName(7, "conv/one", 2, 3), payload)
	other, _ := store.Dehydrate(context.Background(), checkpointStateName(7, "conv/one", 2, 3), []byte(`{"state":2}`))
	if again.URI != ref.URI || other.URI == ref.URI || len(storage.Objects) != 2 {
		t.Fatalf("keys: again=%s other=%s objects=%d", again.URI, other.URI, len(storage.Objects))
	}
	if err := store.Delete(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if _, ok := storage.Payload("sessions", wantKey); ok {
		t.Fatal("delete left the object behind")
	}
}

func TestObjectStateStoreHydrateFailsClosedOnDigestMismatch(t *testing.T) {
	storage := &fake.ObjectStorage{}
	store := newObjectStateStore(storage)
	ref, err := store.Dehydrate(context.Background(), "step/1/req/9", []byte("original"))
	if err != nil {
		t.Fatal(err)
	}
	_, key, _ := strings.Cut(strings.TrimPrefix(ref.URI, "scout://"), "/")
	storage.Overwrite("sessions", key, []byte("tampered"))
	got, err := store.Hydrate(context.Background(), ref)
	if !errors.Is(err, ErrDigestMismatch) || got != nil {
		t.Fatalf("hydrate = %q, %v", got, err)
	}
	// A reference without a digest is rejected before any download.
	if _, err := store.Hydrate(context.Background(), domain.ObjectRef{URI: ref.URI}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("missing digest = %v", err)
	}
	if _, err := store.Hydrate(context.Background(), domain.ObjectRef{URI: "s3://sessions/x", Digest: ref.Digest}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("foreign scheme = %v", err)
	}
}

func TestObjectStateStoreEnforcesBounds(t *testing.T) {
	store := &ObjectStateStore{Storage: &fake.ObjectStorage{}, Bucket: "b", MaxBytes: 4}
	if _, err := store.Dehydrate(context.Background(), "n", []byte("12345")); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("oversize upload = %v", err)
	}
	if _, err := store.Dehydrate(context.Background(), "", []byte("1")); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("empty name = %v", err)
	}
	unbounded := &ObjectStateStore{Storage: &fake.ObjectStorage{}, Bucket: "b"}
	if _, err := unbounded.Dehydrate(context.Background(), "n", []byte("1")); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("missing max bytes = %v", err)
	}
	// Objects that grew past the bound after upload fail closed on read.
	storage := &fake.ObjectStorage{}
	small := &ObjectStateStore{Storage: storage, Bucket: "b", MaxBytes: 4}
	ref, err := small.Dehydrate(context.Background(), "n", []byte("1234"))
	if err != nil {
		t.Fatal(err)
	}
	storage.Overwrite("b", "n/"+ref.Digest, []byte("123456"))
	if _, err := small.Hydrate(context.Background(), ref); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("oversize download = %v", err)
	}
}
