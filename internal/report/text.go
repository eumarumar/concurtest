// Package report renders structured engine results for people and automation.
package report

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/eumarumar/concurtest/internal/engine"
	"github.com/eumarumar/concurtest/internal/reduction"
)

const maxResponseExcerptBytes = 512

// TextInput contains the configured work and recorded evidence for one report.
type TextInput struct {
	ScenarioPath string
	Scenario     engine.Scenario
	Result       engine.TrialsResult
	RunError     error
	Reduction    *reduction.Result
}

// WriteText writes a deterministic, human-readable report. It preserves the
// stable attempt ordering already established by the engine.
func WriteText(writer io.Writer, input TextInput) error {
	if writer == nil {
		return errors.New("write text report: nil writer")
	}
	if err := validateTextInput(input); err != nil {
		return err
	}
	resultLabel, err := resultLabel(input)
	if err != nil {
		return err
	}

	buffer := bufio.NewWriter(writer)
	fmt.Fprintf(buffer, "Result: %s\n", resultLabel)
	fmt.Fprintf(buffer, "Trials: %d\n", input.Result.Requested)
	fmt.Fprintf(buffer, "Completed: %d\n", len(input.Result.Trials))
	fmt.Fprintf(buffer, "Duration: %s\n", displayDuration(input.Result.Duration()))
	fmt.Fprintf(
		buffer,
		"Execution: %d attempts, concurrency %d\n",
		input.Scenario.Attempts,
		input.Scenario.Concurrency,
	)

	counts := trialCounts(input.Result.Trials)
	fmt.Fprintf(buffer, "Passed: %d\n", counts.passed)
	fmt.Fprintf(buffer, "Violated: %d\n", counts.violated)
	fmt.Fprintf(buffer, "Inconclusive: %d\n", counts.inconclusive)
	fmt.Fprintf(buffer, "Errored: %d\n", counts.errored)
	fmt.Fprintf(
		buffer,
		"Violations: %d of %d completed trials\n",
		counts.violated,
		len(input.Result.Trials),
	)
	if counts.firstViolation != 0 {
		fmt.Fprintf(buffer, "First violation: trial %d\n", counts.firstViolation)
	}

	if input.RunError != nil {
		writeRunProblem(buffer, input.RunError)
	}

	if input.Reduction == nil {
		writeTrialResults(buffer, input.Scenario, input.Result, "Trial results")
	} else {
		writeReduction(buffer, input.Scenario, *input.Reduction, input.RunError)
	}
	writeReproduction(buffer, input)

	if err := buffer.Flush(); err != nil {
		return fmt.Errorf("write text report: %w", err)
	}
	return nil
}

func resultLabel(input TextInput) (string, error) {
	if input.RunError != nil {
		return "ERROR", nil
	}
	switch input.Result.Status {
	case engine.TrialStatusPassed:
		return "PASSED", nil
	case engine.TrialStatusViolated:
		return "VIOLATED", nil
	case engine.TrialStatusInconclusive:
		return "INCONCLUSIVE", nil
	case engine.TrialStatusErrored:
		return "ERROR", nil
	default:
		return "", fmt.Errorf("write text report: unknown trials status %q", input.Result.Status)
	}
}

func validateTextInput(input TextInput) error {
	if input.Result.Requested < 1 || input.Result.Requested > engine.MaxTrials {
		return fmt.Errorf("write text report: invalid requested trial count %d", input.Result.Requested)
	}
	if len(input.Result.Trials) > input.Result.Requested {
		return fmt.Errorf(
			"write text report: recorded %d trials for %d requested",
			len(input.Result.Trials),
			input.Result.Requested,
		)
	}
	for index, trial := range input.Result.Trials {
		if trial.Number != index+1 {
			return fmt.Errorf("write text report: trial %d has number %d", index+1, trial.Number)
		}
		switch trial.Status {
		case engine.TrialStatusPassed, engine.TrialStatusViolated, engine.TrialStatusInconclusive, engine.TrialStatusErrored:
		default:
			return fmt.Errorf("write text report: trial %d has unknown status %q", trial.Number, trial.Status)
		}
	}
	if input.RunError == nil {
		switch input.Result.Status {
		case engine.TrialStatusPassed, engine.TrialStatusViolated, engine.TrialStatusInconclusive, engine.TrialStatusErrored:
		default:
			return fmt.Errorf("write text report: unknown trials status %q", input.Result.Status)
		}
	}
	if input.Reduction != nil && input.RunError == nil {
		switch input.Reduction.Status {
		case reduction.StatusSkipped:
			if input.Reduction.SelectedTrials != nil {
				return errors.New("write text report: skipped reduction has a selected result")
			}
		case reduction.StatusReduced, reduction.StatusUnchanged, reduction.StatusLimited:
			if input.Reduction.SelectedTrials == nil {
				return errors.New("write text report: completed reduction has no selected result")
			}
		default:
			return fmt.Errorf("write text report: unknown reduction status %q", input.Reduction.Status)
		}
	}
	return nil
}

