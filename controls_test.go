package scout

import (
	"context"
	"testing"

	"github.com/nauticana/scout/domain"
)

func controlsConfig(t *testing.T) *ScoutConfig {
	t.Helper()
	cfg := &ScoutConfig{}
	if err := cfg.Apply(scoutConfigRows()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return cfg
}

func TestNewControlSetRefusesAnUnusableConfiguration(t *testing.T) {
	if _, err := NewControlSet(&ScoutConfig{}); err == nil {
		t.Fatal("an empty configuration must not build controls")
	}
	if _, err := NewControlSet(controlsConfig(t)); err != nil {
		t.Fatalf("seed defaults: %v", err)
	}
}

func TestActiveControlsFollowThePublishedConfiguration(t *testing.T) {
	previous := Config()
	t.Cleanup(func() { SetConfig(previous) })
	tenant := domain.TenantContext{TenantID: 7}
	controls := ActiveControls()

	SetConfig(&ScoutConfig{})
	if err := controls.RateLimiter.AllowTurn(context.Background(), tenant); err == nil {
		t.Fatal("an unusable configuration must fail admission")
	}
	SetConfig(controlsConfig(t))
	if err := controls.RateLimiter.AllowTurn(context.Background(), tenant); err != nil {
		t.Fatalf("AllowTurn after reload: %v", err)
	}
}
