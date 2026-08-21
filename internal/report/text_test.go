package report_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/eumarumar/concurtest/internal/engine"
	"github.com/eumarumar/concurtest/internal/report"
)

func TestWriteTextReportsViolationEvidence(t *testing.T) {
	t.Parallel()

	input := completedTextInput(engine.RunOutcomeViolated, -1)
	var output bytes.Buffer
	if err := report.WriteText(&output, input); err != nil {
		t.Fatalf("WriteText() error = %v", err)
	}

	reportText := output.String()
	assertContains(t, reportText,
		"Result: VIOLATED",
		"Duration: 80ms",
		"Execution: 2 attempts, concurrency 2",
		`Name: "final stock must be non-negative"`,
		`Expected: "stock" >= 0`,
		`Observed: "stock" = -1`,
		"Setup:",
		`Request: "POST" "/reset"`,
		`#1 "purchase"`,
		"Started: +2ms",
		"Duration: 10ms",
		"Status: HTTP 201 Created",
		`Response: "{\"accepted\":true}"`,
		`#2 "purchase"`,
		"Status: HTTP 409 Conflict",
		"Observation:",
		`Request: "GET" "/state?detail=full"`,
		`Response: "{\"stock\":-1}"`,
		`Reproduce: concurtest run "scenarios/inventory.yaml"`,
	)

	first := strings.Index(reportText, `#1 "purchase"`)
	second := strings.Index(reportText, `#2 "purchase"`)
	if first < 0 || second < 0 || first >= second {
		t.Fatalf("attempt order is not stable:\n%s", reportText)
	}
	if strings.Contains(reportText, "request-secret") || strings.Contains(reportText, "response-secret") {
		t.Fatalf("report exposed a header value:\n%s", reportText)
	}
}

func TestWriteTextClassifiesCompletedOutcomes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		outcome     engine.RunOutcome
		wantResult  string
		wantReason  string
		attempts    []engine.Attempt
		observation int64
	}{
		{
			name:        "passed",
			outcome:     engine.RunOutcomePassed,
			wantResult:  "Result: PASSED",
			observation: 0,
		},
		{
			name:       "inconclusive failed attempt",
			outcome:    engine.RunOutcomeInconclusive,
			wantResult: "Result: INCONCLUSIVE",
			wantReason: "2 of 2 operation attempts failed or did not start",
			attempts: []engine.Attempt{
				{ID: 1, OperationName: "purchase", Execution: failedExecution()},
				{ID: 2, OperationName: "purchase", Execution: nil},
			},
			observation: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := completedTextInput(test.outcome, test.observation)
			if test.attempts != nil {
				input.Result.History.Attempts = test.attempts
			}
			var output bytes.Buffer
			if err := report.WriteText(&output, input); err != nil {
				t.Fatalf("WriteText() error = %v", err)
			}
			assertContains(t, output.String(), test.wantResult)
			if test.wantReason != "" {
				assertContains(t, output.String(), test.wantReason, "Status: not started", `Error: "send request: network unavailable"`)
			}
		})
	}
}

func TestWriteTextReportsPartialRunErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		runErr   error
		wantText []string
	}{
		{
			name:     "canceled",
			runErr:   context.Canceled,
			wantText: []string{"Result: ERROR", "Problem: the run was canceled.", "Observed: not evaluated", "None recorded.", "Not reached."},
		},
		{
			name:     "stage failure",
			runErr:   errors.New("observation returned HTTP 503"),
			wantText: []string{"Result: ERROR", `Problem: "observation returned HTTP 503"`, "Next step: check the target and scenario, then try again."},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := report.TextInput{
				ScenarioPath: "scenario.yaml",
				Scenario:     testScenario(),
				Result: engine.RunResult{
					StartedAt:   time.Unix(0, 0),
					CompletedAt: time.Unix(0, int64(time.Millisecond)),
				},
				RunError: test.runErr,
			}
			var output bytes.Buffer
			if err := report.WriteText(&output, input); err != nil {
				t.Fatalf("WriteText() error = %v", err)
			}
			assertContains(t, output.String(), test.wantText...)
		})
	}
}

func TestWriteTextBoundsAndEscapesResponseBodies(t *testing.T) {
	t.Parallel()

	input := completedTextInput(engine.RunOutcomeViolated, -1)
	input.Result.History.Attempts[0].Execution.Response.Body = []byte(strings.Repeat("x", 513) + "hidden-tail")
	input.Result.History.Attempts[1].Execution.Response.Body = []byte("first\nsecond")
	input.Result.History.Attempts[1].Execution.Response.BodyTruncated = true

	var output bytes.Buffer
	if err := report.WriteText(&output, input); err != nil {
		t.Fatalf("WriteText() error = %v", err)
	}
	reportText := output.String()
	if strings.Contains(reportText, "hidden-tail") {
		t.Fatal("report included bytes beyond the response excerpt limit")
	}
	if count := strings.Count(reportText, "[truncated]"); count != 2 {
		t.Errorf("truncation markers = %d, want 2\n%s", count, reportText)
	}
	if !strings.Contains(reportText, `"first\nsecond" [truncated]`) {
		t.Fatalf("response body was not escaped onto one line:\n%s", reportText)
	}
}

