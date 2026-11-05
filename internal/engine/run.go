package engine

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/eumarumar/concurtest/internal/failure"
)

// RunOutcome classifies a completed scenario evaluation.
type RunOutcome string

const (
	// RunOutcomePassed means the invariant held and the trial met its operation
	// outcome requirements without unknown executions.
	RunOutcomePassed RunOutcome = "passed"
	// RunOutcomeViolated means the recorded evidence demonstrated an invariant
	// violation.
	RunOutcomeViolated RunOutcome = "violated"
	// RunOutcomeInconclusive means the invariant appeared to hold, but the
	// operation outcome requirements were not met.
	RunOutcomeInconclusive RunOutcome = "inconclusive"
)

// MaxSetupSteps bounds preparation requests and their retained response bodies.
const MaxSetupSteps = 100

// SetupStep prepares state before test operations. Name may be empty for a
// legacy single-request setup.
type SetupStep struct {
	Name      string
	Request   HTTPRequest
	Assertion *ResponseAssertion
}

// SetupExecution retains an attempted setup step, including a failed step.
type SetupExecution struct {
	Name      string
	Execution HTTPExecution
	Assertion *AssertionEvaluation
}

// Precondition checks initial state after setup and before any baseline or
// operation. Its request and assertion are independent of the final observer.
type Precondition struct {
	Request   HTTPRequest
	Assertion ResponseAssertion
}

// PreconditionExecution retains the initial-state request and its check even
// when verification fails and no operations are allowed to start.
type PreconditionExecution struct {
	Execution HTTPExecution
	Assertion *AssertionEvaluation
}

// Scenario describes the programmatic scenario supported by the initial
// engine: ordered setup, one repeated operation, an optional observation, and
// one invariant or flat conjunction of existing checks.
type Scenario struct {
	Readiness    *WaitCondition
	Setup        []SetupStep
	Precondition *Precondition
	Operation    Operation
	Attempts     int
	Concurrency  int
	Observation  *HTTPRequest
	Invariant    Invariant
	Outcomes     OutcomePolicy
	Completion   *WaitCondition
}

// EffectiveOutcomes uses a history invariant's explicit response filter for
// acceptance when the outcome policy does not supply its own filter. Otherwise
// the policy's nil filter retains the default 2xx meaning. Conjunctions do not
// inherit an arbitrary member's filter; their outcome policy stays explicit.
func (s Scenario) EffectiveOutcomes() OutcomePolicy {
	policy := s.Outcomes
	if policy.AcceptedStatusCodes == nil && s.Invariant.MaximumSuccessfulAttempts != nil {
		policy.AcceptedStatusCodes = s.Invariant.MaximumSuccessfulAttempts.SuccessfulStatusCodes
	}
	return policy
}

// RunResult records each attempted setup step and later scenario stages.
// Pointer fields are nil when their stage was optional or was not reached.
type RunResult struct {
	StartedAt    time.Time
	CompletedAt  time.Time
	Readiness    *WaitResult
	Completion   *WaitResult
	Setup        []SetupExecution
	Precondition *PreconditionExecution
	// BaselineObservation is captured after setup and before operations only
	// for a change invariant. Each trial and reduction candidate captures its own.
	BaselineObservation *HTTPExecution
	History             History
	Observation         *HTTPExecution
	Evaluation          *InvariantEvaluation
	Outcomes            *OutcomeEvaluation
	Outcome             RunOutcome
}

// Duration reports the elapsed time of the scenario run.
func (r RunResult) Duration() time.Duration {
	return r.CompletedAt.Sub(r.StartedAt)
}

