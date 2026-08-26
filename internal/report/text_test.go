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
	"github.com/eumarumar/concurtest/internal/reduction"
	"github.com/eumarumar/concurtest/internal/report"
)

func TestWriteTextPresentsViolationEvidenceSafely(t *testing.T) {
	t.Parallel()

	input := completedTextInput(engine.RunOutcomeViolated, -1)
	var output bytes.Buffer
	if err := report.WriteText(&output, input); err != nil {
		t.Fatalf("WriteText() error = %v", err)
	}

	text := output.String()
	assertContains(t, text,
		"VIOLATED",
		"1 of 1 completed trials demonstrated the violation.",
		"Requested       1",
		"Completed       1",
		"First violation Trial 1",
		"Attempts        2",
		"Concurrency     2",
		"final stock must be non-negative",
		"Expected        stock >= 0",
		"Observed        stock = -1",
		"Baseline evidence",
		"Violation · Trial 1",
		"POST /reset · HTTP 204 No Content",
		"Attempt #1     POST /purchase · HTTP 201 Created",
		"Attempt #2     POST /purchase · HTTP 409 Conflict",
		`Response        "{\"accepted\":true}"`,
		"GET /state?detail=full · HTTP 200 OK",
		`Response        "{\"stock\":-1}"`,
		"Reproduce\n  concurtest run scenarios/inventory.yaml",
	)
	if first, second := strings.Index(text, "Attempt #1"), strings.Index(text, "Attempt #2"); first < 0 || first >= second {
		t.Fatalf("attempt order is not stable:\n%s", text)
	}
	if strings.Contains(text, "request-secret") || strings.Contains(text, "response-secret") {
		t.Fatalf("report exposed a header value:\n%s", text)
	}
}

func TestWriteTextPresentsHistoryInvariant(t *testing.T) {
	t.Parallel()

	input := completedTextInput(engine.RunOutcomeViolated, -1)
	definition := engine.MaximumSuccessfulAttemptsInvariant{
		Name:                  "accepted purchases must not exceed stock",
		Maximum:               1,
		SuccessfulStatusCodes: []int{http.StatusCreated, http.StatusAccepted},
	}
	input.Scenario.Invariant = engine.Invariant{MaximumSuccessfulAttempts: &definition}
	input.Scenario.Observation = nil
	trial := &input.Result.Trials[0]
	trial.Run.Observation = nil
	trial.Run.History.Attempts[1].Execution.Response.StatusCode = http.StatusAccepted
	trial.Run.Evaluation = &engine.InvariantEvaluation{
		MaximumSuccessfulAttempts: &engine.MaximumSuccessfulAttemptsEvaluation{
			Invariant: definition, SuccessfulAttemptIDs: []int{1, 2}, OverLimitAttemptIDs: []int{2}, Violated: true,
		},
		Violated: true,
	}

	var output bytes.Buffer
	if err := report.WriteText(&output, input); err != nil {
		t.Fatalf("WriteText() error = %v", err)
	}
	assertContains(t, output.String(),
		"accepted purchases must not exceed stock",
		"Expected        At most 1 successful attempt",
		"Success statuses HTTP 201 Created, HTTP 202 Accepted",
		"Observed        2 successful attempts",
		"Successful       #1, #2",
		"Beyond maximum   #2",
		"Observation\n    Not configured.",
	)
}

func TestWriteTextGroupsEquivalentViolationsButSurfacesMaterialDifferences(t *testing.T) {
	t.Parallel()

	base := completedTextInput(engine.RunOutcomeViolated, -1)
	first := base.Result.Trials[0]
	second := completedTextInput(engine.RunOutcomeViolated, -1).Result.Trials[0]
	second.Number = 2
	second.Run.StartedAt = second.Run.StartedAt.Add(time.Second)
	second.Run.CompletedAt = second.Run.CompletedAt.Add(2 * time.Second)
	second.Run.History.Attempts[0].Execution.StartedAt = second.Run.History.Attempts[0].Execution.StartedAt.Add(time.Second)
	third := completedTextInput(engine.RunOutcomeViolated, -2).Result.Trials[0]
	third.Number = 3
	third.Run.Observation.Response.Body = []byte(`{"stock":-2}`)
	fourth := completedTextInput(engine.RunOutcomeViolated, -1).Result.Trials[0]
	fourth.Number = 4
	fourth.Run.History.Attempts[0].Execution.Response.StatusCode = http.StatusAccepted
	base.Result.Requested = 4
	base.Result.Trials = []engine.TrialResult{first, second, third, fourth}
	base.Result.CompletedAt = base.Result.StartedAt.Add(3 * time.Second)

	var output bytes.Buffer
	if err := report.WriteText(&output, base); err != nil {
		t.Fatalf("WriteText() error = %v", err)
	}
	text := output.String()
	if count := strings.Count(text, "Violation · Trial"); count != 3 {
		t.Fatalf("violation details = %d, want 3 distinct representatives\n%s", count, text)
	}
	assertContains(t, text,
		"Violation · Trial 1",
		"Violation · Trial 3",
		"Violation · Trial 4",
		"Trials 2 had the same violation evidence as Trial 1.",
		"Observed        stock = -2",
		"HTTP 202 Accepted",
	)
}