func TestWriteTextReturnsWriterAndInputErrors(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("write failed")
	if err := report.WriteText(errorWriter{err: wantErr}, completedTextInput(engine.RunOutcomePassed, 0)); !errors.Is(err, wantErr) {
		t.Fatalf("WriteText() error = %v, want wrapped %v", err, wantErr)
	}
	if err := report.WriteText(nil, completedTextInput(engine.RunOutcomePassed, 0)); err == nil {
		t.Fatal("WriteText(nil) error = nil, want error")
	}

	var output bytes.Buffer
	input := completedTextInput("unknown", 0)
	if err := report.WriteText(&output, input); err == nil {
		t.Fatal("WriteText() error = nil, want unknown outcome error")
	}
	if output.Len() != 0 {
		t.Errorf("output = %q, want no output for invalid input", output.String())
	}
}

func completedTextInput(outcome engine.RunOutcome, observed int64) report.TextInput {
	start := time.Unix(1_700_000_000, 0)
	scenario := testScenario()
	setup := successfulExecution(
		scenario.Setup,
		start.Add(time.Millisecond),
		2*time.Millisecond,
		http.StatusNoContent,
		nil,
	)
	first := successfulExecution(
		&scenario.Operation.Request,
		start.Add(12*time.Millisecond),
		10*time.Millisecond,
		http.StatusCreated,
		[]byte(`{"accepted":true}`),
	)
	second := successfulExecution(
		&scenario.Operation.Request,
		start.Add(13*time.Millisecond),
		8*time.Millisecond,
		http.StatusConflict,
		[]byte("out of stock"),
	)
	observation := successfulExecution(
		&scenario.Observation,
		start.Add(60*time.Millisecond),
		5*time.Millisecond,
		http.StatusOK,
		[]byte(`{"stock":-1}`),
	)
	evaluation := engine.InvariantEvaluation{
		Invariant: scenario.Invariant,
		Observed:  observed,
		Violated:  outcome == engine.RunOutcomeViolated,
	}

	return report.TextInput{
		ScenarioPath: "scenarios/inventory.yaml",
		Scenario:     scenario,
		Result: engine.RunResult{
			StartedAt:   start,
			CompletedAt: start.Add(80 * time.Millisecond),
			Setup:       setup,
			History: engine.History{
				StartedAt:   start.Add(10 * time.Millisecond),
				CompletedAt: start.Add(50 * time.Millisecond),
				Attempts: []engine.Attempt{
					{ID: 1, OperationName: "purchase", Execution: first},
					{ID: 2, OperationName: "purchase", Execution: second},
				},
			},
			Observation: observation,
			Evaluation:  &evaluation,
			Outcome:     outcome,
		},
	}
}

func testScenario() engine.Scenario {
	secretHeader := http.Header{"Authorization": {"request-secret"}}
	setup := engine.HTTPRequest{Method: http.MethodPost, URL: "http://example.test/reset", Header: secretHeader}
	return engine.Scenario{
		Setup: &setup,
		Operation: engine.Operation{
			Name: "purchase",
			Request: engine.HTTPRequest{
				Method: http.MethodPost,
				URL:    "http://example.test/purchase",
				Header: secretHeader,
			},
		},
		Attempts:    2,
		Concurrency: 2,
		Observation: engine.HTTPRequest{
			Method: http.MethodGet,
			URL:    "http://example.test/state?detail=full",
			Header: secretHeader,
		},
		Invariant: engine.JSONIntegerMinimumInvariant{
			Name:    "final stock must be non-negative",
			Field:   "stock",
			Minimum: 0,
		},
	}
}

func successfulExecution(
	request *engine.HTTPRequest,
	startedAt time.Time,
	duration time.Duration,
	status int,
	body []byte,
) *engine.HTTPExecution {
	return &engine.HTTPExecution{
		Request:     *request,
		StartedAt:   startedAt,
		CompletedAt: startedAt.Add(duration),
		Response: &engine.HTTPResponse{
			StatusCode: status,
			Header:     http.Header{"X-Secret": {"response-secret"}},
			Body:       body,
		},
	}
}

func failedExecution() *engine.HTTPExecution {
	return &engine.HTTPExecution{
		Request:     engine.HTTPRequest{Method: http.MethodPost, URL: "http://example.test/purchase"},
		StartedAt:   time.Unix(1_700_000_000, 0),
		CompletedAt: time.Unix(1_700_000_000, int64(time.Millisecond)),
		Err:         errors.New("send request: network unavailable"),
	}
}

func assertContains(t *testing.T, value string, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if !strings.Contains(value, fragment) {
			t.Errorf("output does not contain %q:\n%s", fragment, value)
		}
	}
}

type errorWriter struct {
	err error
}

func (writer errorWriter) Write([]byte) (int, error) {
	return 0, writer.err
}
