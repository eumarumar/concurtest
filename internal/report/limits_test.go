package report_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eumarumar/concurtest/internal/engine"
	"github.com/eumarumar/concurtest/internal/reduction"
	"github.com/eumarumar/concurtest/internal/report"
)

func TestResourceLimitReportsPreservePartialEvidenceAndUsage(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte(strings.Repeat("x", 1000))); err != nil {
			t.Errorf("write body: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	scenario := engine.Scenario{Operation: engine.Operation{Name: "Execute", Request: engine.HTTPRequest{Method: http.MethodPost, URL: server.URL}}, Attempts: 2, Concurrency: 1, Invariant: engine.Invariant{MaximumSuccessfulAttempts: &engine.MaximumSuccessfulAttemptsInvariant{Name: "No matching responses", Maximum: 0}}, Limits: engine.Limits{MaxRetainedBytes: 1500}}
	result, runErr := engine.RunTrials(context.Background(), server.Client(), scenario, 3)
	if runErr == nil {
		t.Fatal("fixture did not exhaust budget")
	}
	input := report.Input{ScenarioPath: "scenario.yaml", Scenario: scenario, Result: result, RunError: runErr}
	var output bytes.Buffer
	if err := report.WriteJSON(&output, input); err != nil {
		t.Fatal(err)
	}
	validateReportJSON(t, output.Bytes())
	var document map[string]any
	decodeJSON(t, output.Bytes(), &document)
	u := document["resources"].(map[string]any)
	if document["status"] != "errored" || u["retained_bytes"].(float64) > 1500 || u["exhausted"] != "max_retained_bytes" || document["summary"].(map[string]any)["violated"] != float64(1) {
		t.Fatalf("budget error, bytes, or known violation lost: %s", &output)
	}
	attempts := document["trials"].([]any)[0].(map[string]any)["evidence"].(map[string]any)["history"].(map[string]any)["attempts"].([]any)
	last := attempts[1].(map[string]any)["execution"].(map[string]any)
	response := last["response"].(map[string]any)
	if response["evidence_truncated"] != true || response["body_excerpt"].(map[string]any)["truncated"] != true || last["error"] == nil {
		t.Fatal("partial response presented as complete")
	}
	for _, verbose := range []bool{false, true} {
		output.Reset()
		if err := report.WriteTextWithOptions(&output, input, report.TextOptions{Verbose: verbose}); err != nil {
			t.Fatal(err)
		}
		assertContains(t, output.String(), "Resources", "Limit reached   max_retained_bytes", "Earlier violations remain recorded")
	}
	schema := compiledReportSchema(t)
	for _, field := range []string{"attempt_slots", "exchanges", "retained_bytes", "elapsed_ns"} {
		original := u[field]
		u[field] = -1
		if err := schema.Validate(document); err == nil {
			t.Fatalf("schema accepted negative %s", field)
		}
		u[field] = original
	}
}

func TestReportUsageIncludesReductionCandidates(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/state" {
			if _, err := w.Write([]byte(`{"stock":-1}`)); err != nil {
				t.Errorf("write state: %v", err)
			}
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(server.Close)
	s := engine.Scenario{
		Setup:     []engine.SetupStep{{Request: engine.HTTPRequest{Method: http.MethodPost, URL: server.URL + "/setup"}}},
		Operation: engine.Operation{Name: "Execute", Request: engine.HTTPRequest{Method: http.MethodPost, URL: server.URL + "/operation"}},
		Attempts:  4, Concurrency: 4, Observation: &engine.HTTPRequest{Method: http.MethodGet, URL: server.URL + "/state"},
		Invariant: engine.Invariant{JSONInteger: &engine.JSONIntegerInvariant{Name: "Nonnegative", Path: []string{"stock"}, Minimum: new(int64(0))}},
		Limits:    engine.Limits{MaxExchanges: 20},
	}
	result, runErr := reduction.Reduce(context.Background(), server.Client(), s, 3)
	if runErr == nil || result.Baseline.Resources.Exchanges != 18 {
		t.Fatalf("fixture did not exhaust during candidate: %+v %v", result.Resources, runErr)
	}
	input := report.Input{ScenarioPath: "scenario.yaml", Scenario: s, Result: result.Baseline, Reduction: &result, RunError: runErr}
	var output bytes.Buffer
	if err := report.WriteJSON(&output, input); err != nil {
		t.Fatal(err)
	}
	validateReportJSON(t, output.Bytes())
	var document map[string]any
	decodeJSON(t, output.Bytes(), &document)
	if document["resources"].(map[string]any)["exchanges"] != float64(20) {
		t.Fatal("report counted baseline usage only")
	}
	output.Reset()
	if err := report.WriteText(&output, input); err != nil {
		t.Fatal(err)
	}
	assertContains(t, output.String(), "RESOURCE LIMIT REACHED", "max_exchanges")
}

func TestReportBudgetRejectsOversizedEncodingBeforeWriting(t *testing.T) {
	t.Parallel()
	input := completedTextInput(engine.RunOutcomeViolated, -1)
	input.Scenario.Operation.Name = strings.Repeat("large", 20000)
	input.Scenario.Limits.MaxReportBytes = 4096
	for _, format := range []string{"json", "verbose"} {
		var output bytes.Buffer
		var err error
		if format == "json" {
			err = report.WriteJSON(&output, input)
		} else {
			err = report.WriteTextWithOptions(&output, input, report.TextOptions{Verbose: true})
		}
		if err == nil || output.Len() != 0 {
			t.Fatalf("%s allocated/emitted an oversized report: bytes=%d error=%v", format, output.Len(), err)
		}
	}
	input.Scenario.Operation.Name = "operation"
	input.Scenario.Limits.MaxReportBytes = 1
	var output bytes.Buffer
	if err := report.WriteText(&output, input); err == nil || output.Len() > 1 {
		t.Fatal("compact report bypassed byte limit")
	}
}

func TestReportBudgetHandlesEscapingWithinTheBound(t *testing.T) {
	t.Parallel()
	input := completedTextInput(engine.RunOutcomeViolated, -1)
	input.Scenario.Limits.MaxReportBytes = 128 << 10
	input.Result.Trials[0].Run.Observation.Response.Body = []byte(strings.Repeat("\x00\x01\n\"\\", 100))
	var output bytes.Buffer
	if err := report.WriteJSON(&output, input); err != nil {
		t.Fatal(err)
	}
	if int64(output.Len()) > input.Scenario.Limits.MaxReportBytes {
		t.Fatal("JSON escaping escaped report budget")
	}
	validateReportJSON(t, output.Bytes())
}
