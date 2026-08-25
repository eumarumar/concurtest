package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// RunOutcome classifies a completed scenario evaluation.
type RunOutcome string

const (
	// RunOutcomePassed means the invariant held and every operation completed
	// without an execution error.
	RunOutcomePassed RunOutcome = "passed"
	// RunOutcomeViolated means the recorded evidence demonstrated an invariant
	// violation.
	RunOutcomeViolated RunOutcome = "violated"
	// RunOutcomeInconclusive means the invariant appeared to hold, but at least
	// one operation did not produce a clean execution result.
	RunOutcomeInconclusive RunOutcome = "inconclusive"
)

// Scenario describes the programmatic scenario supported by the initial
// engine: optional setup, one repeated operation, an optional observation, and
// one invariant.
type Scenario struct {
	Setup       *HTTPRequest
	Operation   Operation
	Attempts    int
	Concurrency int
	Observation *HTTPRequest
	Invariant   Invariant
}

// RunResult records each completed stage of a scenario run. Pointer fields are
// nil when their stage was optional or was not reached.
type RunResult struct {
	StartedAt   time.Time
	CompletedAt time.Time
	Setup       *HTTPExecution
	History     History
	Observation *HTTPExecution
	Evaluation  *InvariantEvaluation
	Outcome     RunOutcome
}

// Duration reports the elapsed time of the scenario run.
func (r RunResult) Duration() time.Duration {
	return r.CompletedAt.Sub(r.StartedAt)
}

// Run executes a complete programmatic scenario. A non-nil error means the
// engine could not complete a trustworthy invariant evaluation. An invariant
// violation is represented by RunOutcomeViolated with a nil error.
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

	if scenario.Setup != nil {
		setup := ExecuteHTTP(ctx, client, *scenario.Setup)
		result.Setup = &setup
		if err := requireSuccessfulStage("setup", setup); err != nil {
			return result, err
		}
	}

	history, err := ExecuteConcurrent(
		ctx,
		client,
		scenario.Operation,
		scenario.Attempts,
		scenario.Concurrency,
	)
	result.History = history
	if err != nil {
		return result, fmt.Errorf("execute scenario operations: %w", err)
	}

	if scenario.Invariant.MaximumSuccessfulAttempts != nil {
		evaluation, err := EvaluateMaximumSuccessfulAttempts(
			*scenario.Invariant.MaximumSuccessfulAttempts,
			history,
		)
		if err != nil {
			return result, fmt.Errorf("evaluate scenario invariant: %w", err)
		}
		result.Evaluation = &InvariantEvaluation{
			MaximumSuccessfulAttempts: &evaluation,
			Violated:                  evaluation.Violated,
		}

		if scenario.Observation != nil {
			observation := ExecuteHTTP(ctx, client, *scenario.Observation)
			result.Observation = &observation
			if observationErr := requireSuccessfulStage("observation", observation); observationErr != nil {
				if ctx.Err() != nil {
					return result, observationErr
				}
				if !evaluation.Violated {
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
			return result, errors.New("observe scenario state: response body was truncated")
		}

		evaluation, err := EvaluateJSONIntegerMinimum(
			*scenario.Invariant.JSONIntegerMinimum,
			observation.Response.Body,
		)
		if err != nil {
			return result, fmt.Errorf("evaluate scenario invariant: %w", err)
		}
		result.Evaluation = &InvariantEvaluation{
			JSONIntegerMinimum: &evaluation,
			Violated:           evaluation.Violated,
		}
	}

	switch {
	case result.Evaluation.Violated:
		result.Outcome = RunOutcomeViolated
	case historyHasExecutionErrors(history):
		result.Outcome = RunOutcomeInconclusive
	default:
		result.Outcome = RunOutcomePassed
	}

	return result, nil
}

func validateRunInput(ctx context.Context, client *http.Client, scenario Scenario) error {
	if err := validateConcurrentInput(
		ctx,
		client,
		scenario.Operation,
		scenario.Attempts,
		scenario.Concurrency,
	); err != nil {
		return fmt.Errorf("validate scenario execution: %w", err)
	}
	if err := validateInvariant(scenario.Invariant); err != nil {
		return fmt.Errorf("validate scenario invariant: %w", err)
	}
	if scenario.Invariant.JSONIntegerMinimum != nil && scenario.Observation == nil {
		return errors.New("validate scenario invariant: JSON integer minimum requires an observation")
	}
	return nil
}

func requireSuccessfulStage(stage string, execution HTTPExecution) error {
	if execution.Err != nil {
		return fmt.Errorf("%s scenario request: %w", stage, execution.Err)
	}
	if execution.Response == nil {
		return fmt.Errorf("%s scenario request: missing HTTP response", stage)
	}
	if execution.Response.StatusCode < http.StatusOK || execution.Response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf(
			"%s scenario request: unexpected HTTP status %d",
			stage,
			execution.Response.StatusCode,
		)
	}
	return nil
}

func historyHasExecutionErrors(history History) bool {
	for _, attempt := range history.Attempts {
		if attempt.Execution == nil || attempt.Execution.Err != nil || attempt.Execution.Response == nil {
			return true
		}
	}
	return false
}
