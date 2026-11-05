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
	return r.FailureDirection != "" && trialDirection(trial) == r.FailureDirection
}

func majorityDirection(result engine.TrialsResult) FailureDirection {
	for _, direction := range []FailureDirection{FailureBelow, FailureAbove} {
		if matchingViolations(result, direction) > result.Requested/2 {
			return direction
		}
	}
	return ""
}

func matchingViolations(result engine.TrialsResult, direction FailureDirection) int {
	count := 0
	for _, trial := range result.Trials {
		if direction != "" && trialDirection(trial) == direction {
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
