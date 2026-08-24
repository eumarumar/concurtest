package engine

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// MaxTrials is the largest trial sequence accepted by RunTrials.
const MaxTrials = 100

// TrialStatus classifies one trial or an aggregate of recorded trials.
type TrialStatus string

const (
	// TrialStatusPassed means the invariant held after clean operations.
	TrialStatusPassed TrialStatus = "passed"
	// TrialStatusViolated means the trial demonstrated an invariant violation.
	TrialStatusViolated TrialStatus = "violated"
	// TrialStatusInconclusive means the invariant held but operation evidence
	// was incomplete.
	TrialStatusInconclusive TrialStatus = "inconclusive"
	// TrialStatusErrored means the trial could not complete a trustworthy
	// evaluation.
	TrialStatusErrored TrialStatus = "errored"
)

// TrialResult retains the complete evidence and any error from one trial.
type TrialResult struct {
	Number int
	Status TrialStatus
	Run    RunResult
	Err    error
}

// TrialsResult records a sequential, ordered sequence of scenario trials.
type TrialsResult struct {
	Requested   int
	Trials      []TrialResult
	StartedAt   time.Time
	CompletedAt time.Time
	Status      TrialStatus
}

// Duration reports the elapsed time of the trial sequence.
func (r TrialsResult) Duration() time.Duration {
	return r.CompletedAt.Sub(r.StartedAt)
}

// RunTrials runs the same complete scenario sequentially count times. Errors
// from individual trials are retained in the result and do not stop later
// trials. Parent-context cancellation stops the sequence and is returned as an
// orchestration error after preserving any active trial's partial evidence.
func RunTrials(
	ctx context.Context,
	client *http.Client,
	scenario Scenario,
	count int,
) (result TrialsResult, trialsErr error) {
	if count < 1 || count > MaxTrials {
		return result, fmt.Errorf("validate trial count: must be between 1 and %d: %d", MaxTrials, count)
	}
	result.Requested = count
	result.Trials = make([]TrialResult, 0, count)
	result.StartedAt = time.Now()
	defer func() {
		result.CompletedAt = time.Now()
		result.Status = aggregateTrialStatus(result.Trials)
	}()
	if err := validateRunInput(ctx, client, scenario); err != nil {
		return result, err
	}

	for number := 1; number <= count; number++ {
		if err := ctx.Err(); err != nil {
			return result, fmt.Errorf("run trial sequence: %w", err)
		}

		run, runErr := Run(ctx, client, scenario)
		result.Trials = append(result.Trials, TrialResult{
			Number: number,
			Status: classifyTrial(run, runErr),
			Run:    run,
			Err:    runErr,
		})

		if err := ctx.Err(); err != nil {
			return result, fmt.Errorf("run trial sequence: %w", err)
		}
	}

	return result, nil
}

func classifyTrial(result RunResult, runErr error) TrialStatus {
	if runErr != nil {
		return TrialStatusErrored
	}
	switch result.Outcome {
	case RunOutcomePassed:
		return TrialStatusPassed
	case RunOutcomeViolated:
		return TrialStatusViolated
	case RunOutcomeInconclusive:
		return TrialStatusInconclusive
	default:
		return TrialStatusErrored
	}
}

func aggregateTrialStatus(trials []TrialResult) TrialStatus {
	status := TrialStatusPassed
	for _, trial := range trials {
		switch trial.Status {
		case TrialStatusViolated:
			return TrialStatusViolated
		case TrialStatusErrored:
			status = TrialStatusErrored
		case TrialStatusInconclusive:
			if status == TrialStatusPassed {
				status = TrialStatusInconclusive
			}
		case TrialStatusPassed:
		default:
			status = TrialStatusErrored
		}
	}
	return status
}
