package report_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eumarumar/concurtest/internal/engine"
	"github.com/eumarumar/concurtest/internal/reduction"
	"github.com/eumarumar/concurtest/internal/report"
)

func TestReductionReportsPreserveDirectionAndMatchingEvidence(t *testing.T) {
	t.Parallel()
	var trial, stock atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/setup":
			trial.Add(1)
			stock.Store(0)
		case "/operation":
			// Baseline and second candidate have an above-equality majority,
			// but their first violation is below. The first candidate drifts.
			current := trial.Load()
			if current == 2 || current == 3 || current == 8 || current == 9 {
				stock.Store(2)
			}
		case "/state":
			if _, err := fmt.Fprintf(w, `{"stock":%d}`, stock.Load()); err != nil {
				t.Errorf("write state: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	scenario := engine.Scenario{
		Setup: []engine.SetupStep{{Request: engine.HTTPRequest{Method: http.MethodPost, URL: server.URL + "/setup"}}},
		Precondition: &engine.Precondition{
			Request:   engine.HTTPRequest{Method: http.MethodGet, URL: server.URL + "/state"},
			Assertion: engine.ResponseAssertion{JSONInteger: &engine.JSONIntegerInvariant{Name: "empty", Path: []string{"stock"}, Equals: new(int64(0))}},
		},
		Operation: engine.Operation{Name: "execute", Request: engine.HTTPRequest{Method: http.MethodPost, URL: server.URL + "/operation"}},
		Attempts:  4, Concurrency: 4,
		Observation: &engine.HTTPRequest{Method: http.MethodGet, URL: server.URL + "/state"},
		Invariant:   engine.Invariant{JSONInteger: &engine.JSONIntegerInvariant{Name: "one effect", Path: []string{"stock"}, Equals: new(int64(1))}},
	}
	result, err := reduction.Reduce(context.Background(), server.Client(), scenario, 3)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != reduction.StatusReduced || len(result.Candidates) != 2 || trial.Load() != 9 {
		t.Fatalf("unexpected search: %+v", result)
	}
	input := report.Input{ScenarioPath: "scenario.yaml", Scenario: scenario, Result: result.Baseline, Reduction: &result}
	var output bytes.Buffer
	if err := report.WriteJSON(&output, input); err != nil {
		t.Fatal(err)
	}
	validateReportJSON(t, output.Bytes())
	var document map[string]any
	decodeJSON(t, output.Bytes(), &document)
	search := document["reduction"].(map[string]any)
	candidates := search["candidates"].([]any)
	rejected, accepted := candidates[0].(map[string]any), candidates[1].(map[string]any)
	if document["status"] != "violated" || search["failure_direction"] != "above" ||
		rejected["matching_violations"] != float64(0) || rejected["accepted"] != false || rejected["trials"] != nil ||
		accepted["matching_violations"] != float64(2) || accepted["accepted"] != true || len(accepted["trials"].([]any)) != 3 {
		t.Fatalf("direction or evidence lost: %s", &output)
	}
	for _, verbose := range []bool{false, true} {
		output.Reset()
		if err := report.WriteTextWithOptions(&output, input, report.TextOptions{Verbose: verbose}); err != nil {
			t.Fatal(err)
		}
		assertContains(t, output.String(), "Direction       above", "Matching        2/3 trials in the baseline direction", "does not prove the same root cause", "--attempts 3 --concurrency 2 --no-reduce")
		if verbose {
			assertContains(t, output.String(), "0 matching violations", "2 matching violations")
		} else {
			assertContains(t, output.String(), "Smallest observed failure · Trial 2")
			if strings.Contains(output.String(), "Smallest observed failure · Trial 1") {
				t.Fatal("compact evidence selected the opposite-direction minority")
			}
		}
	}

	schema := compiledReportSchema(t)
	for _, invalid := range []any{"sideways", "", 1} {
		search["failure_direction"] = invalid
		if err := schema.Validate(document); err == nil {
			t.Fatalf("schema accepted invalid direction %v", invalid)
		}
	}
	search["failure_direction"] = "above"
	for _, invalid := range []any{-1, 101, 1.5, nil, "2"} {
		accepted["matching_violations"] = invalid
		if err := schema.Validate(document); err == nil {
			t.Fatalf("schema accepted invalid matching count %v", invalid)
		}
	}
}
