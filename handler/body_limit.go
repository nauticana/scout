package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/nauticana/keel/config"
	keelhandler "github.com/nauticana/keel/handler"
)

// bounded caps the request body at keel's MaxRequestSize and refuses a body that is not a
// single JSON value before next reads it, since keel's JSON helper stops at the first value.
func bounded(h *keelhandler.AbstractHandler, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil {
			next(w, r)
			return
		}
		if limits := config.Config(); limits != nil && limits.MaxRequestSize > 0 {
			r.Body = http.MaxBytesReader(w, r.Body, limits.MaxRequestSize)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || !singleJSONValue(body) {
			h.WriteError(w, http.StatusBadRequest, "Bad Request", "invalid JSON request body")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		next(w, r)
	}
}

// singleJSONValue reports whether body is empty or holds exactly one JSON value.
func singleJSONValue(body []byte) bool {
	if len(bytes.TrimSpace(body)) == 0 {
		return true
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	var value json.RawMessage
	if decoder.Decode(&value) != nil {
		return false
	}
	_, err := decoder.Token()
	return err == io.EOF
}
