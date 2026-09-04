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
	"github.com/eumarumar/concurtest/internal/failure"
	"github.com/eumarumar/concurtest/internal/reduction"
)

const (
	maxResponseExcerptBytes        = 512
	maxCompactResponseExcerptBytes = 160
	maxCompactAttempts             = 4
)

const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
)

// Input contains the configured work and recorded evidence for a completed or
// partially completed run report.
type Input struct {
	ScenarioPath     string
	ScenarioName     string
	Target           string
	RequestTimeout   time.Duration
	ConfiguredTrials int
	ReductionEnabled bool
	Scenario         engine.Scenario
	Result           engine.TrialsResult
	RunError         error
	Reduction        *reduction.Result
}

// TextInput is retained as an alias for callers constructing text reports.
type TextInput = Input

// TextOptions controls terminal-only presentation. It does not affect the
// structured evidence retained by the engine or emitted as JSON.
type TextOptions struct {
	Verbose bool
	Color   bool
}

// TextStartInput contains details that must be shown before requests begin.
type TextStartInput struct {
	ScenarioName     string
	Target           string
	ReductionEnabled bool
}

type textStyle struct{ color bool }

func (s textStyle) wrap(code, value string) string {
	if !s.color {
		return value
	}
	return code + value + ansiReset
}

func (s textStyle) heading(value string) string { return s.wrap(ansiBold+ansiCyan, value) }
func (s textStyle) note(value string) string    { return s.wrap(ansiDim, value) }

func (s textStyle) status(status string) string {
	switch {
	case strings.Contains(status, "PASSED"):
		return s.wrap(ansiBold+ansiGreen, status)
	case strings.Contains(status, "VIOLATED"), strings.Contains(status, "ERROR"), strings.Contains(status, "ERRORED"), strings.Contains(status, "INTERRUPTED"), strings.HasPrefix(status, "Violation"), status == "Problem":
		return s.wrap(ansiBold+ansiRed, status)
	case strings.Contains(status, "INCONCLUSIVE"), status == "SEARCH LIMIT REACHED":
		return s.wrap(ansiBold+ansiYellow, status)
	default:
		return s.wrap(ansiBold, status)
	}
}

// WriteTextStart writes the safety preamble before any target requests begin.
func WriteTextStart(writer io.Writer, input TextStartInput, options TextOptions) error {
	if writer == nil {
		return failure.New(failure.CodeReportInvalid, "write text start: nil writer")
	}
	style := textStyle{color: options.Color}
	if _, err := fmt.Fprintf(
		writer,
		"%s\n%s · %s\n%s\n",
		style.heading("ConcurTest · "+input.ScenarioName),
		style.heading("Target"),
		input.Target,
		style.wrap(ansiBold+ansiYellow, "Warning · This run sends concurrent requests and may change target data."),
	); err != nil {
		return failure.Wrap(failure.CodeReportWriteFailed, "write text start", err)
	}
	if input.ReductionEnabled {
		if _, err := fmt.Fprintf(writer, "%s · Up to %d smaller configurations may also run.\n", style.heading("Reduction"), reduction.MaxCandidates); err != nil {
			return failure.Wrap(failure.CodeReportWriteFailed, "write text start", err)
		}
	}
	if _, err := fmt.Fprintln(writer); err != nil {
		return failure.Wrap(failure.CodeReportWriteFailed, "write text start", err)
	}
	return nil
}

// WriteText writes a deterministic, human-readable report using plain text.
func WriteText(writer io.Writer, input Input) error {
	return WriteTextWithOptions(writer, input, TextOptions{})
}

