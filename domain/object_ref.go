package domain

import (
	"fmt"
	"net/url"
	"strings"
)

// ObjectRef points at content kept in object storage; relational rows store
// only the reference and the digest that verifies it.
type ObjectRef struct {
	URI    string
	Digest string
}

// ObjectAddress is the parsed form of an ObjectRef URI, "<scheme>://<bucket>/<key>".
// The bucket is path-escaped so a file-storage bucket naming a directory stays one segment.
type ObjectAddress struct {
	Scheme string
	Bucket string
	Key    string
}

// ParseObjectAddress reads "<scheme>://<bucket>/<key>".
func ParseObjectAddress(uri string) (ObjectAddress, error) {
	scheme, rest, ok := strings.Cut(uri, "://")
	bucket, key, found := strings.Cut(rest, "/")
	if ok && found {
		bucket, err := url.PathUnescape(bucket)
		if err == nil && scheme != "" && bucket != "" && key != "" {
			return ObjectAddress{Scheme: scheme, Bucket: bucket, Key: key}, nil
		}
	}
	return ObjectAddress{}, fmt.Errorf("%w: object URI %q must be <scheme>://bucket/key", ErrValidation, uri)
}

func (address ObjectAddress) String() string {
	return address.Scheme + "://" + url.PathEscape(address.Bucket) + "/" + address.Key
}
