package gobench

import "fmt"

// MarkInfra annotates v as an infrastructure/fixture failure. Infra verdicts
// are publication-blocking and must be excluded from resolve-rate denominators.
func MarkInfra(v *Verdict, cause string) {
	if v == nil {
		return
	}
	v.Infra = true
	v.InfraCause = cause
	if v.GraderError == "" {
		v.GraderError = cause
	}
}

// BoardReady rejects verdict sets containing infrastructure failures. Public
// leaderboard publication should call this after grading and before aggregation;
// infra-marked verdicts are not model/task outcomes.
func BoardReady(verdicts []Verdict) error {
	for _, v := range verdicts {
		if v.Infra {
			cause := v.InfraCause
			if cause == "" {
				cause = v.GraderError
			}
			if cause == "" {
				cause = "unspecified infra failure"
			}
			return fmt.Errorf("board not ready: %s has infrastructure failure: %s", v.InstanceID, cause)
		}
	}
	return nil
}

// ResolveDenominator returns the number of verdicts eligible for resolve-rate
// aggregation. Infra failures are excluded because they are fixture/harness
// failures, not model outcomes.
func ResolveDenominator(verdicts []Verdict) int {
	n := 0
	for _, v := range verdicts {
		if !v.Infra {
			n++
		}
	}
	return n
}
