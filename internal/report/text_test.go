package report_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/eumarumar/concurtest/internal/engine"
	"github.com/eumarumar/concurtest/internal/reduction"
	"github.com/eumarumar/concurtest/internal/report"
)

func TestWriteTextStartWarningMatchesConcurrency(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		concurrency int
		warning     string
	}{
		{1, "Warning · This run sends requests and may change target data."},
		{2, "Warning · This run sends concurrent requests and may change target data."},
	} {
		t.Run(fmt.Sprint(test.concurrency), func(t *testing.T) {
			var output bytes.Buffer
			if err := report.WriteTextStart(&output, report.TextStartInput{Concurrency: test.concurrency}, report.TextOptions{}); err != nil {
				t.Fatal(err)
			}
			assertContains(t, output.String(), test.warning)
			if test.concurrency == 1 && strings.Contains(output.String(), "concurrent requests") {
				t.Fatalf("sequential run warning claims concurrency: %s", output.String())
			}
		})
	}
}

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
		"1/1 trials demonstrated the violation.",
		"Requested       1",
		"Completed       1",
		"First violation Trial 1",
		"Attempts        2",
		"Concurrency     2",
		"final stock must be non-negative",
		"Expected        $[\"stock\"] >= 0",
		"Observed        $[\"stock\"] = -1",
		"Evidence",
		"Baseline failure · Trial 1",
		"Attempt #1     POST /purchase · HTTP 201 Created",
		"Attempt #2     POST /purchase · HTTP 409 Conflict",
		`Response        "{\"accepted\":true}"`,
		"GET /state?detail=full · HTTP 200 OK",
		`Response        "{\"stock\":-1}"`,
		"Reproduce\n  concurtest run --attempts 2 --concurrency 2 --no-reduce scenarios/inventory.yaml",
		"Run with --verbose for all trial evidence.",
	)
	if strings.Contains(text, "POST /reset") {
		t.Fatalf("compact evidence included setup details:\n%s", text)
	}
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
	input.Scenario.Attempts = 3
	trial := &input.Result.Trials[0]
	trial.Run.Observation = nil
	trial.Run.History.Attempts[1].Execution.Response.StatusCode = http.StatusAccepted
	trial.Run.History.Attempts = append(trial.Run.History.Attempts, engine.Attempt{
		ID:            3,
		OperationName: "purchase",
		Execution: successfulExecution(
			&input.Scenario.Operation.Request,
			trial.Run.History.CompletedAt.Add(-time.Millisecond),
			time.Millisecond,
			http.StatusConflict,
			[]byte("out of stock"),
		),
	})
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
		"Evidence\n  Baseline failure · Trial 1",
		"Run with --verbose for all trial evidence.",
	)
	if strings.Contains(output.String(), "Observation") {
		t.Fatalf("compact history evidence included an unused observation:\n%s", output.String())
	}
	if strings.Contains(output.String(), "Attempt #3") {
		t.Fatalf("compact history evidence included an attempt unrelated to the violation:\n%s", output.String())
	}
}

func TestWriteTextPresentsNestedJSONPath(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		path []string
		want string
	}{
		{[]string{"data", "quantity"}, `$["data"]["quantity"]`},
		{[]string{"data", "Products", "0", "BasketItem", "quantity"}, `$["data"]["Products"]["0"]["BasketItem"]["quantity"]`},
	} {
		t.Run(test.want, func(t *testing.T) {
			input := completedTextInput(engine.RunOutcomePassed, 2)
			input.Scenario.Invariant.JSONIntegerMinimum.Path = test.path
			input.Result.Trials[0].Run.Evaluation.JSONIntegerMinimum.Invariant.Path = append([]string(nil), test.path...)

			var output bytes.Buffer
			if err := report.WriteText(&output, input); err != nil {
				t.Fatalf("WriteText() error = %v", err)
			}
			assertContains(t, output.String(),
				"Expected        "+test.want+" >= 0",
				"Observed        "+test.want+" = 2",
			)
		})
	}
}

