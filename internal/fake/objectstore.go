package fake

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/nauticana/keel/storage"
)

// ObjectStorage is an in-memory keel storage.ObjectStorage. Objects is keyed by
// bucket/key; the optional Func hooks override or fail individual operations.
type ObjectStorage struct {
	mu           sync.Mutex
	Objects      map[string][]byte
	Uploads      []string
	Deletes      []string
	UploadFunc   func(ctx context.Context, bucket, key string, payload []byte) error
	DownloadFunc func(ctx context.Context, bucket, key string) ([]byte, error)
	DeleteFunc   func(ctx context.Context, bucket, key string) error
}

// ObjectKey joins bucket and key the way Objects is indexed.
func ObjectKey(bucket, key string) string { return bucket + "/" + key }

// Upload stores the payload, or delegates to UploadFunc when set.
func (store *ObjectStorage) Upload(ctx context.Context, bucket, key string, reader io.Reader, _ string) error {
	payload, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	if store.UploadFunc != nil {
		return store.UploadFunc(ctx, bucket, key, payload)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.Objects == nil {
		store.Objects = make(map[string][]byte)
	}
	store.Objects[ObjectKey(bucket, key)] = append([]byte(nil), payload...)
	store.Uploads = append(store.Uploads, ObjectKey(bucket, key))
	return nil
}

// Download returns the stored payload, or delegates to DownloadFunc when set.
func (store *ObjectStorage) Download(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	if store.DownloadFunc != nil {
		payload, err := store.DownloadFunc(ctx, bucket, key)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(bytes.NewReader(payload)), nil
	}
	store.mu.Lock()
	payload, ok := store.Objects[ObjectKey(bucket, key)]
	store.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("fake storage: %s not found", ObjectKey(bucket, key))
	}
	return io.NopCloser(bytes.NewReader(payload)), nil
}

// Delete removes the object, or delegates to DeleteFunc when set.
func (store *ObjectStorage) Delete(ctx context.Context, bucket, key string) error {
	if store.DeleteFunc != nil {
		return store.DeleteFunc(ctx, bucket, key)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	delete(store.Objects, ObjectKey(bucket, key))
	store.Deletes = append(store.Deletes, ObjectKey(bucket, key))
	return nil
}

// GetSignedURL returns a stable pseudo-URL.
func (store *ObjectStorage) GetSignedURL(_ context.Context, bucket, key string, _ int) (string, error) {
	return "fake://" + ObjectKey(bucket, key), nil
}

// PublicURL returns a stable pseudo-URL.
func (store *ObjectStorage) PublicURL(bucket, key string) string {
	return "fake://" + ObjectKey(bucket, key)
}

// Payload returns a stored object and whether it exists.
func (store *ObjectStorage) Payload(bucket, key string) ([]byte, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	payload, ok := store.Objects[ObjectKey(bucket, key)]
	return payload, ok
}

// Overwrite replaces a stored object in place, for digest-mismatch tests.
func (store *ObjectStorage) Overwrite(bucket, key string, payload []byte) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.Objects == nil {
		store.Objects = make(map[string][]byte)
	}
	store.Objects[ObjectKey(bucket, key)] = append([]byte(nil), payload...)
}

var _ storage.ObjectStorage = (*ObjectStorage)(nil)