type statusCounts struct {
	passed         int
	violated       int
	inconclusive   int
	errored        int
	firstViolation int
}

func trialCounts(trials []engine.TrialResult) statusCounts {
	var counts statusCounts
	for _, trial := range trials {
		switch trial.Status {
		case engine.TrialStatusPassed:
			counts.passed++
		case engine.TrialStatusViolated:
			counts.violated++
			if counts.firstViolation == 0 {
				counts.firstViolation = trial.Number
			}
		case engine.TrialStatusInconclusive:
			counts.inconclusive++
		case engine.TrialStatusErrored:
			counts.errored++
		}
	}
	return counts
}

func trialStatusLabel(status engine.TrialStatus) string {
	return strings.ToUpper(string(status))
}

func writeTrialResults(
	writer io.Writer,
	scenario engine.Scenario,
	result engine.TrialsResult,
	label string,
) {
	fmt.Fprintf(writer, "\n%s:\n", label)
	for _, trial := range result.Trials {
		fmt.Fprintf(writer, "  Trial %d: %s\n", trial.Number, trialStatusLabel(trial.Status))
	}
	if len(result.Trials) == 0 {
		fmt.Fprintln(writer, "  None recorded.")
	}
	for _, trial := range result.Trials {
		if trial.Status == engine.TrialStatusPassed {
			continue
		}
		writeTrialEvidence(writer, scenario, trial)
	}
}

func writeReduction(
	writer io.Writer,
	scenario engine.Scenario,
	result reduction.Result,
	runErr error,
) {
	fmt.Fprintf(writer, "\nReduction: %s\n", reductionStatusLabel(result.Status, runErr))
	fmt.Fprintf(writer, "Duration: %s\n", displayDuration(result.Duration()))
	fmt.Fprintf(writer, "Candidates evaluated: %d\n", len(result.Candidates))

	if len(result.Candidates) > 0 {
		fmt.Fprintln(writer, "Candidate results:")
		for _, candidate := range result.Candidates {
			writeCandidateResult(writer, candidate)
		}
	}

	if runErr != nil {
		writeInterruptedReductionEvidence(writer, scenario, result)
		return
	}

	switch result.Status {
	case reduction.StatusSkipped:
		baseline := reductionSummary(result.Baseline)
		if baseline.Inconclusive > 0 || baseline.Errored > 0 {
			fmt.Fprintf(
				writer,
				"Reason: the baseline included %d inconclusive and %d errored trials; reduction needs clean trial results.\n",
				baseline.Inconclusive,
				baseline.Errored,
			)
		} else {
			fmt.Fprintf(
				writer,
				"Reason: the baseline produced %d violations in %d trials; a strict majority is required.\n",
				baseline.Violated,
				baseline.Requested,
			)
		}
		writeTrialResults(writer, scenario, result.Baseline, "Baseline trial results")
	case reduction.StatusReduced, reduction.StatusUnchanged, reduction.StatusLimited:
		selected := reductionSummary(*result.SelectedTrials)
		fmt.Fprintln(writer, "Smallest observed failure:")
		fmt.Fprintf(writer, "  Attempts: %d\n", result.Selected.Attempts)
		fmt.Fprintf(writer, "  Concurrency: %d\n", result.Selected.Concurrency)
		fmt.Fprintf(
			writer,
			"  Violations: %d of %d trials\n",
			selected.Violated,
			selected.Requested,
		)
		if result.Status == reduction.StatusUnchanged {
			fmt.Fprintln(writer, "No smaller tested configuration met the reproduction rule.")
		}
		if result.Status == reduction.StatusLimited {
			fmt.Fprintf(
				writer,
				"The %d-candidate search limit was reached; smaller untested configurations may remain.\n",
				reduction.MaxCandidates,
			)
		}
		fmt.Fprintln(
			writer,
			"Note: this is the smallest failing configuration ConcurTest observed, not proof that no smaller failure exists.",
		)
		writeTrialResults(writer, scenario, *result.SelectedTrials, "Selected trial results")
	}
}

