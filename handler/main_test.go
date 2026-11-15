package handler

import (
	"os"
	"testing"

	"github.com/nauticana/keel/config"
)

func TestMain(m *testing.M) {
	config.SetConfig(&config.KeelConfig{MaxRequestSize: 1 << 20})
	os.Exit(m.Run())
}
