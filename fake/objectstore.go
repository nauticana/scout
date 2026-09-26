package fake

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/nauticana/keel/storage"
)

// ObjectStorage is an in-memory keel storage.ObjectStorage bound to the bucket
// Name. Objects is keyed by object key; the optional Func hooks override or
// fail individual operations.
type ObjectStorage struct {
	Name       string
	mu         sync.Mutex
	Objects    map[string][]byte
	Attributes map[string]map[string]string
	Uploads    []string
	Deletes    []string
	PutFunc    func(ctx context.Context, key string, payload []byte) error
	GetFunc    func(ctx context.Context, key string) ([]byte, error)
	DeleteFunc func(ctx context.Context, key string) error
}

var _ storage.ObjectStorage = (*ObjectStorage)(nil)

func (store *ObjectStorage) Bucket() string { return store.Name }

// PutObject stores the payload, or delegates to PutFunc when set.
func (store *ObjectStorage) PutObject(ctx context.Context, key string, reader io.Reader, _ string, attributes map[string]string) error {
	return store.put(ctx, key, reader, attributes, false)
}

func (store *ObjectStorage) PutObjectIfAbsent(ctx context.Context, key string, reader io.Reader, _ string, attributes map[string]string) error {
	return store.put(ctx, key, reader, attributes, true)
}

func (store *ObjectStorage) put(ctx context.Context, key string, reader io.Reader, attributes map[string]string, ifAbsent bool) error {
	payload, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	if store.PutFunc != nil {
		return store.PutFunc(ctx, key, payload)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, exists := store.Objects[key]; ifAbsent && exists {
		return fmt.Errorf("fake storage %s/%s: %w", store.Name, key, storage.ErrExists)
	}
	if store.Objects == nil {
		store.Objects = make(map[string][]byte)
	}
	store.Objects[key] = append([]byte(nil), payload...)
	store.setAttributes(key, attributes)
	store.Uploads = append(store.Uploads, key)
	return nil
}

func (store *ObjectStorage) setAttributes(key string, attributes map[string]string) {
	if store.Attributes == nil {
		store.Attributes = make(map[string]map[string]string)
	}
	copied := make(map[string]string, len(attributes))
	for name, value := range attributes {
		copied[name] = value
	}
	store.Attributes[key] = copied
}

// GetObject returns the stored payload, or delegates to GetFunc when set.
func (store *ObjectStorage) GetObject(ctx context.Context, key string) (io.ReadCloser, error) {
	if store.GetFunc != nil {
		payload, err := store.GetFunc(ctx, key)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(bytes.NewReader(payload)), nil
	}
	payload, err := store.payload(key)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(payload)), nil
}

func (store *ObjectStorage) payload(key string) ([]byte, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	payload, ok := store.Objects[key]
	if !ok {
		return nil, fmt.Errorf("fake storage %s/%s: %w", store.Name, key, storage.ErrNotFound)
	}
	return payload, nil
}

// DeleteObject removes the object, or delegates to DeleteFunc when set.
func (store *ObjectStorage) DeleteObject(ctx context.Context, key string) error {
	if store.DeleteFunc != nil {
		return store.DeleteFunc(ctx, key)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	delete(store.Objects, key)
	delete(store.Attributes, key)
	store.Deletes = append(store.Deletes, key)
	return nil
}

func (store *ObjectStorage) ListObjects(_ context.Context, prefix string, limit int) ([]string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	var keys []string
	for key := range store.Objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if limit > 0 && len(keys) > limit {
		keys = keys[:limit]
	}
	return keys, nil
}

func (store *ObjectStorage) ListPrefixes(ctx context.Context, prefix string, limit int) ([]string, error) {
	keys, err := store.ListObjects(ctx, prefix, 0)
	if err != nil {
		return nil, err
	}
	var names []string
	seen := map[string]bool{}
	for _, key := range keys {
		name, _, isFolder := strings.Cut(strings.TrimPrefix(key, prefix), "/")
		if isFolder && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	if limit > 0 && len(names) > limit {
		names = names[:limit]
	}
	return names, nil
}

func (store *ObjectStorage) SetObjectAttributes(_ context.Context, key string, attributes map[string]string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, ok := store.Objects[key]; !ok {
		return fmt.Errorf("fake storage %s/%s: %w", store.Name, key, storage.ErrNotFound)
	}
	store.setAttributes(key, attributes)
	return nil
}

func (store *ObjectStorage) GetObjectAttributes(_ context.Context, key string) (map[string]string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, ok := store.Objects[key]; !ok {
		return nil, fmt.Errorf("fake storage %s/%s: %w", store.Name, key, storage.ErrNotFound)
	}
	return store.Attributes[key], nil
}

func (store *ObjectStorage) GetObjectAndAttributes(ctx context.Context, key string) (*storage.Component, error) {
	payload, err := store.payload(key)
	if err != nil {
		return nil, err
	}
	attributes, err := store.GetObjectAttributes(ctx, key)
	if err != nil {
		return nil, err
	}
	return storage.NewComponent(payload, attributes), nil
}

// GetSignedURL returns a stable pseudo-URL.
func (store *ObjectStorage) GetSignedURL(_ context.Context, key string, _ int) (string, error) {
	return store.PublicURL(key), nil
}

// PublicURL returns a stable pseudo-URL.
func (store *ObjectStorage) PublicURL(key string) string {
	return "fake://" + store.Name + "/" + key
}

// Payload returns a stored object and whether it exists.
func (store *ObjectStorage) Payload(key string) ([]byte, bool) {
	payload, err := store.payload(key)
	return payload, err == nil
}

// Overwrite replaces a stored object in place, for digest-mismatch tests.
func (store *ObjectStorage) Overwrite(key string, payload []byte) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.Objects == nil {
		store.Objects = make(map[string][]byte)
	}
	store.Objects[key] = append([]byte(nil), payload...)
}
