package domain

import "time"

// ContractTestCase defines one agent compatibility assertion.
type ContractTestCase struct {
	TestCaseID   string
	AgentID      string
	AgentVersion string
	Input        []byte
	Assertions   []byte
}

// ContractTestResult contains the outcome of one compatibility test.
type ContractTestResult struct {
	TestCaseID string
	Passed     bool
	Failures   []string
}

// RolloutTarget identifies a platform build and tenant rollout ring.
type RolloutTarget struct {
	PlatformVersion string
	TenantRing      string
	Percentage      int
}

// RolloutVerdict is a three-state health outcome; inconclusive pauses promotion.
type RolloutVerdict string

const (
	RolloutHealthy      RolloutVerdict = "healthy"
	RolloutUnhealthy    RolloutVerdict = "unhealthy"
	RolloutInconclusive RolloutVerdict = "inconclusive"
)

// RolloutHealth carries a verdict with the evidence behind it.
type RolloutHealth struct {
	Verdict        RolloutVerdict
	BreachedMetric string
	Samples        int64
	Window         time.Duration
}