// Run executes a complete programmatic scenario. A non-nil error means the
// engine could not complete every required stage or check. A demonstrated
// violation has RunOutcomeViolated, possibly alongside an error from an
// unreadable sibling check or unverified completion condition.
func Run(
	ctx context.Context,
	client *http.Client,
	scenario Scenario,
) (result RunResult, runErr error) {
	result.StartedAt = time.Now()
	defer func() {
		result.CompletedAt = time.Now()
	}()

	if err := validateRunInput(ctx, client, scenario); err != nil {
		return result, err
	}
	if scenario.Readiness != nil {
		wait := waitForCondition(ctx, client, *scenario.Readiness, "readiness")
		result.Readiness = &wait
		if wait.Err != nil {
			return result, wait.Err
		}
	}

	// Preparation belongs to this goroutine. Complete each response before
	// starting the next step; only ExecuteConcurrent starts operation workers.
	for index, step := range scenario.Setup {
		if err := ctx.Err(); err != nil {
			return result, failure.Wrap(failure.CodeSetupFailed, "prepare scenario", err)
		}
		setup := ExecuteHTTP(ctx, client, step.Request)
		result.Setup = append(result.Setup, SetupExecution{Name: step.Name, Execution: setup})
		label := fmt.Sprintf("setup step %d", index+1)
		if step.Name != "" {
			label += fmt.Sprintf(" (%q)", step.Name)
		}
		if err := requireSuccessfulStage(label, setup); err != nil {
			return result, err
		}
		if err := ctx.Err(); err != nil {
			return result, failure.Wrap(failure.CodeSetupFailed, label+" scenario request", err)
		}
		if step.Assertion != nil {
			evaluation, err := checkResponseAssertion(label, *step.Assertion, setup)
			result.Setup[index].Assertion = evaluation
			if err != nil {
				return result, err
			}
		}
	}
	if definition := scenario.Precondition; definition != nil {
		if err := ctx.Err(); err != nil {
			return result, failure.Wrap(failure.CodePreconditionFailed, "check initial state", err)
		}
		execution := ExecuteHTTP(ctx, client, definition.Request)
		result.Precondition = &PreconditionExecution{Execution: execution}
		if err := requireSuccessfulStage("precondition", execution); err != nil {
			return result, failure.Wrap(failure.CodePreconditionFailed, "check initial state", err)
		}
		if err := ctx.Err(); err != nil {
			return result, failure.Wrap(failure.CodePreconditionFailed, "check initial state", err)
		}
		evaluation, err := checkResponseAssertion("precondition", definition.Assertion, execution)
		result.Precondition.Assertion = evaluation
		if err != nil {
			return result, err
		}
	}

	var conjunctionBaselines []int64
	if scenario.Invariant.All != nil && scenario.Invariant.HasChange() {
		var err error
		conjunctionBaselines, err = captureConjunctionBaselines(ctx, client, scenario, &result)
		if err != nil {
			return result, err
		}
	}
	var baseline *int64
	if definition := scenario.Invariant.JSONInteger; definition != nil && definition.Change != nil {
		observation := ExecuteHTTP(ctx, client, *scenario.Observation)
		result.BaselineObservation = &observation
		if err := requireSuccessfulStage("baseline observation", observation); err != nil {
			return result, err
		}
		if observation.Response.BodyTruncated {
			return result, failure.New(failure.CodeResponseTruncated, "observe baseline state: response body was truncated")
		}
		value, err := readJSONInteger(definition.Path, observation.Response.Body)
		if err != nil {
			return result, failure.Wrap(failure.CodeInvariantEvaluationFailed, "read baseline observation", err)
		}
		baseline = &value
	}

	history, err := ExecuteConcurrent(
		ctx,
		client,
		scenario.Operation,
		scenario.Attempts,
		scenario.Concurrency,
	)
	result.History = history
	if len(history.Attempts) > 0 {
		outcomes, outcomeErr := EvaluateOutcomes(scenario.EffectiveOutcomes(), history)
		if outcomeErr != nil {
			return result, failure.Wrap(failure.CodeInvalidExecution, "evaluate operation outcomes", outcomeErr)
		}
		result.Outcomes = &outcomes
	}
	if err != nil {
		return result, failure.Wrap(failure.CodeOperationBatchFailed, "execute scenario operations", err)
	}

	if scenario.Invariant.MaximumSuccessfulAttempts != nil {
		evaluation, err := EvaluateMaximumSuccessfulAttempts(
			*scenario.Invariant.MaximumSuccessfulAttempts,
			history,
		)
		if err != nil {
			return result, failure.Wrap(failure.CodeInvariantEvaluationFailed, "evaluate scenario invariant", err)
		}
		result.Evaluation = &InvariantEvaluation{
			MaximumSuccessfulAttempts: &evaluation,
			Violated:                  evaluation.Violated,
		}
	}
	if scenario.Invariant.All != nil {
		result.Evaluation = evaluateConjunctionHistory(scenario.Invariant, history)
	}
	if scenario.Completion != nil {
		wait := waitForCondition(ctx, client, *scenario.Completion, "completion")
		result.Completion = &wait
		if wait.Err != nil {
			// A response-count violation is already demonstrated independently
			// of target settlement. Retain it while preventing further work.
			if result.Evaluation != nil && result.Evaluation.Violated {
				result.Outcome = RunOutcomeViolated
			}
			return result, wait.Err
		}
	}
	if scenario.Invariant.All != nil {
		if err := evaluateConjunctionObservation(ctx, client, scenario, conjunctionBaselines, &result); err != nil {
			if result.Evaluation.Violated && ctx.Err() == nil {
				result.Outcome = RunOutcomeViolated
			}
			return result, err
		}
	} else if scenario.Invariant.MaximumSuccessfulAttempts != nil {
		if scenario.Observation != nil {
			observation := ExecuteHTTP(ctx, client, *scenario.Observation)
			result.Observation = &observation
			if observationErr := requireSuccessfulStage("observation", observation); observationErr != nil {
				if ctx.Err() != nil {
					return result, observationErr
				}
				if !result.Evaluation.Violated {
					return result, observationErr
				}
			}
		}
	} else {
		observation := ExecuteHTTP(ctx, client, *scenario.Observation)
		result.Observation = &observation
		if err := requireSuccessfulStage("observation", observation); err != nil {
			return result, err
		}
		if observation.Response.BodyTruncated {
			return result, failure.New(failure.CodeResponseTruncated, "observe scenario state: response body was truncated")
		}

		var evaluation JSONIntegerEvaluation
		var err error
		if baseline != nil {
			evaluation, err = EvaluateJSONIntegerChange(*scenario.Invariant.JSONInteger, *baseline, observation.Response.Body)
		} else {
			evaluation, err = EvaluateJSONInteger(*scenario.Invariant.JSONInteger, observation.Response.Body)
		}
		if err != nil {
			return result, failure.Wrap(failure.CodeInvariantEvaluationFailed, "evaluate scenario invariant", err)
		}
		result.Evaluation = &InvariantEvaluation{
			JSONInteger: &evaluation,
			Violated:    evaluation.Violated,
		}
	}

	switch {
	case result.Evaluation.Violated:
		result.Outcome = RunOutcomeViolated
	case result.Outcomes == nil || !result.Outcomes.Trustworthy():
		result.Outcome = RunOutcomeInconclusive
	default:
		result.Outcome = RunOutcomePassed
	}

	return result, nil
}

