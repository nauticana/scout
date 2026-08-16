package dataplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/nauticana/keel/storage"

	"github.com/nauticana/scout/domain"
)

// ErrDigestMismatch reports object content that no longer matches the digest
// committed with its reference; readers fail closed rather than trust it.
var ErrDigestMismatch = errors.New("object digest mismatch")

// ObjectStateCodec moves large state between memory and object storage; rows
// keep only the returned reference.
type ObjectStateCodec interface {
	// Dehydrate stores payload under a deterministic, content-addressed key.
	Dehydrate(ctx context.Context, name string, payload []byte) (domain.ObjectRef, error)
	// Hydrate loads the referenced payload and verifies its digest.
	Hydrate(ctx context.Context, ref domain.ObjectRef) ([]byte, error)
	// Delete removes the referenced object.
	Delete(ctx context.Context, ref domain.ObjectRef) error
}

// ObjectStateStore is an ObjectStateCodec over keel object storage. Keys are
// "<KeyPrefix>/<name>/<sha256>" so a retry of identical content overwrites the
// same object while different content never collides with a concurrent writer.
type ObjectStateStore struct {
	Storage storage.ObjectStorage
	Bucket  string
	// KeyPrefix namespaces objects inside the bucket; empty is allowed.
	KeyPrefix string
	// Scheme prefixes stored URIs, "<Scheme>://<bucket>/<key>"; default "scout".
	Scheme string
	// ContentType is sent on upload; default application/octet-stream.
	ContentType string
	// MaxBytes bounds a single payload in both directions; required.
	MaxBytes int64
}

const (
	defaultObjectScheme      = "scout"
	defaultObjectContentType = "application/octet-stream"
)

func (store *ObjectStateStore) validate() error {
	if store.Storage == nil || strings.TrimSpace(store.Bucket) == "" || store.MaxBytes <= 0 {
		return fmt.Errorf("%w: object state store needs storage, bucket, and a positive max size", domain.ErrValidation)
	}
	return nil
}

func (store *ObjectStateStore) scheme() string {
	if store.Scheme == "" {
		return defaultObjectScheme
	}
	return store.Scheme
}

func (store *ObjectStateStore) contentType() string {
	if store.ContentType == "" {
		return defaultObjectContentType
	}
	return store.ContentType
}

// Dehydrate uploads payload and returns its URI and SHA-256 digest.
func (store *ObjectStateStore) Dehydrate(ctx context.Context, name string, payload []byte) (domain.ObjectRef, error) {
	if err := store.validate(); err != nil {
		return domain.ObjectRef{}, err
	}
	name = strings.Trim(name, "/")
	if name == "" {
		return domain.ObjectRef{}, fmt.Errorf("%w: object name is required", domain.ErrValidation)
	}
	if int64(len(payload)) > store.MaxBytes {
		return domain.ObjectRef{}, fmt.Errorf("%w: payload of %d bytes exceeds %d", domain.ErrValidation, len(payload), store.MaxBytes)
	}
	digest := DigestBytes(payload)
	key := name + "/" + digest
	if prefix := strings.Trim(store.KeyPrefix, "/"); prefix != "" {
		key = prefix + "/" + key
	}
	if err := store.Storage.Upload(ctx, store.Bucket, key, bytes.NewReader(payload), store.contentType()); err != nil {
		return domain.ObjectRef{}, fmt.Errorf("upload %s: %w", key, err)
	}
	return domain.ObjectRef{URI: fmt.Sprintf("%s://%s/%s", store.scheme(), store.Bucket, key), Digest: digest}, nil
}

// Hydrate downloads the referenced object and fails closed on any digest drift.
func (store *ObjectStateStore) Hydrate(ctx context.Context, ref domain.ObjectRef) ([]byte, error) {
	if err := store.validate(); err != nil {
		return nil, err
	}
	bucket, key, err := store.locate(ref)
	if err != nil {
		return nil, err
	}
	reader, err := store.Storage.Download(ctx, bucket, key)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", ref.URI, err)
	}
	defer reader.Close()
	payload, err := io.ReadAll(io.LimitReader(reader, store.MaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", ref.URI, err)
	}
	if int64(len(payload)) > store.MaxBytes {
		return nil, fmt.Errorf("%w: %s exceeds %d bytes", domain.ErrValidation, ref.URI, store.MaxBytes)
	}
	if actual := DigestBytes(payload); actual != ref.Digest {
		return nil, fmt.Errorf("%w: %s stored %s, expected %s", ErrDigestMismatch, ref.URI, actual, ref.Digest)
	}
	return payload, nil
}

// Delete removes the referenced object; callers treat failure as best effort.
func (store *ObjectStateStore) Delete(ctx context.Context, ref domain.ObjectRef) error {
	if err := store.validate(); err != nil {
		return err
	}
	bucket, key, err := store.locate(ref)
	if err != nil {
		return err
	}
	if err := store.Storage.Delete(ctx, bucket, key); err != nil {
		return fmt.Errorf("delete %s: %w", ref.URI, err)
	}
	return nil
}

func (store *ObjectStateStore) locate(ref domain.ObjectRef) (bucket, key string, err error) {
	if len(ref.Digest) != 64 {
		return "", "", fmt.Errorf("%w: object reference %q lacks a sha-256 digest", domain.ErrValidation, ref.URI)
	}
	rest, ok := strings.CutPrefix(ref.URI, store.scheme()+"://")
	if !ok {
		return "", "", fmt.Errorf("%w: object uri %q does not use scheme %q", domain.ErrValidation, ref.URI, store.scheme())
	}
	bucket, key, ok = strings.Cut(rest, "/")
	if !ok || bucket == "" || key == "" {
		return "", "", fmt.Errorf("%w: object uri %q lacks bucket or key", domain.ErrValidation, ref.URI)
	}
	return bucket, key, nil
}

// DigestBytes returns the lowercase hex SHA-256 of payload.
func DigestBytes(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// Deterministic object names; the codec appends the content digest.
func checkpointStateName(tenantID int64, conversationID string, turnNo int64, stepNo int) string {
	return fmt.Sprintf("checkpoint/%d/%s/%d/%d", tenantID, url.PathEscape(conversationID), turnNo, stepNo)
}

func turnResponseName(tenantID int64, conversationID string, turnNo int64) string {
	return fmt.Sprintf("turn/%d/%s/%d/response", tenantID, url.PathEscape(conversationID), turnNo)
}

func stepResultName(tenantID int64, requestID string, executionStepID int64) string {
	return fmt.Sprintf("step/%d/%s/%d", tenantID, url.PathEscape(requestID), executionStepID)
}

var _ ObjectStateCodec = (*ObjectStateStore)(nil)
