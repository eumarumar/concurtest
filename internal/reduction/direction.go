package reduction

import "github.com/eumarumar/concurtest/internal/engine"

// FailureDirection identifies which side of an invariant's constraint failed.
// For change invariants it compares the evaluated change, not the final value.
type FailureDirection string

const (
	// FailureBelow means an observed value or change fell below its constraint.
	FailureBelow FailureDirection = "below"
	// FailureAbove includes values, changes, and response counts above a constraint.
	FailureAbove FailureDirection = "above"
)

// MatchesFailure reports whether a trial reproduces the baseline's majority
// direction. Reduction varies only attempts and concurrency; the invariant's
// name, kind, path, status filter, and constraints remain fixed by construction.
// Direction is coarse evidence, not a causal failure identity.
func (r Result) MatchesFailure(trial engine.TrialResult) bool {
	if r.FailureCheck != "" {
		if trial.Run.Evaluation == nil {
			return false
		}
		for _, check := range trial.Run.Evaluation.All {
			if check.Name == r.FailureCheck {
				if check.Err != nil || check.Evaluation == nil {
					return false
				}
				trial.Run.Evaluation = check.Evaluation
				return r.FailureDirection != "" && trialDirection(trial) == r.FailureDirection
			}
		}
		return false
	}
	return r.FailureDirection != "" && trialDirection(trial) == r.FailureDirection
}

// Each member keeps its identity. A majority of unrelated failing members is
// insufficient; choose the first declared member with a direction majority.
func majorityFailure(result engine.TrialsResult) (FailureDirection, string) {
	names := []string{""}
	if len(result.Trials) > 0 && result.Trials[0].Run.Evaluation != nil && result.Trials[0].Run.Evaluation.All != nil {
		names = nil
		for _, check := range result.Trials[0].Run.Evaluation.All {
			names = append(names, check.Name)
		}
	}
	for _, name := range names {
		for _, direction := range []FailureDirection{FailureBelow, FailureAbove} {
			if matchingViolations(result, direction, name) > result.Requested/2 {
				return direction, name
			}
		}
	}
	return "", ""
}

func matchingViolations(result engine.TrialsResult, direction FailureDirection, name string) int {
	count := 0
	target := Result{FailureDirection: direction, FailureCheck: name}
	for _, trial := range result.Trials {
		if target.MatchesFailure(trial) {
			count++
		}
	}
	return count
}

func trialDirection(trial engine.TrialResult) FailureDirection {
	if trial.Status != engine.TrialStatusViolated || trial.Run.Evaluation == nil || !trial.Run.Evaluation.Violated {
		return ""
	}
	evaluation := trial.Run.Evaluation
	if value := evaluation.JSONInteger; value != nil && value.Violated {
		constraint := value.Invariant
		switch {
		case constraint.Change != nil:
			if value.Change != nil {
				return compareDirection(*value.Change, *constraint.Change)
			}
		case constraint.Equals != nil:
			return compareDirection(value.Observed, *constraint.Equals)
		case constraint.Minimum != nil && value.Observed < *constraint.Minimum:
			return FailureBelow
		case constraint.Maximum != nil && value.Observed > *constraint.Maximum:
			return FailureAbove
		}
	}
	if value := evaluation.MaximumSuccessfulAttempts; value != nil && value.Violated {
		return FailureAbove
	}
	return ""
}

func compareDirection(observed, expected int64) FailureDirection {
	// Compare directly: subtracting the constraint could overflow int64.
	if observed < expected {
		return FailureBelow
	}
	if observed > expected {
		return FailureAbove
	}
	return ""
}
