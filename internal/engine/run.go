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
	// RunOutcomeViolated means the observed state demonstrated an invariant
	// violation.
	RunOutcomeViolated RunOutcome = "violated"
	// RunOutcomeInconclusive means the invariant appeared to hold, but at least
	// one operation did not produce a clean execution result.
	RunOutcomeInconclusive RunOutcome = "inconclusive"
)

// Scenario describes the programmatic scenario supported by the initial
// engine: optional setup, one repeated operation, one observation, and one
// invariant.
type Scenario struct {
	Setup       *HTTPRequest
	Operation   Operation
	Attempts    int
	Concurrency int
	Observation HTTPRequest
	Invariant   JSONIntegerMinimumInvariant
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

	if err := validateConcurrentInput(
		ctx,
		client,
		scenario.Operation,
		scenario.Attempts,
		scenario.Concurrency,
	); err != nil {
		return result, fmt.Errorf("validate scenario execution: %w", err)
	}
	if err := validateJSONIntegerMinimumInvariant(scenario.Invariant); err != nil {
		return result, fmt.Errorf("validate scenario invariant: %w", err)
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

	observation := ExecuteHTTP(ctx, client, scenario.Observation)
	result.Observation = &observation
	if err := requireSuccessfulStage("observation", observation); err != nil {
		return result, err
	}
	if observation.Response.BodyTruncated {
		return result, errors.New("observe scenario state: response body was truncated")
	}

	evaluation, err := EvaluateJSONIntegerMinimum(
		scenario.Invariant,
		observation.Response.Body,
	)
	if err != nil {
		return result, fmt.Errorf("evaluate scenario invariant: %w", err)
	}
	result.Evaluation = &evaluation

	switch {
	case evaluation.Violated:
		result.Outcome = RunOutcomeViolated
	case historyHasExecutionErrors(history):
		result.Outcome = RunOutcomeInconclusive
	default:
		result.Outcome = RunOutcomePassed
	}

	return result, nil
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
		if attempt.Execution == nil || attempt.Execution.Err != nil {
			return true
		}
	}
	return false
}