func TestWriteTextCompactShowsOneViolationDespiteDistinctEvidence(t *testing.T) {
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
	if count := strings.Count(text, "Baseline failure · Trial"); count != 1 {
		t.Fatalf("compact violation evidence = %d blocks, want 1\n%s", count, text)
	}
	assertContains(t, text,
		"Baseline failure · Trial 1",
		"Run with --verbose for all trial evidence.",
	)
	if strings.Contains(text, "Trial 3") || strings.Contains(text, "HTTP 202 Accepted") {
		t.Fatalf("compact report expanded later distinct violations:\n%s", text)
	}
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
	secondInconclusive := inconclusive
	secondInconclusive.Number = 4
	secondErrored := errored
	secondErrored.Number = 5
	input.Result.Requested = 5
	input.Result.Trials = []engine.TrialResult{violated, inconclusive, errored, secondInconclusive, secondErrored}

	var output bytes.Buffer
	if err := report.WriteText(&output, input); err != nil {
		t.Fatalf("WriteText() error = %v", err)
	}
	text := output.String()
	assertContains(t, text,
		"Problem · Baseline · Trial 3 · ERRORED",
		`"setup unavailable"`,
		"Problem · Baseline · Trial 2 · INCONCLUSIVE",
		"1/2 attempts failed or did not start",
		`Error           "send request: network unavailable"`,
		"1 more errored trial not shown.",
		"1 more inconclusive trial not shown.",
	)
	if strings.Count(text, "Problem · Baseline") != 2 {
		t.Fatalf("compact report did not show one example per problem status:\n%s", text)
	}
}

