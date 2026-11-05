package engine

import (
	"context"
	"fmt"
	"net/http"

	"github.com/eumarumar/concurtest/internal/failure"
)

// All change checks read the same pre-operation document. A missing baseline
// stops the run before any operation; no check borrows another trial's state.
func captureConjunctionBaselines(ctx context.Context, client *http.Client, scenario Scenario, result *RunResult) ([]int64, error) {
	observation := ExecuteHTTP(ctx, client, *scenario.Observation)
	result.BaselineObservation = &observation
	if err := requireSuccessfulStage("baseline observation", observation); err != nil {
		return nil, err
	}
	if observation.Response.BodyTruncated {
		return nil, failure.New(failure.CodeResponseTruncated, "observe baseline state: response body was truncated")
	}
	values := make([]int64, len(scenario.Invariant.All))
	for index, check := range scenario.Invariant.All {
		if check.JSONInteger == nil || check.JSONInteger.Change == nil {
			continue
		}
		value, err := readJSONInteger(check.JSONInteger.Path, observation.Response.Body)
		if err != nil {
			return nil, checkError(check.Name(), err)
		}
		values[index] = value
	}
	return values, nil
}

// The run goroutine owns all check evaluation. History checks complete before
// the completion wait, so a demonstrated response violation survives a failed
// wait. State checks remain unattempted until settlement has been verified.
func evaluateConjunctionHistory(invariant Invariant, history History) *InvariantEvaluation {
	result := &InvariantEvaluation{All: make([]CheckEvaluation, len(invariant.All))}
	for index, check := range invariant.All {
		member := &result.All[index]
		member.Name = check.Name()
		if check.MaximumSuccessfulAttempts == nil {
			continue
		}
		evaluation, err := EvaluateMaximumSuccessfulAttempts(*check.MaximumSuccessfulAttempts, history)
		if err != nil {
			member.Err = checkError(member.Name, err)
			continue
		}
		member.Evaluation = &InvariantEvaluation{MaximumSuccessfulAttempts: &evaluation, Violated: evaluation.Violated}
		result.Violated = result.Violated || evaluation.Violated
	}
	return result
}

// One final exchange serves every state check. Unreadable members retain errors;
// evaluation continues so a later valid check can still demonstrate a violation.
func evaluateConjunctionObservation(ctx context.Context, client *http.Client, scenario Scenario, baselines []int64, result *RunResult) error {
	var observationErr error
	if scenario.Observation != nil {
		observation := ExecuteHTTP(ctx, client, *scenario.Observation)
		result.Observation = &observation
		observationErr = requireSuccessfulStage("observation", observation)
		if observationErr == nil && observation.Response.BodyTruncated && scenario.Invariant.RequiresObservation() {
			observationErr = failure.New(failure.CodeResponseTruncated, "observe scenario state: response body was truncated")
		}
	}
	var errs []error
	for index, check := range scenario.Invariant.All {
		member := &result.Evaluation.All[index]
		if check.JSONInteger != nil {
			if observationErr != nil {
				member.Err = checkError(member.Name, observationErr)
			} else {
				var evaluation JSONIntegerEvaluation
				var err error
				if check.JSONInteger.Change != nil {
					evaluation, err = EvaluateJSONIntegerChange(*check.JSONInteger, baselines[index], result.Observation.Response.Body)
				} else {
					evaluation, err = EvaluateJSONInteger(*check.JSONInteger, result.Observation.Response.Body)
				}
				if err != nil {
					member.Err = checkError(member.Name, err)
				} else {
					member.Evaluation = &InvariantEvaluation{JSONInteger: &evaluation, Violated: evaluation.Violated}
					result.Evaluation.Violated = result.Evaluation.Violated || evaluation.Violated
				}
			}
		}
		if member.Err != nil {
			errs = append(errs, member.Err)
		}
	}
	if !scenario.Invariant.RequiresObservation() && observationErr != nil {
		errs = append(errs, observationErr)
	}
	if len(errs) > 0 {
		return failure.Join(failure.CodeInvariantEvaluationFailed, "evaluate required checks", errs...)
	}
	return nil
}

func checkError(name string, err error) error {
	return failure.Wrap(failure.CodeInvariantEvaluationFailed, fmt.Sprintf("evaluate check %q", name), err)
}