func TestWriteTextAlwaysSurfacesExecutionProblems(t *testing.T) {
	t.Parallel()

	input := completedTextInput(engine.RunOutcomeViolated, -1)
	violated := input.Result.Trials[0]
	inconclusive := completedTextInput(engine.RunOutcomeInconclusive, 0).Result.Trials[0]
	inconclusive.Number = 2
	inconclusive.Run.History.Attempts[0].Execution = failedExecution()
	errored := engine.TrialResult{
		Number: 3, Status: engine.TrialStatusErrored,
		Run: engine.RunResult{StartedAt: time.Unix(1_700_000_001, 0), CompletedAt: time.Unix(1_700_000_001, int64(time.Millisecond))},
		Err: errors.New("setup unavailable"),
	}
	input.Result.Requested = 3
	input.Result.Trials = []engine.TrialResult{violated, inconclusive, errored}

	var output bytes.Buffer
	if err := report.WriteText(&output, input); err != nil {
		t.Fatalf("WriteText() error = %v", err)
	}
	text := output.String()
	assertContains(t, text,
		"Trial 2 · INCONCLUSIVE",
		"1 of 2 attempts failed or did not start",
		`Error           "send request: network unavailable"`,
		"Trial 3 · ERRORED",
		`Problem         "setup unavailable"`,
		"Observation\n    Not reached.",
	)
	if strings.Index(text, "Violation · Trial 1") >= strings.Index(text, "Trial 2 · INCONCLUSIVE") || strings.Index(text, "Trial 2 · INCONCLUSIVE") >= strings.Index(text, "Trial 3 · ERRORED") {
		t.Fatalf("problem evidence is not in trial order:\n%s", text)
	}
}

