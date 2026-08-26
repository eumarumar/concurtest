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

const maxResponseExcerptBytes = 512

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

	writeTrialCollection(buffer, style, input.Scenario, input.Result, "Baseline", options, nil)
	if input.Reduction != nil {
		writeReduction(buffer, style, input.Scenario, *input.Reduction, input.RunError, options)
	}
	writeReproduction(buffer, style, input)

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
		return fmt.Sprintf("All %d completed trials passed.", completed)
	case engine.TrialStatusViolated:
		return fmt.Sprintf("%d of %d completed trials demonstrated the violation.", counts.violated, completed)
	case engine.TrialStatusInconclusive:
		return fmt.Sprintf("%d of %d completed trials were inconclusive.", counts.inconclusive, completed)
	case engine.TrialStatusErrored:
		return fmt.Sprintf("%d of %d completed trials encountered execution problems.", counts.errored, completed)
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

type violationGroup struct {
	representative int
	additional     []int
}

func writeTrialCollection(writer io.Writer, style textStyle, scenario engine.Scenario, result engine.TrialsResult, label string, options TextOptions, excludeKeys map[string]struct{}) {
	if len(result.Trials) == 0 {
		fmt.Fprintf(writer, "\n%s\n  No trial evidence was recorded.\n", style.heading(label+" evidence"))
		return
	}

	seen := make(map[string]*violationGroup)
	groups := make([]*violationGroup, 0)
	detailed := make([]engine.TrialResult, 0, len(result.Trials))
	for _, trial := range result.Trials {
		if options.Verbose {
			detailed = append(detailed, trial)
			continue
		}
		switch trial.Status {
		case engine.TrialStatusViolated:
			key := trialEvidenceKey(trial)
			if _, excluded := excludeKeys[key]; excluded {
				continue
			}
			if group, ok := seen[key]; ok {
				group.additional = append(group.additional, trial.Number)
				continue
			}
			group := &violationGroup{representative: trial.Number}
			seen[key] = group
			groups = append(groups, group)
			detailed = append(detailed, trial)
		case engine.TrialStatusInconclusive, engine.TrialStatusErrored:
			detailed = append(detailed, trial)
		}
	}

	if len(detailed) == 0 {
		counts := trialCounts(result.Trials)
		if counts.passed == len(result.Trials) {
			fmt.Fprintf(writer, "\n%s\n  All %d passing trials are summarized above.\n", style.heading(label+" evidence"), counts.passed)
		}
		return
	}

	fmt.Fprintf(writer, "\n%s\n", style.heading(label+" evidence"))
	for _, trial := range detailed {
		writeTrialEvidence(writer, style, scenario, trial, label)
	}
	if !options.Verbose {
		for _, group := range groups {
			if len(group.additional) > 0 {
				fmt.Fprintf(writer, "\n  %s\n", style.note(fmt.Sprintf("Trials %s had the same violation evidence as Trial %d.", formatTrialNumbers(group.additional), group.representative)))
			}
		}
		counts := trialCounts(result.Trials)
		if counts.passed > 0 {
			fmt.Fprintf(writer, "  %s\n", style.note(fmt.Sprintf("%d passing trials are summarized.", counts.passed)))
		}
		fmt.Fprintf(writer, "  %s\n", style.note("Use --verbose to display every retained trial."))
	}
}

