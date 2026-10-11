package handler

import (
	"net/http"

	"github.com/nauticana/keel/config"
)

// bounded caps the request body at keel's MaxRequestSize before next reads it.
func bounded(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if limits := config.Config(); limits != nil && limits.MaxRequestSize > 0 && r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, limits.MaxRequestSize)
		}
		next(w, r)
	}
}