func TestWriteTextVerboseExpandsEveryRetainedTrial(t *testing.T) {
	t.Parallel()

	input := completedTextInput(engine.RunOutcomeViolated, -1)
	first := input.Result.Trials[0]
	second := first
	second.Number = 2
	passed := completedTextInput(engine.RunOutcomePassed, 0).Result.Trials[0]
	passed.Number = 3
	input.Result.Requested = 3
	input.Result.Trials = []engine.TrialResult{first, second, passed}

	var compact, verbose bytes.Buffer
	if err := report.WriteText(&compact, input); err != nil {
		t.Fatal(err)
	}
	if err := report.WriteTextWithOptions(&verbose, input, report.TextOptions{Verbose: true}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(compact.String(), "Trial 3 · PASSED") {
		t.Fatalf("compact report expanded a passing trial:\n%s", compact.String())
	}
	assertContains(t, verbose.String(), "Violation · Trial 1", "Violation · Trial 2", "Trial 3 · PASSED")
	if strings.Contains(verbose.String(), "Use --verbose") || strings.Contains(verbose.String(), "same violation evidence") {
		t.Fatalf("verbose report retained compact summaries:\n%s", verbose.String())
	}
}

func TestWriteTextColorIsOptionalAndDoesNotColorCommands(t *testing.T) {
	t.Parallel()

	input := completedTextInput(engine.RunOutcomeViolated, -1)
	var plain, colored bytes.Buffer
	if err := report.WriteTextWithOptions(&plain, input, report.TextOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := report.WriteTextWithOptions(&colored, input, report.TextOptions{Color: true}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain.String(), "\x1b[") {
		t.Fatalf("plain report contains ANSI escapes: %q", plain.String())
	}
	assertContains(t, colored.String(), "\x1b[", "\x1b[0m")
	command := "  concurtest run scenarios/inventory.yaml\n"
	if !strings.Contains(colored.String(), command) || strings.Contains(command, "\x1b[") {
		t.Fatalf("reproduction command is not plain and copyable:\n%q", colored.String())
	}
}

func TestWriteTextReductionKeepsBaselineFirstAndAvoidsDuplicateEvidence(t *testing.T) {
	t.Parallel()

	input := completedTextInput(engine.RunOutcomeViolated, -1)
	input.Scenario.Attempts = 4
	input.Scenario.Concurrency = 4
	selected := completedTextInput(engine.RunOutcomeViolated, -1).Result
	input.Reduction = &reduction.Result{
		StartedAt: input.Result.StartedAt, CompletedAt: input.Result.CompletedAt.Add(time.Second), Baseline: input.Result,
		Candidates: []reduction.CandidateResult{{
			Candidate: reduction.Candidate{Attempts: 2, Concurrency: 2},
			Summary:   reduction.TrialSummary{Requested: 1, Completed: 1, Violated: 1},
			Accepted:  true, Trials: &selected,
		}},
		Selected: reduction.Candidate{Attempts: 2, Concurrency: 2}, SelectedTrials: &selected, Status: reduction.StatusReduced,
	}

	var compact bytes.Buffer
	if err := report.WriteText(&compact, input); err != nil {
		t.Fatal(err)
	}
	text := compact.String()
	assertContains(t, text,
		"Baseline evidence",
		"Reduction\n  Status          REDUCED",
		"Smallest observed failure",
		"Attempts      2",
		"Concurrency   2",
		"Selected candidate violations matched the baseline evidence.",
		"concurtest run --attempts 2 --concurrency 2 --no-reduce scenarios/inventory.yaml",
	)
	if strings.Index(text, "Baseline evidence") >= strings.Index(text, "Reduction\n") {
		t.Fatalf("baseline was not reported before reduction:\n%s", text)
	}

	var verbose bytes.Buffer
	if err := report.WriteTextWithOptions(&verbose, input, report.TextOptions{Verbose: true}); err != nil {
		t.Fatal(err)
	}
	assertContains(t, verbose.String(), "Candidate results", "Selected candidate evidence")
	if strings.Count(verbose.String(), "Violation · Trial 1") != 1 || strings.Count(verbose.String(), "Trial 1 · VIOLATED") != 1 {
		t.Fatalf("verbose report did not retain baseline and selected evidence:\n%s", verbose.String())
	}
}

func TestWriteTextQuotesReproductionPathsForPOSIXShells(t *testing.T) {
	t.Parallel()

	tests := []struct{ path, want string }{
		{"scenario.yaml", "concurtest run scenario.yaml"},
		{"path with spaces/scenario.yaml", "concurtest run 'path with spaces/scenario.yaml'"},
		{"owner's scenario.yaml", `concurtest run 'owner'"'"'s scenario.yaml'`},
		{"$(touch nope).yaml", "concurtest run '$(touch nope).yaml'"},
		{"-scenario.yaml", "concurtest run ./-scenario.yaml"},
		{"", "concurtest run ''"},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			input := completedTextInput(engine.RunOutcomePassed, 0)
			input.ScenarioPath = test.path
			var output bytes.Buffer
			if err := report.WriteText(&output, input); err != nil {
				t.Fatal(err)
			}
			assertContains(t, output.String(), test.want)
		})
	}
}

func TestWriteTextBoundsAndEscapesResponseBodies(t *testing.T) {
	t.Parallel()

	input := completedTextInput(engine.RunOutcomeViolated, -1)
	longBody := append(bytes.Repeat([]byte("x"), 600), []byte("hidden-tail")...)
	input.Result.Trials[0].Run.History.Attempts[0].Execution.Response.Body = longBody
	input.Result.Trials[0].Run.Observation.Response.Body = []byte("first\nsecond")
	input.Result.Trials[0].Run.Observation.Response.BodyTruncated = true
	var output bytes.Buffer
	if err := report.WriteText(&output, input); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if strings.Contains(text, "hidden-tail") {
		t.Fatal("report included bytes beyond the response excerpt limit")
	}
	if count := strings.Count(text, "[truncated]"); count != 2 {
		t.Fatalf("truncation markers = %d, want 2\n%s", count, text)
	}
	assertContains(t, text, `"first\nsecond" [truncated]`)
}

