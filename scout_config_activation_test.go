package scout

import (
	"testing"
	"time"
)

func TestReleaseActivationSettingsFromFlags(t *testing.T) {
	cfg := &ScoutConfig{}
	if err := cfg.Apply(scoutConfigRows()); err != nil {
		t.Fatal(err)
	}
	settings := cfg.ReleaseActivationSettings()
	if settings.Batch != 20 || settings.PollPeriod != 30*time.Second || settings.MaxAttempts != 5 {
		t.Fatalf("settings = %+v", settings)
	}
}
