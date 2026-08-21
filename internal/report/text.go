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
	"time"

	"github.com/eumarumar/concurtest/internal/engine"
)

const maxResponseExcerptBytes = 512

// TextInput contains the configured work and recorded evidence for one report.
type TextInput struct {
	ScenarioPath string
	Scenario     engine.Scenario
	Result       engine.RunResult
	RunError     error
}

// WriteText writes a deterministic, human-readable report. It preserves the
// stable attempt ordering already established by the engine.
func WriteText(writer io.Writer, input TextInput) error {
	if writer == nil {
		return errors.New("write text report: nil writer")
	}
	resultLabel, err := resultLabel(input)
	if err != nil {
		return err
	}

	buffer := bufio.NewWriter(writer)
	fmt.Fprintf(buffer, "Result: %s\n", resultLabel)
	fmt.Fprintf(buffer, "Duration: %s\n", displayDuration(input.Result.Duration()))
	fmt.Fprintf(
		buffer,
		"Execution: %d attempts, concurrency %d\n",
		input.Scenario.Attempts,
		input.Scenario.Concurrency,
	)

	if input.RunError != nil {
		writeRunProblem(buffer, input.RunError)
	} else if input.Result.Outcome == engine.RunOutcomeInconclusive {
		failed := failedAttemptCount(input.Result.History)
		fmt.Fprintf(
			buffer,
			"Reason: %d of %d operation attempts failed or did not start, so this run cannot be reported as a pass.\n",
			failed,
			len(input.Result.History.Attempts),
		)
	}

	writeInvariant(buffer, input.Scenario.Invariant, input.Result.Evaluation)
	writeOptionalExecution(buffer, "Setup", input.Result.Setup)
	writeAttempts(buffer, input.Result.History)
	writeObservation(buffer, input.Result.Observation)
	fmt.Fprintf(buffer, "\nReproduce: concurtest run %s\n", strconv.QuoteToGraphic(input.ScenarioPath))

	if err := buffer.Flush(); err != nil {
		return fmt.Errorf("write text report: %w", err)
	}
	return nil
}

func resultLabel(input TextInput) (string, error) {
	if input.RunError != nil {
		return "ERROR", nil
	}
	switch input.Result.Outcome {
	case engine.RunOutcomePassed:
		return "PASSED", nil
	case engine.RunOutcomeViolated:
		return "VIOLATED", nil
	case engine.RunOutcomeInconclusive:
		return "INCONCLUSIVE", nil
	default:
		return "", fmt.Errorf("write text report: unknown run outcome %q", input.Result.Outcome)
	}
}

func writeRunProblem(writer io.Writer, runErr error) {
	if errors.Is(runErr, context.Canceled) {
		fmt.Fprintln(writer, "Problem: the run was canceled.")
		return
	}
	fmt.Fprintf(writer, "Problem: %s\n", quoted(runErr.Error()))
	fmt.Fprintln(writer, "Next step: check the target and scenario, then try again.")
}

func writeInvariant(
	writer io.Writer,
	invariant engine.JSONIntegerMinimumInvariant,
	evaluation *engine.InvariantEvaluation,
) {
	fmt.Fprintln(writer, "\nInvariant:")
	fmt.Fprintf(writer, "  Name: %s\n", quoted(invariant.Name))
	fmt.Fprintf(writer, "  Expected: %s >= %d\n", quoted(invariant.Field), invariant.Minimum)
	if evaluation == nil {
		fmt.Fprintln(writer, "  Observed: not evaluated")
		return
	}
	fmt.Fprintf(writer, "  Observed: %s = %d\n", quoted(invariant.Field), evaluation.Observed)
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

func writeObservation(writer io.Writer, execution *engine.HTTPExecution) {
	fmt.Fprintln(writer, "\nObservation:")
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