func reductionStatusLabel(status reduction.Status, runErr error) string {
	if runErr != nil {
		return "INTERRUPTED"
	}
	switch status {
	case reduction.StatusSkipped:
		return "NOT RUN"
	case reduction.StatusReduced:
		return "REDUCED"
	case reduction.StatusUnchanged:
		return "UNCHANGED"
	case reduction.StatusLimited:
		return "SEARCH LIMIT REACHED"
	default:
		return "UNKNOWN"
	}
}

func writeCandidateResult(writer io.Writer, candidate reduction.CandidateResult) {
	fmt.Fprintf(
		writer,
		"  %d attempts, concurrency %d: %d passed, %d violated, %d inconclusive, %d errored; %d of %d completed; duration %s",
		candidate.Candidate.Attempts,
		candidate.Candidate.Concurrency,
		candidate.Summary.Passed,
		candidate.Summary.Violated,
		candidate.Summary.Inconclusive,
		candidate.Summary.Errored,
		candidate.Summary.Completed,
		candidate.Summary.Requested,
		displayDuration(candidate.Summary.Duration()),
	)
	if candidate.Accepted {
		fmt.Fprint(writer, " — selected")
	}
	fmt.Fprintln(writer)
}

func writeInterruptedReductionEvidence(
	writer io.Writer,
	scenario engine.Scenario,
	result reduction.Result,
) {
	if len(result.Candidates) > 0 {
		candidate := result.Candidates[len(result.Candidates)-1]
		if candidate.Trials != nil {
			fmt.Fprintf(
				writer,
				"Interrupted candidate: %d attempts, concurrency %d\n",
				candidate.Candidate.Attempts,
				candidate.Candidate.Concurrency,
			)
			writeTrialResults(writer, scenario, *candidate.Trials, "Interrupted candidate trial results")
			return
		}
	}
	writeTrialResults(writer, scenario, result.Baseline, "Baseline trial results")
}

func reductionSummary(result engine.TrialsResult) reduction.TrialSummary {
	counts := trialCounts(result.Trials)
	return reduction.TrialSummary{
		Requested:    result.Requested,
		Completed:    len(result.Trials),
		Passed:       counts.passed,
		Violated:     counts.violated,
		Inconclusive: counts.inconclusive,
		Errored:      counts.errored,
		StartedAt:    result.StartedAt,
		CompletedAt:  result.CompletedAt,
	}
}

func writeReproduction(writer io.Writer, input TextInput) {
	if input.Reduction != nil && input.Reduction.SelectedTrials != nil {
		fmt.Fprintf(
			writer,
			"\nReproduce: concurtest run --attempts %d --concurrency %d --no-reduce %s\n",
			input.Reduction.Selected.Attempts,
			input.Reduction.Selected.Concurrency,
			strconv.QuoteToGraphic(input.ScenarioPath),
		)
		return
	}
	fmt.Fprintf(writer, "\nReproduce: concurtest run %s\n", strconv.QuoteToGraphic(input.ScenarioPath))
}

