package domain

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