// WriteTextWithOptions writes a deterministic terminal report. Attempt and
// trial ordering remain the stable ordering established by the engine.
func WriteTextWithOptions(writer io.Writer, input Input, options TextOptions) error {
	if writer == nil {
		return failure.New(failure.CodeReportInvalid, "write text report: nil writer")
	}
	if err := validateTextInput(input); err != nil {
		return failure.Wrap(failure.CodeReportInvalid, "", err)
	}
	resultLabel, err := resultLabel(input)
	if err != nil {
		return failure.Wrap(failure.CodeReportInvalid, "", err)
	}

	buffer := bufio.NewWriter(writer)
	style := textStyle{color: options.Color}
	counts := trialCounts(input.Result.Trials)

	fmt.Fprintln(buffer, style.status(resultLabel))
	fmt.Fprintln(buffer, aggregateSentence(input, counts))

	fmt.Fprintf(buffer, "\n%s\n", style.heading("Trials"))
	fmt.Fprintf(buffer, "  Requested       %d\n", input.Result.Requested)
	fmt.Fprintf(buffer, "  Completed       %d\n", len(input.Result.Trials))
	fmt.Fprintf(buffer, "  Passed          %d\n", counts.passed)
	fmt.Fprintf(buffer, "  Violated        %d\n", counts.violated)
	fmt.Fprintf(buffer, "  Inconclusive    %d\n", counts.inconclusive)
	fmt.Fprintf(buffer, "  Errored         %d\n", counts.errored)
	if counts.firstViolation != 0 {
		fmt.Fprintf(buffer, "  First violation Trial %d\n", counts.firstViolation)
	}

	fmt.Fprintf(buffer, "\n%s\n", style.heading("Execution"))
	fmt.Fprintf(buffer, "  Attempts        %d\n", input.Scenario.Attempts)
	fmt.Fprintf(buffer, "  Concurrency     %d\n", input.Scenario.Concurrency)
	fmt.Fprintf(buffer, "  Duration        %s\n", displayDuration(input.Result.Duration()))

	writeInvariantSummary(buffer, style, input.Scenario.Invariant, firstEvaluation(input.Result.Trials))
	if input.RunError != nil {
		writeRunProblem(buffer, style, input.RunError)
	}

	if options.Verbose {
		writeTrialCollection(buffer, style, input.Scenario, input.Result, "Baseline")
	}
	if input.Reduction != nil {
		writeReduction(buffer, style, input.Scenario, *input.Reduction, input.RunError, options)
	}
	if !options.Verbose {
		writeCompactEvidence(buffer, style, input)
	}
	writeReproduction(buffer, style, input)
	if !options.Verbose {
		fmt.Fprintf(buffer, "\n%s\n", style.note("Run with --verbose for all trial evidence."))
	}

	if err := buffer.Flush(); err != nil {
		return failure.Wrap(failure.CodeReportWriteFailed, "write text report", err)
	}
	return nil
}

func aggregateSentence(input Input, counts statusCounts) string {
	completed := len(input.Result.Trials)
	if input.RunError != nil {
		return fmt.Sprintf("The run stopped after %d of %d requested trials.", completed, input.Result.Requested)
	}
	switch input.Result.Status {
	case engine.TrialStatusPassed:
		return fmt.Sprintf("%d/%d trials passed.", counts.passed, completed)
	case engine.TrialStatusViolated:
		return fmt.Sprintf("%d/%d trials demonstrated the violation.", counts.violated, completed)
	case engine.TrialStatusInconclusive:
		return fmt.Sprintf("%d/%d trials were inconclusive.", counts.inconclusive, completed)
	case engine.TrialStatusErrored:
		return fmt.Sprintf("%d/%d trials encountered problems.", counts.errored, completed)
	default:
		return "The run produced an unknown result."
	}
}

