package knowledge

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/nauticana/keel/storage"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// ObjectStorageLoader reads <scheme>://bucket/key objects from keel object storage: knowledge sources and any other object reference.
type ObjectStorageLoader struct {
	Storage storage.ObjectStorage
	// Schemes restricts accepted URI schemes; empty accepts any scheme.
	Schemes []string
	// MaxBytes bounds one source object; default 32 MiB.
	MaxBytes int64
}

var (
	_ contract.SourceLoader = (*ObjectStorageLoader)(nil)
	_ contract.ObjectReader = (*ObjectStorageLoader)(nil)
)

const defaultMaxSourceBytes = 32 << 20

// ParseObjectURI splits <scheme>://bucket/key into its parts.
func ParseObjectURI(uri string) (scheme, bucket, key string, err error) {
	scheme, rest, ok := strings.Cut(uri, "://")
	if !ok || scheme == "" {
		return "", "", "", fmt.Errorf("%w: object URI %q must be <scheme>://bucket/key", domain.ErrValidation, uri)
	}
	bucket, key, ok = strings.Cut(rest, "/")
	if !ok || bucket == "" || key == "" {
		return "", "", "", fmt.Errorf("%w: object URI %q must be <scheme>://bucket/key", domain.ErrValidation, uri)
	}
	return scheme, bucket, key, nil
}

// Load downloads the object; the pipeline verifies the digest.
func (loader *ObjectStorageLoader) Load(ctx context.Context, document domain.KnowledgeDocument) ([]byte, error) {
	return loader.download(ctx, document.SourceURI)
}

// Read downloads the referenced object; the caller verifies the digest.
func (loader *ObjectStorageLoader) Read(ctx context.Context, ref domain.ObjectRef) ([]byte, error) {
	return loader.download(ctx, ref.URI)
}

func (loader *ObjectStorageLoader) download(ctx context.Context, uri string) ([]byte, error) {
	if loader.Storage == nil {
		return nil, fmt.Errorf("object storage loader: storage is required")
	}
	if loader.MaxBytes < 0 {
		return nil, fmt.Errorf("%w: max bytes cannot be negative", domain.ErrValidation)
	}
	scheme, bucket, key, err := ParseObjectURI(uri)
	if err != nil {
		return nil, err
	}
	if len(loader.Schemes) > 0 && !containsFold(loader.Schemes, scheme) {
		return nil, fmt.Errorf("%w: URI scheme %q is not accepted", domain.ErrValidation, scheme)
	}
	maxBytes := loader.MaxBytes
	if maxBytes == 0 {
		maxBytes = defaultMaxSourceBytes
	}
	reader, err := loader.Storage.Download(ctx, bucket, key)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", uri, err)
	}
	defer reader.Close()
	raw, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", uri, err)
	}
	if int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf("%w: object %s exceeds %d bytes", domain.ErrValidation, uri, maxBytes)
	}
	return raw, nil
}

func containsFold(values []string, value string) bool {
	for _, candidate := range values {
		if strings.EqualFold(candidate, value) {
			return true
		}
	}
	return false
}
