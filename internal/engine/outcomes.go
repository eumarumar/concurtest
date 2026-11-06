package engine

import (
	"fmt"
	"slices"
)

// OutcomePolicy describes usable HTTP operation outcomes independently of the
// invariant. Acceptance is a response contract, not proof of committed effects.
// The zero value accepts 2xx, expects no rejections, and requires one acceptance.
type OutcomePolicy struct {
	AcceptedStatusCodes     []int
	RejectedStatusCodes     []int
	MinimumAcceptedAttempts *int
}

// MinimumAccepted returns the effective per-trial acceptance requirement.
func (p OutcomePolicy) MinimumAccepted() int {
	if p.MinimumAcceptedAttempts == nil {
		return 1
	}
	return *p.MinimumAcceptedAttempts
}

// Validate rejects ambiguous status classifications and impossible requirements.
func (p OutcomePolicy) Validate(attempts int) error {
	if minimum := p.MinimumAccepted(); minimum < 0 || minimum > attempts {
		return fmt.Errorf("outcomes.minimum_accepted_attempts must be between 0 and execution.attempts (%d)", attempts)
	}
	if p.AcceptedStatusCodes != nil && len(p.AcceptedStatusCodes) == 0 {
		return fmt.Errorf("outcomes.accepted_status_codes must not be empty")
	}
	for _, list := range []struct {
		name   string
		values []int
	}{{"accepted_status_codes", p.AcceptedStatusCodes}, {"rejected_status_codes", p.RejectedStatusCodes}} {
		seen := make(map[int]bool, len(list.values))
		for _, status := range list.values {
			if status < 100 || status > 599 {
				return fmt.Errorf("outcomes.%s entries must be HTTP status codes between 100 and 599", list.name)
			}
			if seen[status] {
				return fmt.Errorf("outcomes.%s repeats HTTP status %d", list.name, status)
			}
			seen[status] = true
		}
	}
	for _, status := range p.RejectedStatusCodes {
		if successfulStatusCode(p.AcceptedStatusCodes, status) {
			return fmt.Errorf("outcomes: HTTP status %d cannot be both accepted and rejected", status)
		}
	}
	return nil
}

// OutcomeEvaluation partitions stable attempt IDs. Unknown means an absent or
// failed exchange; unexpected means a complete response outside the policy.
// Neither category establishes whether target work committed.
type OutcomeEvaluation struct {
	Policy               OutcomePolicy
	AcceptedAttemptIDs   []int
	RejectedAttemptIDs   []int
	UnexpectedAttemptIDs []int
	UnknownAttemptIDs    []int
}

// Trustworthy reports whether the required response coverage was established.
// It is independent of whether the declared invariant was violated.
func (e OutcomeEvaluation) Trustworthy() bool {
	return len(e.UnexpectedAttemptIDs) == 0 && len(e.UnknownAttemptIDs) == 0 &&
		len(e.AcceptedAttemptIDs) >= e.Policy.MinimumAccepted()
}

// EvaluateOutcomes classifies operation evidence without altering HTTP errors
// or interpreting response bodies as business effects.
func EvaluateOutcomes(policy OutcomePolicy, history History) (OutcomeEvaluation, error) {
	if err := policy.Validate(len(history.Attempts)); err != nil {
		return OutcomeEvaluation{}, err
	}
	policy.AcceptedStatusCodes = slices.Clone(policy.AcceptedStatusCodes)
	policy.RejectedStatusCodes = slices.Clone(policy.RejectedStatusCodes)
	policy.MinimumAcceptedAttempts = new(policy.MinimumAccepted())
	result := OutcomeEvaluation{Policy: policy}
	for _, attempt := range history.Attempts {
		execution := attempt.Execution
		switch {
		case execution == nil || execution.Err != nil || execution.Response == nil:
			result.UnknownAttemptIDs = append(result.UnknownAttemptIDs, attempt.ID)
		case successfulStatusCode(policy.AcceptedStatusCodes, execution.Response.StatusCode):
			result.AcceptedAttemptIDs = append(result.AcceptedAttemptIDs, attempt.ID)
		case slices.Contains(policy.RejectedStatusCodes, execution.Response.StatusCode):
			result.RejectedAttemptIDs = append(result.RejectedAttemptIDs, attempt.ID)
		default:
			result.UnexpectedAttemptIDs = append(result.UnexpectedAttemptIDs, attempt.ID)
		}
	}
	return result, nil
}