func writeTrialEvidence(writer io.Writer, scenario engine.Scenario, trial engine.TrialResult) {
	fmt.Fprintf(writer, "\nTrial %d evidence (%s):\n", trial.Number, trialStatusLabel(trial.Status))
	fmt.Fprintf(writer, "  Duration: %s\n", displayDuration(trial.Run.Duration()))
	if trial.Err != nil {
		writeTrialProblem(writer, trial.Err)
	} else if trial.Status == engine.TrialStatusInconclusive {
		failed := failedAttemptCount(trial.Run.History)
		fmt.Fprintf(
			writer,
			"  Reason: %d of %d operation attempts failed or did not start, so this trial cannot be reported as a pass.\n",
			failed,
			len(trial.Run.History.Attempts),
		)
	}
	writeInvariant(writer, scenario.Invariant, trial.Run.Evaluation)
	writeOptionalExecution(writer, "Setup", trial.Run.Setup)
	writeAttempts(writer, trial.Run.History)
	writeObservation(writer, scenario.Observation, trial.Run.Observation)
}

func writeRunProblem(writer io.Writer, runErr error) {
	if errors.Is(runErr, context.Canceled) {
		fmt.Fprintln(writer, "Problem: the trial sequence was canceled before all requested trials completed.")
		return
	}
	if errors.Is(runErr, context.DeadlineExceeded) {
		fmt.Fprintln(writer, "Problem: the trial sequence deadline was exceeded before all requested trials completed.")
		return
	}
	fmt.Fprintf(writer, "Problem: %s\n", quoted(runErr.Error()))
	fmt.Fprintln(writer, "Next step: check the target and scenario, then try again.")
}

func writeTrialProblem(writer io.Writer, runErr error) {
	if errors.Is(runErr, context.Canceled) {
		fmt.Fprintln(writer, "  Problem: this trial was canceled.")
		return
	}
	if errors.Is(runErr, context.DeadlineExceeded) {
		fmt.Fprintln(writer, "  Problem: this trial's deadline was exceeded.")
		return
	}
	fmt.Fprintf(writer, "  Problem: %s\n", quoted(runErr.Error()))
}

func writeInvariant(
	writer io.Writer,
	invariant engine.Invariant,
	evaluation *engine.InvariantEvaluation,
) {
	fmt.Fprintln(writer, "\nInvariant:")
	switch {
	case invariant.JSONIntegerMinimum != nil:
		definition := invariant.JSONIntegerMinimum
		fmt.Fprintf(writer, "  Name: %s\n", quoted(definition.Name))
		fmt.Fprintf(writer, "  Expected: %s >= %d\n", quoted(definition.Field), definition.Minimum)
		if evaluation == nil || evaluation.JSONIntegerMinimum == nil {
			fmt.Fprintln(writer, "  Observed: not evaluated")
			return
		}
		fmt.Fprintf(
			writer,
			"  Observed: %s = %d\n",
			quoted(definition.Field),
			evaluation.JSONIntegerMinimum.Observed,
		)
	case invariant.MaximumSuccessfulAttempts != nil:
		definition := invariant.MaximumSuccessfulAttempts
		fmt.Fprintf(writer, "  Name: %s\n", quoted(definition.Name))
		fmt.Fprintf(
			writer,
			"  Expected: at most %d successful %s\n",
			definition.Maximum,
			attemptWord(definition.Maximum),
		)
		fmt.Fprintf(writer, "  Successful statuses: %s\n", successfulStatuses(definition.SuccessfulStatusCodes))
		if evaluation == nil || evaluation.MaximumSuccessfulAttempts == nil {
			fmt.Fprintln(writer, "  Observed: not evaluated")
			return
		}
		historyEvaluation := evaluation.MaximumSuccessfulAttempts
		fmt.Fprintf(
			writer,
			"  Observed: %d successful %s\n",
			len(historyEvaluation.SuccessfulAttemptIDs),
			attemptWord(len(historyEvaluation.SuccessfulAttemptIDs)),
		)
		fmt.Fprintf(writer, "  Successful attempts: %s\n", attemptIDs(historyEvaluation.SuccessfulAttemptIDs))
		fmt.Fprintf(writer, "  Beyond maximum: %s\n", attemptIDs(historyEvaluation.OverLimitAttemptIDs))
	}
}

func successfulStatuses(statuses []int) string {
	if statuses == nil {
		return "HTTP 200-299 (default)"
	}
	formatted := make([]string, 0, len(statuses))
	for _, status := range statuses {
		label := fmt.Sprintf("HTTP %d", status)
		if text := http.StatusText(status); text != "" {
			label += " " + text
		}
		formatted = append(formatted, label)
	}
	return strings.Join(formatted, ", ")
}

