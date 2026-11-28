package provider

import (
	"fmt"
	"io"

	"github.com/nauticana/scout/domain"
)

// boundedBody fails a response read past remaining bytes and releases its endpoint slot on Close.
type boundedBody struct {
	body      io.ReadCloser
	remaining int64
	release   func()
}

func (body *boundedBody) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	if body.remaining <= 0 {
		var probe [1]byte
		n, err := body.body.Read(probe[:])
		if n > 0 {
			return 0, fmt.Errorf("%w: model endpoint response exceeds its size bound", domain.ErrInvalidModelOutput)
		}
		return 0, err
	}
	if int64(len(buffer)) > body.remaining {
		buffer = buffer[:body.remaining]
	}
	n, err := body.body.Read(buffer)
	body.remaining -= int64(n)
	return n, err
}

func (body *boundedBody) Close() error {
	defer body.release()
	return body.body.Close()
}
