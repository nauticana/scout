package domain

import (
	"errors"
	"testing"
)

func TestObjectAddressRoundTripsDirectoryBucket(t *testing.T) {
	address := ObjectAddress{Scheme: "scout", Bucket: "/opt/app/state", Key: "turn/1/input/abc"}
	uri := address.String()
	if uri != "scout://%2Fopt%2Fapp%2Fstate/turn/1/input/abc" {
		t.Fatalf("uri = %s", uri)
	}
	if parsed, err := ParseObjectAddress(uri); err != nil || parsed != address {
		t.Fatalf("parsed = %+v, err = %v", parsed, err)
	}
	if parsed, err := ParseObjectAddress("s3://bucket/a/b"); err != nil || parsed.Bucket != "bucket" || parsed.Key != "a/b" {
		t.Fatalf("parsed = %+v, err = %v", parsed, err)
	}
}

func TestParseObjectAddressRefusesMalformed(t *testing.T) {
	for _, uri := range []string{"", "bucket/key", "://b/k", "s://b", "s://b/", "s:///k", "s://%zz/k"} {
		if _, err := ParseObjectAddress(uri); !errors.Is(err, ErrValidation) {
			t.Fatalf("%q: err = %v", uri, err)
		}
	}
}