func validateRunInput(ctx context.Context, client *http.Client, scenario Scenario) error {
	if len(scenario.Setup) > MaxSetupSteps {
		return failure.New(failure.CodeInvalidExecution, fmt.Sprintf("setup must contain at most %d steps", MaxSetupSteps))
	}
	if err := validateConcurrentInput(
		ctx,
		client,
		scenario.Operation,
		scenario.Attempts,
		scenario.Concurrency,
	); err != nil {
		return failure.Wrap(failure.CodeInvalidExecution, "validate scenario execution", err)
	}
	if err := validateInvariant(scenario.Invariant); err != nil {
		return failure.Wrap(failure.CodeInvariantInvalid, "validate scenario invariant", err)
	}
	if scenario.Invariant.RequiresObservation() && scenario.Observation == nil {
		return failure.New(failure.CodeInvariantInvalid, "validate scenario invariant: JSON integer requires an observation")
	}
	if err := scenario.EffectiveOutcomes().Validate(scenario.Attempts); err != nil {
		return failure.Wrap(failure.CodeInvalidExecution, "validate operation outcomes", err)
	}
	for index, step := range scenario.Setup {
		if step.Assertion != nil {
			if err := step.Assertion.Validate(); err != nil {
				return failure.Wrap(failure.CodeInvalidExecution, fmt.Sprintf("validate setup step %d assertion", index+1), err)
			}
		}
	}
	if scenario.Precondition != nil {
		if err := scenario.Precondition.Assertion.Validate(); err != nil {
			return failure.Wrap(failure.CodeInvalidExecution, "validate precondition assertion", err)
		}
	}
	for _, wait := range []struct {
		name       string
		definition *WaitCondition
	}{{"readiness", scenario.Readiness}, {"completion", scenario.Completion}} {
		if wait.definition != nil {
			if err := wait.definition.Validate(); err != nil {
				return failure.Wrap(failure.CodeInvalidExecution, "validate "+wait.name+" condition", err)
			}
		}
	}
	return nil
}

func requireSuccessfulStage(stage string, execution HTTPExecution) error {
	if execution.Err != nil {
		code := failure.CodeSetupFailed
		if stage == "observation" || stage == "baseline observation" {
			code = failure.CodeObservationFailed
		} else if stage == "precondition" {
			code = failure.CodePreconditionFailed
		} else if stage == "readiness" {
			code = failure.CodeReadinessFailed
		} else if stage == "completion" {
			code = failure.CodeCompletionFailed
		}
		return failure.Wrap(code, stage+" scenario request", execution.Err)
	}
	if execution.Response == nil {
		return failure.New(failure.CodeMissingHTTPResponse, stage+" scenario request: missing HTTP response")
	}
	if execution.Response.StatusCode < http.StatusOK || execution.Response.StatusCode >= http.StatusMultipleChoices {
		return failure.New(failure.CodeUnexpectedHTTPStatus, fmt.Sprintf("%s scenario request: unexpected HTTP status %d", stage, execution.Response.StatusCode))
	}
	return nil
}