func TestWriteTextReportsInterruptedSequence(t *testing.T) {
	t.Parallel()

	input := completedTextInput(engine.RunOutcomePassed, 0)
	input.Result.Requested = 3
	input.RunError = context.Canceled
	var output bytes.Buffer
	if err := report.WriteText(&output, input); err != nil {
		t.Fatal(err)
	}
	assertContains(t, output.String(), "ERROR", "stopped after 1 of 3 requested trials", "Execution problem", "trial sequence was canceled")
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
	if err := report.WriteTextStart(nil, report.TextStartInput{}, report.TextOptions{}); err == nil {
		t.Fatal("WriteTextStart(nil) error = nil, want error")
	}

	var output bytes.Buffer
	input := completedTextInput(engine.RunOutcomePassed, 0)
	input.Result.Status = "unknown"
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
	setup := successfulExecution(scenario.Setup, start.Add(time.Millisecond), 2*time.Millisecond, http.StatusNoContent, nil)
	first := successfulExecution(&scenario.Operation.Request, start.Add(12*time.Millisecond), 10*time.Millisecond, http.StatusCreated, []byte(`{"accepted":true}`))
	second := successfulExecution(&scenario.Operation.Request, start.Add(13*time.Millisecond), 8*time.Millisecond, http.StatusConflict, []byte("out of stock"))
	observation := successfulExecution(scenario.Observation, start.Add(60*time.Millisecond), 5*time.Millisecond, http.StatusOK, []byte(`{"stock":-1}`))
	evaluation := engine.InvariantEvaluation{
		JSONIntegerMinimum: &engine.JSONIntegerMinimumEvaluation{Invariant: *scenario.Invariant.JSONIntegerMinimum, Observed: observed, Violated: outcome == engine.RunOutcomeViolated},
		Violated:           outcome == engine.RunOutcomeViolated,
	}
	status := engine.TrialStatusPassed
	switch outcome {
	case engine.RunOutcomeViolated:
		status = engine.TrialStatusViolated
	case engine.RunOutcomeInconclusive:
		status = engine.TrialStatusInconclusive
	}
	run := engine.RunResult{
		StartedAt: start, CompletedAt: start.Add(80 * time.Millisecond), Setup: setup,
		History:     engine.History{StartedAt: start.Add(10 * time.Millisecond), CompletedAt: start.Add(50 * time.Millisecond), Attempts: []engine.Attempt{{ID: 1, OperationName: "purchase", Execution: first}, {ID: 2, OperationName: "purchase", Execution: second}}},
		Observation: observation, Evaluation: &evaluation, Outcome: outcome,
	}
	return report.TextInput{
		ScenarioPath: "scenarios/inventory.yaml", Scenario: scenario,
		Result: engine.TrialsResult{Requested: 1, Trials: []engine.TrialResult{{Number: 1, Status: status, Run: run}}, StartedAt: start, CompletedAt: start.Add(80 * time.Millisecond), Status: status},
	}
}

func testScenario() engine.Scenario {
	secretHeader := http.Header{"Authorization": {"request-secret"}}
	setup := engine.HTTPRequest{Method: http.MethodPost, URL: "http://example.test/reset", Header: secretHeader}
	return engine.Scenario{
		Setup:     &setup,
		Operation: engine.Operation{Name: "purchase", Request: engine.HTTPRequest{Method: http.MethodPost, URL: "http://example.test/purchase", Header: secretHeader}},
		Attempts:  2, Concurrency: 2,
		Observation: &engine.HTTPRequest{Method: http.MethodGet, URL: "http://example.test/state?detail=full", Header: secretHeader},
		Invariant:   engine.Invariant{JSONIntegerMinimum: &engine.JSONIntegerMinimumInvariant{Name: "final stock must be non-negative", Field: "stock", Minimum: 0}},
	}
}

func successfulExecution(request *engine.HTTPRequest, startedAt time.Time, duration time.Duration, status int, body []byte) *engine.HTTPExecution {
	return &engine.HTTPExecution{
		Request: *request, StartedAt: startedAt, CompletedAt: startedAt.Add(duration),
		Response: &engine.HTTPResponse{StatusCode: status, Header: http.Header{"X-Secret": {"response-secret"}}, Body: body},
	}
}

func failedExecution() *engine.HTTPExecution {
	return &engine.HTTPExecution{
		Request:   engine.HTTPRequest{Method: http.MethodPost, URL: "http://example.test/purchase"},
		StartedAt: time.Unix(1_700_000_000, 0), CompletedAt: time.Unix(1_700_000_000, int64(time.Millisecond)),
		Err: errors.New("send request: network unavailable"),
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

type errorWriter struct{ err error }

func (writer errorWriter) Write([]byte) (int, error) { return 0, writer.err }
