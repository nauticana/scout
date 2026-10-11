package evaluation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nauticana/keel/storage"

	"github.com/nauticana/scout/domain"
)

// putContent stores content under prefix/<sha256> unless an object already holds it there,
// and returns the reference; scheme prefixes the URI, empty meaning "object".
func putContent(ctx context.Context, objects storage.ObjectStorage, scheme, prefix string, content []byte) (domain.ObjectRef, error) {
	if objects == nil || strings.TrimSpace(objects.Bucket()) == "" {
		return domain.ObjectRef{}, fmt.Errorf("object storage with a bucket is required")
	}
	digest := sha256Hex(content)
	key := prefix + "/" + digest
	if err := objects.PutObjectIfAbsent(ctx, key, bytes.NewReader(content), "application/octet-stream", nil); err != nil && !errors.Is(err, storage.ErrExists) {
		return domain.ObjectRef{}, err
	}
	if scheme == "" {
		scheme = "object"
	}
	return domain.ObjectRef{URI: scheme + "://" + objects.Bucket() + "/" + key, Digest: digest}, nil
}