func TestWriteTextCompactPrefersInterruptedCandidateProblems(t *testing.T) {
	t.Parallel()

	input := completedTextInput(engine.RunOutcomeViolated, -1)
	baselineProblem := engine.TrialResult{
		Number: 2,
		Status: engine.TrialStatusErrored,
		Err:    errors.New("baseline unavailable"),
	}
	input.Result.Requested = 2
	input.Result.Trials = append(input.Result.Trials, baselineProblem)
	input.RunError = errors.New("reduction interrupted")
	interruptedProblem := engine.TrialResult{
		Number: 1,
		Status: engine.TrialStatusErrored,
		Err:    errors.New("candidate unavailable"),
	}
	interruptedTrials := engine.TrialsResult{
		Requested: 1,
		Trials:    []engine.TrialResult{interruptedProblem},
	}
	input.Reduction = &reduction.Result{
		Baseline: input.Result,
		Candidates: []reduction.CandidateResult{{
			Candidate: reduction.Candidate{Attempts: 2, Concurrency: 2},
			Trials:    &interruptedTrials,
		}},
	}

	var output bytes.Buffer
	if err := report.WriteText(&output, input); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	assertContains(t, text,
		"Problem · Interrupted candidate · Trial 1 · ERRORED",
		`"candidate unavailable"`,
		"1 more errored trial not shown.",
	)
	if strings.Contains(text, "Problem · Baseline · Trial 2") {
		t.Fatalf("compact report preferred the baseline problem over the interrupted candidate:\n%s", text)
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
	if strings.Contains(verbose.String(), "Run with --verbose") {
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
	command := "  concurtest run --attempts 2 --concurrency 2 --no-reduce scenarios/inventory.yaml\n"
	if !strings.Contains(colored.String(), command) || strings.Contains(command, "\x1b[") {
		t.Fatalf("reproduction command is not plain and copyable:\n%q", colored.String())
	}
}

func TestWriteTextCompactPrefersSelectedReductionEvidence(t *testing.T) {
	t.Parallel()

	input := completedTextInput(engine.RunOutcomeViolated, -1)
	input.Scenario.Attempts = 4
	input.Scenario.Concurrency = 4
	input.Result.Requested = 10
	input.Result.Trials = distinctViolationTrials(10, "baseline")
	selected := completedTextInput(engine.RunOutcomeViolated, -1).Result
	selected.Requested = 10
	selected.Trials = distinctViolationTrials(10, "selected")
	input.Reduction = &reduction.Result{
		StartedAt: input.Result.StartedAt, CompletedAt: input.Result.CompletedAt.Add(time.Second), Baseline: input.Result,
		Candidates: []reduction.CandidateResult{{
			Candidate: reduction.Candidate{Attempts: 2, Concurrency: 2},
			Summary:   reduction.TrialSummary{Requested: 10, Completed: 10, Violated: 10},
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
		"Reduction\n  Status          REDUCED",
		"Attempts        2",
		"Concurrency     2",
		"Violations      10/10 trials",
		"Note            Smallest observed failure; a smaller one may still exist.",
		"Evidence\n  Smallest observed failure · Trial 1",
		`Response        "{\"source\":\"selected-1\"}"`,
		"concurtest run --attempts 2 --concurrency 2 --no-reduce scenarios/inventory.yaml",
	)
	if strings.Contains(text, "Baseline evidence") || strings.Index(text, "Reduction\n") >= strings.Index(text, "Evidence\n") {
		t.Fatalf("compact evidence was not placed after reduction:\n%s", text)
	}
	if strings.Contains(text, "baseline-1") || strings.Contains(text, "selected-2") || strings.Count(text, "Smallest observed failure · Trial") != 1 {
		t.Fatalf("compact report expanded more than the selected representative:\n%s", text)
	}

	var verbose bytes.Buffer
	if err := report.WriteTextWithOptions(&verbose, input, report.TextOptions{Verbose: true}); err != nil {
		t.Fatal(err)
	}
	assertContains(t, verbose.String(), "Candidate results", "Selected candidate evidence")
	if strings.Count(verbose.String(), "Violation · Trial") != 10 || strings.Count(verbose.String(), "· VIOLATED") != 10 {
		t.Fatalf("verbose report did not retain baseline and selected evidence:\n%s", verbose.String())
	}
}

func TestWriteTextCompactBoundsAttemptsAndResponseBodies(t *testing.T) {
	t.Parallel()

	input := completedTextInput(engine.RunOutcomeViolated, -1)
	input.Scenario.Attempts = 6
	template := input.Result.Trials[0].Run.History.Attempts[0]
	attempts := make([]engine.Attempt, 6)
	for index := range attempts {
		attempts[index] = template
		attempts[index].ID = index + 1
		attempts[index].Execution.Response.Body = []byte(strings.Repeat("x", 200))
	}
	input.Result.Trials[0].Run.History.Attempts = attempts

	var output bytes.Buffer
	if err := report.WriteText(&output, input); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	assertContains(t, text, "Attempt #4", "2 more attempts not shown.", "[truncated]")
	if strings.Contains(text, "Attempt #5") || strings.Contains(text, strings.Repeat("x", 161)) {
		t.Fatalf("compact evidence exceeded its attempt or response limit:\n%s", text)
	}
}

func TestWriteTextQuotesReproductionPathsForPOSIXShells(t *testing.T) {
	t.Parallel()

	tests := []struct{ path, want string }{
		{"scenario.yaml", "concurtest run --attempts 2 --concurrency 2 --no-reduce scenario.yaml"},
		{"path with spaces/scenario.yaml", "concurtest run --attempts 2 --concurrency 2 --no-reduce 'path with spaces/scenario.yaml'"},
		{"owner's scenario.yaml", `concurtest run --attempts 2 --concurrency 2 --no-reduce 'owner'"'"'s scenario.yaml'`},
		{"$(touch nope).yaml", "concurtest run --attempts 2 --concurrency 2 --no-reduce '$(touch nope).yaml'"},
		{"-scenario.yaml", "concurtest run --attempts 2 --concurrency 2 --no-reduce ./-scenario.yaml"},
		{"", "concurtest run --attempts 2 --concurrency 2 --no-reduce ''"},
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
		Invariant:   engine.Invariant{JSONIntegerMinimum: &engine.JSONIntegerMinimumInvariant{Name: "final stock must be non-negative", Path: []string{"stock"}, Minimum: 0}},
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

func distinctViolationTrials(count int, prefix string) []engine.TrialResult {
	trials := make([]engine.TrialResult, count)
	for index := range trials {
		trial := completedTextInput(engine.RunOutcomeViolated, -1).Result.Trials[0]
		trial.Number = index + 1
		trial.Run.History.Attempts[0].Execution.Response.Body = []byte(fmt.Sprintf(`{"source":"%s-%d"}`, prefix, index+1))
		trials[index] = trial
	}
	return trials
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