func writeReduction(writer io.Writer, style textStyle, scenario engine.Scenario, result reduction.Result, runErr error, options TextOptions) {
	status := reductionStatusLabel(result.Status, runErr)
	fmt.Fprintf(writer, "\n%s\n", style.heading("Reduction"))
	fmt.Fprintf(writer, "  Status          %s\n", style.status(status))
	fmt.Fprintf(writer, "  Candidates      %d evaluated\n", len(result.Candidates))
	fmt.Fprintf(writer, "  Duration        %s\n", displayDuration(result.Duration()))

	if options.Verbose && len(result.Candidates) > 0 {
		fmt.Fprintln(writer, "  Candidate results")
		for _, candidate := range result.Candidates {
			writeCandidateResult(writer, candidate)
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
		fmt.Fprintln(writer, "  Smallest observed failure")
		fmt.Fprintf(writer, "    Attempts      %d\n", result.Selected.Attempts)
		fmt.Fprintf(writer, "    Concurrency   %d\n", result.Selected.Concurrency)
		fmt.Fprintf(writer, "    Violations    %d of %d trials\n", selected.Violated, selected.Requested)
		if result.Status == reduction.StatusUnchanged {
			fmt.Fprintln(writer, "  Finding         No smaller tested configuration met the reproduction rule.")
		}
		if result.Status == reduction.StatusLimited {
			fmt.Fprintf(writer, "  Finding         The %d-candidate limit was reached; smaller untested configurations may remain.\n", reduction.MaxCandidates)
		}
		fmt.Fprintln(writer, "  Note            This is an observed failure, not proof that no smaller failure exists.")

		if result.Status == reduction.StatusReduced {
			baselineKeys := violationKeys(result.Baseline.Trials)
			renderable := countRenderableTrials(*result.SelectedTrials, options, baselineKeys)
			writeTrialCollection(writer, style, scenario, *result.SelectedTrials, "Selected candidate", options, baselineKeys)
			if !options.Verbose && renderable == 0 {
				fmt.Fprintln(writer, "  Selected candidate violations matched the baseline evidence.")
			}
		}
	}
}

func countRenderableTrials(result engine.TrialsResult, options TextOptions, exclude map[string]struct{}) int {
	if options.Verbose {
		return len(result.Trials)
	}
	seen := make(map[string]struct{})
	count := 0
	for _, trial := range result.Trials {
		switch trial.Status {
		case engine.TrialStatusViolated:
			key := trialEvidenceKey(trial)
			if _, ok := exclude[key]; ok {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			count++
		case engine.TrialStatusInconclusive, engine.TrialStatusErrored:
			count++
		}
	}
	return count
}

func violationKeys(trials []engine.TrialResult) map[string]struct{} {
	keys := make(map[string]struct{})
	for _, trial := range trials {
		if trial.Status == engine.TrialStatusViolated {
			keys[trialEvidenceKey(trial)] = struct{}{}
		}
	}
	return keys
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
	writeTrialCollection(writer, style, scenario, *candidate.Trials, "Interrupted candidate", options, violationKeys(result.Baseline.Trials))
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
		fmt.Fprintf(writer, "%s%s\n", indent, definition.Name)
		fmt.Fprintf(writer, "%sExpected        %s >= %d\n", indent, definition.Field, definition.Minimum)
		if evaluation == nil || evaluation.JSONIntegerMinimum == nil {
			fmt.Fprintf(writer, "%sObserved        Not evaluated\n", indent)
			return
		}
		fmt.Fprintf(writer, "%sObserved        %s = %d\n", indent, definition.Field, evaluation.JSONIntegerMinimum.Observed)
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

func trialEvidenceKey(trial engine.TrialResult) string {
	var key strings.Builder
	fmt.Fprintf(&key, "status=%q;trial_error=%q;outcome=%q;", trial.Status, errorText(trial.Err), trial.Run.Outcome)
	appendInvariantKey(&key, trial.Run.Evaluation)
	appendExecutionKey(&key, "setup", trial.Run.Setup)
	fmt.Fprintf(&key, "attempts=%d;", len(trial.Run.History.Attempts))
	for _, attempt := range trial.Run.History.Attempts {
		fmt.Fprintf(&key, "attempt=%d,%q;", attempt.ID, attempt.OperationName)
		appendExecutionKey(&key, "execution", attempt.Execution)
	}
	appendExecutionKey(&key, "observation", trial.Run.Observation)
	return key.String()
}

func appendInvariantKey(key *strings.Builder, evaluation *engine.InvariantEvaluation) {
	if evaluation == nil {
		key.WriteString("invariant=nil;")
		return
	}
	fmt.Fprintf(key, "violated=%t;", evaluation.Violated)
	if evaluation.JSONIntegerMinimum != nil {
		value := evaluation.JSONIntegerMinimum
		fmt.Fprintf(key, "json=%q,%q,%d,%d,%t;", value.Invariant.Name, value.Invariant.Field, value.Invariant.Minimum, value.Observed, value.Violated)
	}
	if evaluation.MaximumSuccessfulAttempts != nil {
		value := evaluation.MaximumSuccessfulAttempts
		fmt.Fprintf(key, "history=%q,%d,%v,%v,%v,%t;", value.Invariant.Name, value.Invariant.Maximum, value.Invariant.SuccessfulStatusCodes, value.SuccessfulAttemptIDs, value.OverLimitAttemptIDs, value.Violated)
	}
}

func appendExecutionKey(key *strings.Builder, label string, execution *engine.HTTPExecution) {
	if execution == nil {
		fmt.Fprintf(key, "%s=nil;", label)
		return
	}
	fmt.Fprintf(key, "%s=%q,%q,%q;", label, execution.Request.Method, requestTarget(execution.Request.URL), errorText(execution.Err))
	if execution.Response == nil {
		key.WriteString("response=nil;")
		return
	}
	fmt.Fprintf(key, "response=%d,%q,%t;", execution.Response.StatusCode, responseExcerpt(execution.Response), execution.Response.BodyTruncated)
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func formatTrialNumbers(numbers []int) string {
	parts := make([]string, 0, len(numbers))
	for start := 0; start < len(numbers); {
		end := start
		for end+1 < len(numbers) && numbers[end+1] == numbers[end]+1 {
			end++
		}
		if end == start {
			parts = append(parts, strconv.Itoa(numbers[start]))
		} else {
			parts = append(parts, fmt.Sprintf("%d–%d", numbers[start], numbers[end]))
		}
		start = end + 1
	}
	return strings.Join(parts, ", ")
}

func quoted(value string) string { return strconv.QuoteToGraphic(value) }

func displayDuration(duration time.Duration) string {
	if duration < 0 {
		return "0s"
	}
	return duration.String()
}
