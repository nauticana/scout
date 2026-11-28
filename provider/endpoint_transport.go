package provider

import (
	"fmt"
	"net/http"
	"sync"
)

// endpointTransport bounds a registered endpoint: a call holds one of slots until its response
// body is closed, so a streamed reply keeps its slot; a body larger than maxBytes fails the read.
type endpointTransport struct {
	base     http.RoundTripper
	slots    chan struct{}
	maxBytes int64
}

func (transport *endpointTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	release := func() {}
	if transport.slots != nil {
		select {
		case transport.slots <- struct{}{}:
			var once sync.Once
			release = func() { once.Do(func() { <-transport.slots }) }
		case <-request.Context().Done():
			return nil, fmt.Errorf("model endpoint slot: %w", request.Context().Err())
		}
	}
	response, err := transport.base.RoundTrip(request)
	if err != nil {
		release()
		return nil, err
	}
	response.Body = &boundedBody{body: response.Body, remaining: transport.maxBytes, release: release}
	return response, nil
}
