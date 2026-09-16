package charter

import (
	"github.com/nauticana/charter/sdk/model"
	"github.com/nauticana/charter/sdk/validate"
)

// Claim runs Charter's conformance manifest against Scout's runtime and adapter subjects and returns the claim;
// rules the subjects do not exercise are reported not tested, never passed (CHR-CONF-014).
func Claim(confDir string, runtime validate.Subject, adapter validate.AdapterSubject, spec validate.ClaimSpec) (model.ConformanceClaim, error) {
	runner, err := validate.NewRunner(confDir)
	if err != nil {
		return model.ConformanceClaim{}, err
	}
	runner.Subject, runner.Adapter = runtime, adapter
	return runner.Claim(spec)
}
