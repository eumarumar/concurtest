// Package reduction searches for a smaller observed reproduction of a
// correctness failure by composing sequential engine trials.
package reduction

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/eumarumar/concurtest/internal/engine"
)

// MaxCandidates is the largest number of smaller execution configurations a
// reduction run evaluates.
const MaxCandidates = 100

// Status describes how a completed reduction search ended.
type Status string

const (
	// StatusSkipped means the baseline did not meet the reproduction rule.
	StatusSkipped Status = "skipped"
	// StatusReduced means a smaller candidate met the reproduction rule.
	StatusReduced Status = "reduced"
	// StatusUnchanged means every smaller candidate was evaluated and rejected.
	StatusUnchanged Status = "unchanged"
	// StatusLimited means the candidate limit was reached before the search
	// space was exhausted.
	StatusLimited Status = "limited"
)

// Candidate identifies one attempt and concurrency combination.
type Candidate struct {
	Attempts    int
	Concurrency int
}

// TrialSummary retains bounded outcome and timing data for one candidate.
type TrialSummary struct {
	Requested    int
	Completed    int
	Passed       int
	Violated     int
	Inconclusive int
	Errored      int
	StartedAt    time.Time
	CompletedAt  time.Time
}

// Duration reports the elapsed candidate evaluation time.
func (s TrialSummary) Duration() time.Duration {
	return s.CompletedAt.Sub(s.StartedAt)
}

// CandidateResult records one candidate evaluation. Trials is retained only
// for the selected candidate or an interrupted active candidate.
type CandidateResult struct {
	Candidate Candidate
	Summary   TrialSummary
	Accepted  bool
	Trials    *engine.TrialsResult
	Err       error
}

// Result records the baseline, ordered candidate summaries, and any selected
// observed reproduction.
type Result struct {
	StartedAt      time.Time
	CompletedAt    time.Time
	Baseline       engine.TrialsResult
	Candidates     []CandidateResult
	Selected       Candidate
	SelectedTrials *engine.TrialsResult
	Status         Status
}

// Duration reports the elapsed reduction time, including the baseline.
func (r Result) Duration() time.Duration {
	return r.CompletedAt.Sub(r.StartedAt)
}

// Reduce runs a baseline and then evaluates up to MaxCandidates smaller
// attempt/concurrency pairs in deterministic order. A candidate is accepted
// only when a strict majority of its trials violate and none are inconclusive
// or errored.
func Reduce(
	ctx context.Context,
	client *http.Client,
	scenario engine.Scenario,
	trials int,
) (result Result, reduceErr error) {
	return reduce(ctx, client, scenario, trials, MaxCandidates)
}

func reduce(
	ctx context.Context,
	client *http.Client,
	scenario engine.Scenario,
	trials int,
	candidateLimit int,
) (result Result, reduceErr error) {
	if err := validateInput(scenario, trials); err != nil {
		return result, err
	}

	result.StartedAt = time.Now()
	defer func() {
		result.CompletedAt = time.Now()
	}()

	baseline, err := engine.RunTrials(ctx, client, scenario, trials)
	result.Baseline = baseline
	if err != nil {
		return result, fmt.Errorf("run reduction baseline: %w", err)
	}
	if !qualifies(summarize(baseline)) {
		result.Status = StatusSkipped
		return result, nil
	}

	candidates, limited := smallerCandidates(scenario, candidateLimit)
	result.Candidates = make([]CandidateResult, 0, len(candidates))
	for _, candidate := range candidates {
		candidateScenario := scenario
		candidateScenario.Attempts = candidate.Attempts
		candidateScenario.Concurrency = candidate.Concurrency

		trialsResult, candidateErr := engine.RunTrials(ctx, client, candidateScenario, trials)
		candidateResult := CandidateResult{
			Candidate: candidate,
			Summary:   summarize(trialsResult),
			Err:       candidateErr,
		}
		if candidateErr != nil {
			candidateResult.Trials = &trialsResult
			result.Candidates = append(result.Candidates, candidateResult)
			return result, fmt.Errorf(
				"evaluate reduction candidate with %d attempts and concurrency %d: %w",
				candidate.Attempts,
				candidate.Concurrency,
				candidateErr,
			)
		}
		if qualifies(candidateResult.Summary) {
			candidateResult.Accepted = true
			candidateResult.Trials = &trialsResult
			result.Candidates = append(result.Candidates, candidateResult)
			result.Selected = candidate
			result.SelectedTrials = &trialsResult
			result.Status = StatusReduced
			return result, nil
		}
		result.Candidates = append(result.Candidates, candidateResult)
	}

	baselineCopy := baseline
	result.Selected = Candidate{
		Attempts:    scenario.Attempts,
		Concurrency: scenario.Concurrency,
	}
	result.SelectedTrials = &baselineCopy
	if limited {
		result.Status = StatusLimited
	} else {
		result.Status = StatusUnchanged
	}
	return result, nil
}

func validateInput(scenario engine.Scenario, trials int) error {
	if scenario.Setup == nil {
		return errors.New("validate reduction: setup is required to reset state before every trial")
	}
	if trials < 3 || trials > engine.MaxTrials {
		return fmt.Errorf(
			"validate reduction: trials must be between 3 and %d: %d",
			engine.MaxTrials,
			trials,
		)
	}
	if scenario.Attempts < 2 {
		return fmt.Errorf("validate reduction: attempts must be at least 2: %d", scenario.Attempts)
	}
	if scenario.Concurrency < 2 {
		return fmt.Errorf("validate reduction: concurrency must be at least 2: %d", scenario.Concurrency)
	}
	if scenario.Concurrency > scenario.Attempts {
		return fmt.Errorf(
			"validate reduction: concurrency %d exceeds attempts %d",
			scenario.Concurrency,
			scenario.Attempts,
		)
	}
	return nil
}

func smallerCandidates(scenario engine.Scenario, limit int) ([]Candidate, bool) {
	candidates := make([]Candidate, 0, min(limit, MaxCandidates))
	for attempts := 2; attempts <= scenario.Attempts; attempts++ {
		maximumConcurrency := min(attempts, scenario.Concurrency)
		for concurrency := 2; concurrency <= maximumConcurrency; concurrency++ {
			if attempts == scenario.Attempts && concurrency == scenario.Concurrency {
				continue
			}
			if len(candidates) == limit {
				return candidates, true
			}
			candidates = append(candidates, Candidate{
				Attempts:    attempts,
				Concurrency: concurrency,
			})
		}
	}
	return candidates, false
}

func summarize(result engine.TrialsResult) TrialSummary {
	summary := TrialSummary{
		Requested:   result.Requested,
		Completed:   len(result.Trials),
		StartedAt:   result.StartedAt,
		CompletedAt: result.CompletedAt,
	}
	for _, trial := range result.Trials {
		switch trial.Status {
		case engine.TrialStatusPassed:
			summary.Passed++
		case engine.TrialStatusViolated:
			summary.Violated++
		case engine.TrialStatusInconclusive:
			summary.Inconclusive++
		case engine.TrialStatusErrored:
			summary.Errored++
		}
	}
	return summary
}

func qualifies(summary TrialSummary) bool {
	return summary.Completed == summary.Requested &&
		summary.Inconclusive == 0 &&
		summary.Errored == 0 &&
		summary.Violated > summary.Requested/2
}