func attemptIDs(ids []int) string {
	if len(ids) == 0 {
		return "none"
	}
	formatted := make([]string, len(ids))
	for index, id := range ids {
		formatted[index] = fmt.Sprintf("#%d", id)
	}
	return strings.Join(formatted, ", ")
}

func attemptWord(count int) string {
	if count == 1 {
		return "attempt"
	}
	return "attempts"
}

func writeOptionalExecution(writer io.Writer, label string, execution *engine.HTTPExecution) {
	if execution == nil {
		return
	}
	fmt.Fprintf(writer, "\n%s:\n", label)
	writeExecution(writer, execution, false, time.Time{})
}

func writeAttempts(writer io.Writer, history engine.History) {
	fmt.Fprintln(writer, "\nAttempts:")
	if len(history.Attempts) == 0 {
		fmt.Fprintln(writer, "  None recorded.")
		return
	}

	for _, attempt := range history.Attempts {
		fmt.Fprintf(writer, "  #%d %s\n", attempt.ID, quoted(attempt.OperationName))
		if attempt.Execution == nil {
			fmt.Fprintln(writer, "    Status: not started")
			continue
		}
		writeExecution(writer, attempt.Execution, true, history.StartedAt)
	}
}

func writeObservation(
	writer io.Writer,
	configured *engine.HTTPRequest,
	execution *engine.HTTPExecution,
) {
	fmt.Fprintln(writer, "\nObservation:")
	if configured == nil {
		fmt.Fprintln(writer, "  Not configured.")
		return
	}
	if execution == nil {
		fmt.Fprintln(writer, "  Not reached.")
		return
	}
	writeExecution(writer, execution, false, time.Time{})
}

func writeExecution(
	writer io.Writer,
	execution *engine.HTTPExecution,
	includeRelativeStart bool,
	historyStart time.Time,
) {
	fmt.Fprintf(
		writer,
		"    Request: %s %s\n",
		quoted(execution.Request.Method),
		quoted(requestTarget(execution.Request.URL)),
	)
	if includeRelativeStart {
		fmt.Fprintf(
			writer,
			"    Started: +%s\n",
			displayDuration(execution.StartedAt.Sub(historyStart)),
		)
	}
	fmt.Fprintf(writer, "    Duration: %s\n", displayDuration(execution.Duration()))

	if execution.Response == nil {
		fmt.Fprintln(writer, "    Status: no response")
	} else {
		fmt.Fprintf(writer, "    Status: %s\n", displayStatus(execution.Response.StatusCode))
		if len(execution.Response.Body) > 0 || execution.Response.BodyTruncated {
			fmt.Fprintf(writer, "    Response: %s\n", responseExcerpt(execution.Response))
		}
	}
	if execution.Err != nil {
		fmt.Fprintf(writer, "    Error: %s\n", quoted(execution.Err.Error()))
	}
}

func failedAttemptCount(history engine.History) int {
	failed := 0
	for _, attempt := range history.Attempts {
		if attempt.Execution == nil || attempt.Execution.Err != nil {
			failed++
		}
	}
	return failed
}

func requestTarget(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Path == "" {
		return rawURL
	}
	target := parsed.EscapedPath()
	if parsed.ForceQuery || parsed.RawQuery != "" {
		target += "?" + parsed.RawQuery
	}
	return target
}

func displayStatus(statusCode int) string {
	statusText := http.StatusText(statusCode)
	if statusText == "" {
		return fmt.Sprintf("HTTP %d", statusCode)
	}
	return fmt.Sprintf("HTTP %d %s", statusCode, statusText)
}

func responseExcerpt(response *engine.HTTPResponse) string {
	body := response.Body
	truncated := response.BodyTruncated
	if len(body) > maxResponseExcerptBytes {
		body = body[:maxResponseExcerptBytes]
		truncated = true
	}

	excerpt := strconv.QuoteToGraphic(string(body))
	if truncated {
		excerpt += " [truncated]"
	}
	return excerpt
}

func quoted(value string) string {
	return strconv.QuoteToGraphic(value)
}

func displayDuration(duration time.Duration) string {
	if duration < 0 {
		return "0s"
	}
	return duration.String()
}