func resultLabel(input Input) (string, error) {
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

func validateTextInput(input Input) error {
	if input.Result.Requested < 1 || input.Result.Requested > engine.MaxTrials {
		return fmt.Errorf("write text report: invalid requested trial count %d", input.Result.Requested)
	}
	if len(input.Result.Trials) > input.Result.Requested {
		return fmt.Errorf("write text report: recorded %d trials for %d requested", len(input.Result.Trials), input.Result.Requested)
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

func firstEvaluation(trials []engine.TrialResult) *engine.InvariantEvaluation {
	for _, trial := range trials {
		if trial.Status == engine.TrialStatusViolated && trial.Run.Evaluation != nil {
			return trial.Run.Evaluation
		}
	}
	for _, trial := range trials {
		if trial.Run.Evaluation != nil {
			return trial.Run.Evaluation
		}
	}
	return nil
}

func trialStatusLabel(status engine.TrialStatus) string { return strings.ToUpper(string(status)) }

func writeTrialCollection(writer io.Writer, style textStyle, scenario engine.Scenario, result engine.TrialsResult, label string) {
	if len(result.Trials) == 0 {
		fmt.Fprintf(writer, "\n%s\n  No trial evidence was recorded.\n", style.heading(label+" evidence"))
		return
	}

	fmt.Fprintf(writer, "\n%s\n", style.heading(label+" evidence"))
	for _, trial := range result.Trials {
		writeTrialEvidence(writer, style, scenario, trial, label)
	}
}

func writeReduction(writer io.Writer, style textStyle, scenario engine.Scenario, result reduction.Result, runErr error, options TextOptions) {
	status := reductionStatusLabel(result.Status, runErr)
	fmt.Fprintf(writer, "\n%s\n", style.heading("Reduction"))
	fmt.Fprintf(writer, "  Status          %s\n", style.status(status))

	if options.Verbose {
		fmt.Fprintf(writer, "  Candidates      %d evaluated\n", len(result.Candidates))
		fmt.Fprintf(writer, "  Duration        %s\n", displayDuration(result.Duration()))
		if len(result.Candidates) > 0 {
			fmt.Fprintln(writer, "  Candidate results")
			for _, candidate := range result.Candidates {
				writeCandidateResult(writer, candidate)
			}
		}
	}

	if runErr != nil {
		writeInterruptedReductionEvidence(writer, style, scenario, result, options)
		return
	}

	switch result.Status {
	case reduction.StatusSkipped:
		baseline := reductionSummary(result.Baseline)
		if baseline.Inconclusive > 0 || baseline.Errored > 0 {
			fmt.Fprintf(writer, "  Finding         Baseline had %d inconclusive and %d errored trials; reduction needs clean results.\n", baseline.Inconclusive, baseline.Errored)
		} else {
			fmt.Fprintf(writer, "  Finding         Baseline had %d violations in %d trials; a strict majority is required.\n", baseline.Violated, baseline.Requested)
		}
	case reduction.StatusReduced, reduction.StatusUnchanged, reduction.StatusLimited:
		selected := reductionSummary(*result.SelectedTrials)
		fmt.Fprintf(writer, "  Attempts        %d\n", result.Selected.Attempts)
		fmt.Fprintf(writer, "  Concurrency     %d\n", result.Selected.Concurrency)
		fmt.Fprintf(writer, "  Violations      %d/%d trials\n", selected.Violated, selected.Requested)
		if result.Status == reduction.StatusUnchanged {
			fmt.Fprintln(writer, "  Finding         No smaller tested configuration met the reproduction rule.")
		}
		if result.Status == reduction.StatusLimited {
			fmt.Fprintf(writer, "  Finding         The %d-candidate limit was reached; smaller untested configurations may remain.\n", reduction.MaxCandidates)
		}
		fmt.Fprintln(writer, "  Note            Smallest observed failure; a smaller one may still exist.")

		if options.Verbose && result.Status == reduction.StatusReduced {
			writeTrialCollection(writer, style, scenario, *result.SelectedTrials, "Selected candidate")
		}
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
	fmt.Fprintf(writer, "    %d attempts · concurrency %d · %d passed · %d violated · %d inconclusive · %d errored · %d of %d completed · %s",
		candidate.Candidate.Attempts, candidate.Candidate.Concurrency,
		candidate.Summary.Passed, candidate.Summary.Violated, candidate.Summary.Inconclusive, candidate.Summary.Errored,
		candidate.Summary.Completed, candidate.Summary.Requested, displayDuration(candidate.Summary.Duration()))
	if candidate.Accepted {
		fmt.Fprint(writer, " · selected")
	}
	if candidate.Err != nil {
		fmt.Fprintf(writer, " · error %s", quoted(candidate.Err.Error()))
	}
	fmt.Fprintln(writer)
}

func writeInterruptedReductionEvidence(writer io.Writer, style textStyle, scenario engine.Scenario, result reduction.Result, options TextOptions) {
	if len(result.Candidates) == 0 {
		return
	}
	candidate := result.Candidates[len(result.Candidates)-1]
	if candidate.Trials == nil {
		return
	}
	fmt.Fprintf(writer, "  Interrupted at  %d attempts · concurrency %d\n", candidate.Candidate.Attempts, candidate.Candidate.Concurrency)
	if options.Verbose {
		writeTrialCollection(writer, style, scenario, *candidate.Trials, "Interrupted candidate")
	}
}

type compactTrialSource struct {
	label  string
	trials []engine.TrialResult
}

func writeCompactEvidence(writer io.Writer, style textStyle, input Input) {
	violation, violationLabel, hasViolation := compactViolation(input)
	problemSources := compactProblemSources(input)
	_, _, errored := compactProblem(problemSources, engine.TrialStatusErrored)
	_, _, inconclusive := compactProblem(problemSources, engine.TrialStatusInconclusive)
	if !hasViolation && errored == 0 && inconclusive == 0 {
		return
	}

	fmt.Fprintf(writer, "\n%s\n", style.heading("Evidence"))
	if hasViolation {
		fmt.Fprintf(writer, "  %s · Trial %d\n", violationLabel, violation.Number)
		writeCompactViolation(writer, input.Scenario, violation)
	}
	writeCompactProblem(writer, style, problemSources, engine.TrialStatusErrored)
	writeCompactProblem(writer, style, problemSources, engine.TrialStatusInconclusive)
}

func compactViolation(input Input) (engine.TrialResult, string, bool) {
	if input.Reduction != nil && input.Reduction.SelectedTrials != nil {
		if trial, ok := firstTrialWithStatus(input.Reduction.SelectedTrials.Trials, engine.TrialStatusViolated); ok {
			return trial, "Smallest observed failure", true
		}
	}
	if trial, ok := firstTrialWithStatus(input.Result.Trials, engine.TrialStatusViolated); ok {
		return trial, "Baseline failure", true
	}
	return engine.TrialResult{}, "", false
}

func firstTrialWithStatus(trials []engine.TrialResult, status engine.TrialStatus) (engine.TrialResult, bool) {
	for _, trial := range trials {
		if trial.Status == status {
			return trial, true
		}
	}
	return engine.TrialResult{}, false
}

func writeCompactViolation(writer io.Writer, scenario engine.Scenario, trial engine.TrialResult) {
	attempts := trial.Run.History.Attempts
	description := "attempt"
	if scenario.Invariant.MaximumSuccessfulAttempts != nil && trial.Run.Evaluation != nil && trial.Run.Evaluation.MaximumSuccessfulAttempts != nil {
		description = "successful attempt"
		successful := make(map[int]struct{}, len(trial.Run.Evaluation.MaximumSuccessfulAttempts.SuccessfulAttemptIDs))
		for _, id := range trial.Run.Evaluation.MaximumSuccessfulAttempts.SuccessfulAttemptIDs {
			successful[id] = struct{}{}
		}
		filtered := make([]engine.Attempt, 0, len(successful))
		for _, attempt := range attempts {
			if _, ok := successful[attempt.ID]; ok {
				filtered = append(filtered, attempt)
			}
		}
		attempts = filtered
	}
	writeCompactAttempts(writer, attempts, description)
	if scenario.Invariant.JSONIntegerMinimum != nil {
		writeCompactObservation(writer, scenario.Observation, trial.Run.Observation)
	}
}

func writeCompactAttempts(writer io.Writer, attempts []engine.Attempt, description string) {
	shown := min(len(attempts), maxCompactAttempts)
	for _, attempt := range attempts[:shown] {
		writeCompactAttempt(writer, attempt)
	}
	if omitted := len(attempts) - shown; omitted > 0 {
		fmt.Fprintf(writer, "    %d more %s not shown.\n", omitted, pluralize(description, omitted))
	}
}

func writeCompactAttempt(writer io.Writer, attempt engine.Attempt) {
	if attempt.Execution == nil {
		fmt.Fprintf(writer, "    Attempt #%d     %s · not started\n", attempt.ID, attempt.OperationName)
		return
	}
	execution := attempt.Execution
	fmt.Fprintf(writer, "    Attempt #%d     %s %s · %s\n", attempt.ID, execution.Request.Method, requestTarget(execution.Request.URL), executionStatus(execution))
	writeCompactExecutionDetails(writer, execution, "      ")
}

func writeCompactObservation(writer io.Writer, configured *engine.HTTPRequest, execution *engine.HTTPExecution) {
	if configured == nil {
		return
	}
	if execution == nil {
		fmt.Fprintln(writer, "    Observation    not reached")
		return
	}
	fmt.Fprintf(writer, "    Observation    %s %s · %s\n", execution.Request.Method, requestTarget(execution.Request.URL), executionStatus(execution))
	writeCompactExecutionDetails(writer, execution, "      ")
}

func compactProblemSources(input Input) []compactTrialSource {
	sources := make([]compactTrialSource, 0, 2)
	if input.RunError != nil && input.Reduction != nil && len(input.Reduction.Candidates) > 0 {
		candidate := input.Reduction.Candidates[len(input.Reduction.Candidates)-1]
		if candidate.Trials != nil {
			sources = append(sources, compactTrialSource{label: "Interrupted candidate", trials: candidate.Trials.Trials})
		}
	}
	return append(sources, compactTrialSource{label: "Baseline", trials: input.Result.Trials})
}

func compactProblem(sources []compactTrialSource, status engine.TrialStatus) (engine.TrialResult, string, int) {
	var first engine.TrialResult
	firstLabel := ""
	count := 0
	for _, source := range sources {
		for _, trial := range source.trials {
			if trial.Status != status {
				continue
			}
			if count == 0 {
				first = trial
				firstLabel = source.label
			}
			count++
		}
	}
	return first, firstLabel, count
}

func writeCompactProblem(writer io.Writer, style textStyle, sources []compactTrialSource, status engine.TrialStatus) {
	trial, source, count := compactProblem(sources, status)
	if count == 0 {
		return
	}
	fmt.Fprintf(writer, "  %s · %s · Trial %d · %s\n", style.status("Problem"), source, trial.Number, trialStatusLabel(status))
	if trial.Err != nil {
		fmt.Fprintf(writer, "    %s\n", quoted(trial.Err.Error()))
	} else if status == engine.TrialStatusInconclusive {
		failed := failedAttemptCount(trial.Run.History)
		fmt.Fprintf(writer, "    %d/%d attempts failed or did not start.\n", failed, len(trial.Run.History.Attempts))
	}
	writeCompactProblemExecutions(writer, trial)
	if omitted := count - 1; omitted > 0 {
		fmt.Fprintf(writer, "    %d more %s %s not shown.\n", omitted, status, pluralize("trial", omitted))
	}
}

func writeCompactProblemExecutions(writer io.Writer, trial engine.TrialResult) {
	if stageExecutionProblem(trial.Run.Setup) {
		writeCompactStage(writer, "Setup", trial.Run.Setup)
	}

	problemAttempts := make([]engine.Attempt, 0)
	for _, attempt := range trial.Run.History.Attempts {
		if attempt.Execution == nil || attempt.Execution.Err != nil || attempt.Execution.Response == nil {
			problemAttempts = append(problemAttempts, attempt)
		}
	}
	writeCompactAttempts(writer, problemAttempts, "problem attempt")

	if stageExecutionProblem(trial.Run.Observation) {
		writeCompactStage(writer, "Observation", trial.Run.Observation)
	}
}

func stageExecutionProblem(execution *engine.HTTPExecution) bool {
	return execution != nil && (execution.Err != nil || execution.Response == nil || execution.Response.StatusCode < http.StatusOK || execution.Response.StatusCode >= http.StatusMultipleChoices)
}

func writeCompactStage(writer io.Writer, label string, execution *engine.HTTPExecution) {
	fmt.Fprintf(writer, "    %-15s %s %s · %s\n", label, execution.Request.Method, requestTarget(execution.Request.URL), executionStatus(execution))
	writeCompactExecutionDetails(writer, execution, "      ")
}

func writeCompactExecutionDetails(writer io.Writer, execution *engine.HTTPExecution, indent string) {
	if execution.Response != nil && (len(execution.Response.Body) > 0 || execution.Response.BodyTruncated) {
		fmt.Fprintf(writer, "%sResponse        %s\n", indent, boundedResponseExcerpt(execution.Response, maxCompactResponseExcerptBytes))
	}
	if execution.Err != nil {
		fmt.Fprintf(writer, "%sError           %s\n", indent, quoted(execution.Err.Error()))
	}
}

func pluralize(word string, count int) string {
	if count == 1 {
		return word
	}
	return word + "s"
}

func reductionSummary(result engine.TrialsResult) reduction.TrialSummary {
	counts := trialCounts(result.Trials)
	return reduction.TrialSummary{Requested: result.Requested, Completed: len(result.Trials), Passed: counts.passed, Violated: counts.violated, Inconclusive: counts.inconclusive, Errored: counts.errored, StartedAt: result.StartedAt, CompletedAt: result.CompletedAt}
}

func writeReproduction(writer io.Writer, style textStyle, input Input) {
	arguments := []string{"concurtest", "run"}
	if input.Reduction != nil && input.Reduction.SelectedTrials != nil {
		arguments = append(arguments, "--attempts", strconv.Itoa(input.Reduction.Selected.Attempts), "--concurrency", strconv.Itoa(input.Reduction.Selected.Concurrency), "--no-reduce")
	}
	arguments = append(arguments, shellPath(input.ScenarioPath))
	fmt.Fprintf(writer, "\n%s\n  %s\n", style.heading("Reproduce"), strings.Join(arguments, " "))
}

func shellPath(path string) string {
	if strings.HasPrefix(path, "-") {
		path = "./" + path
	}
	if path != "" && strings.IndexFunc(path, func(r rune) bool {
		return !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_@%+=:,./-", r)
	}) == -1 {
		return path
	}
	return "'" + strings.ReplaceAll(path, "'", `'"'"'`) + "'"
}

func writeTrialEvidence(writer io.Writer, style textStyle, scenario engine.Scenario, trial engine.TrialResult, collection string) {
	label := fmt.Sprintf("Trial %d · %s", trial.Number, trialStatusLabel(trial.Status))
	if collection == "Baseline" && trial.Status == engine.TrialStatusViolated {
		label = fmt.Sprintf("Violation · Trial %d", trial.Number)
	}
	fmt.Fprintf(writer, "\n%s\n", style.status(label))
	fmt.Fprintf(writer, "  Duration        %s\n", displayDuration(trial.Run.Duration()))
	if trial.Err != nil {
		writeTrialProblem(writer, style, trial.Err)
	} else if trial.Status == engine.TrialStatusInconclusive {
		failed := failedAttemptCount(trial.Run.History)
		fmt.Fprintf(writer, "  Finding         %d of %d attempts failed or did not start, so this trial cannot be reported as a pass.\n", failed, len(trial.Run.History.Attempts))
	}
	writeInvariant(writer, style, scenario.Invariant, trial.Run.Evaluation)
	writeOptionalExecution(writer, "Setup", trial.Run.Setup)
	writeAttempts(writer, trial.Run.History)
	writeObservation(writer, scenario.Observation, trial.Run.Observation)
}

func writeRunProblem(writer io.Writer, style textStyle, runErr error) {
	fmt.Fprintf(writer, "\n%s\n", style.heading("Execution problem"))
	if errors.Is(runErr, context.Canceled) {
		fmt.Fprintln(writer, "  The trial sequence was canceled before all requested trials completed.")
		return
	}
	if errors.Is(runErr, context.DeadlineExceeded) {
		fmt.Fprintln(writer, "  The trial sequence deadline was exceeded before all requested trials completed.")
		return
	}
	fmt.Fprintf(writer, "  %s\n", quoted(runErr.Error()))
	fmt.Fprintln(writer, "  Check the target and scenario, then try again.")
}

func writeTrialProblem(writer io.Writer, style textStyle, runErr error) {
	if errors.Is(runErr, context.Canceled) {
		fmt.Fprintf(writer, "  %s         This trial was canceled.\n", style.status("Problem"))
		return
	}
	if errors.Is(runErr, context.DeadlineExceeded) {
		fmt.Fprintf(writer, "  %s         This trial's deadline was exceeded.\n", style.status("Problem"))
		return
	}
	fmt.Fprintf(writer, "  %s         %s\n", style.status("Problem"), quoted(runErr.Error()))
}

func writeInvariantSummary(writer io.Writer, style textStyle, invariant engine.Invariant, evaluation *engine.InvariantEvaluation) {
	fmt.Fprintf(writer, "\n%s\n", style.heading("Invariant"))
	writeInvariantLines(writer, invariant, evaluation, "  ")
}

func writeInvariant(writer io.Writer, style textStyle, invariant engine.Invariant, evaluation *engine.InvariantEvaluation) {
	fmt.Fprintf(writer, "\n  %s\n", style.heading("Invariant"))
	writeInvariantLines(writer, invariant, evaluation, "    ")
}

func writeInvariantLines(writer io.Writer, invariant engine.Invariant, evaluation *engine.InvariantEvaluation, indent string) {
	switch {
	case invariant.JSONIntegerMinimum != nil:
		definition := invariant.JSONIntegerMinimum
		path := formatJSONPath(definition.Path)
		fmt.Fprintf(writer, "%s%s\n", indent, definition.Name)
		fmt.Fprintf(writer, "%sExpected        %s >= %d\n", indent, path, definition.Minimum)
		if evaluation == nil || evaluation.JSONIntegerMinimum == nil {
			fmt.Fprintf(writer, "%sObserved        Not evaluated\n", indent)
			return
		}
		fmt.Fprintf(writer, "%sObserved        %s = %d\n", indent, path, evaluation.JSONIntegerMinimum.Observed)
	case invariant.MaximumSuccessfulAttempts != nil:
		definition := invariant.MaximumSuccessfulAttempts
		fmt.Fprintf(writer, "%s%s\n", indent, definition.Name)
		fmt.Fprintf(writer, "%sExpected        At most %d successful %s\n", indent, definition.Maximum, attemptWord(definition.Maximum))
		fmt.Fprintf(writer, "%sSuccess statuses %s\n", indent, successfulStatuses(definition.SuccessfulStatusCodes))
		if evaluation == nil || evaluation.MaximumSuccessfulAttempts == nil {
			fmt.Fprintf(writer, "%sObserved        Not evaluated\n", indent)
			return
		}
		historyEvaluation := evaluation.MaximumSuccessfulAttempts
		fmt.Fprintf(writer, "%sObserved        %d successful %s\n", indent, len(historyEvaluation.SuccessfulAttemptIDs), attemptWord(len(historyEvaluation.SuccessfulAttemptIDs)))
		fmt.Fprintf(writer, "%sSuccessful       %s\n", indent, attemptIDs(historyEvaluation.SuccessfulAttemptIDs))
		fmt.Fprintf(writer, "%sBeyond maximum   %s\n", indent, attemptIDs(historyEvaluation.OverLimitAttemptIDs))
	}
}

func successfulStatuses(statuses []int) string {
	if statuses == nil {
		return "HTTP 200–299 (default)"
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
		return "None"
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
	fmt.Fprintf(writer, "\n  %s\n", label)
	writeExecution(writer, execution, "    ")
}

func writeAttempts(writer io.Writer, history engine.History) {
	fmt.Fprintln(writer, "\n  Attempts")
	if len(history.Attempts) == 0 {
		fmt.Fprintln(writer, "    None recorded.")
		return
	}
	for _, attempt := range history.Attempts {
		if attempt.Execution == nil {
			fmt.Fprintf(writer, "    Attempt #%d     %s · not started\n", attempt.ID, attempt.OperationName)
			continue
		}
		execution := attempt.Execution
		fmt.Fprintf(writer, "    Attempt #%d     %s %s · %s · %s\n", attempt.ID, execution.Request.Method, requestTarget(execution.Request.URL), executionStatus(execution), displayDuration(execution.Duration()))
		fmt.Fprintf(writer, "      Operation     %s\n", attempt.OperationName)
		fmt.Fprintf(writer, "      Started       +%s\n", displayDuration(execution.StartedAt.Sub(history.StartedAt)))
		writeExecutionDetails(writer, execution, "      ")
	}
}

func writeObservation(writer io.Writer, configured *engine.HTTPRequest, execution *engine.HTTPExecution) {
	fmt.Fprintln(writer, "\n  Observation")
	if configured == nil {
		fmt.Fprintln(writer, "    Not configured.")
		return
	}
	if execution == nil {
		fmt.Fprintln(writer, "    Not reached.")
		return
	}
	writeExecution(writer, execution, "    ")
}

func writeExecution(writer io.Writer, execution *engine.HTTPExecution, indent string) {
	fmt.Fprintf(writer, "%s%s %s · %s · %s\n", indent, execution.Request.Method, requestTarget(execution.Request.URL), executionStatus(execution), displayDuration(execution.Duration()))
	writeExecutionDetails(writer, execution, indent)
}

func writeExecutionDetails(writer io.Writer, execution *engine.HTTPExecution, indent string) {
	if execution.Response != nil && (len(execution.Response.Body) > 0 || execution.Response.BodyTruncated) {
		fmt.Fprintf(writer, "%sResponse        %s\n", indent, responseExcerpt(execution.Response))
	}
	if execution.Err != nil {
		fmt.Fprintf(writer, "%sError           %s\n", indent, quoted(execution.Err.Error()))
	}
}

func executionStatus(execution *engine.HTTPExecution) string {
	if execution.Response == nil {
		return "no response"
	}
	return displayStatus(execution.Response.StatusCode)
}

func failedAttemptCount(history engine.History) int {
	failed := 0
	for _, attempt := range history.Attempts {
		if attempt.Execution == nil || attempt.Execution.Err != nil || attempt.Execution.Response == nil {
			failed++
		}
	}
	return failed
}

func requestTarget(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	target := parsed.EscapedPath()
	if target == "" && (parsed.IsAbs() || parsed.Host != "") {
		target = "/"
	}
	if target == "" {
		return rawURL
	}
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
	return boundedResponseExcerpt(response, maxResponseExcerptBytes)
}

func boundedResponseExcerpt(response *engine.HTTPResponse, maximum int) string {
	body := response.Body
	truncated := response.BodyTruncated
	if len(body) > maximum {
		body = body[:maximum]
		truncated = true
	}
	excerpt := strconv.QuoteToGraphic(string(body))
	if truncated {
		excerpt += " [truncated]"
	}
	return excerpt
}

func formatJSONPath(path []string) string {
	var formatted strings.Builder
	formatted.WriteByte('$')
	for _, segment := range path {
		fmt.Fprintf(&formatted, "[%q]", segment)
	}
	return formatted.String()
}

func quoted(value string) string { return strconv.QuoteToGraphic(value) }

func displayDuration(duration time.Duration) string {
	if duration < 0 {
		return "0s"
	}
	return duration.String()
}
