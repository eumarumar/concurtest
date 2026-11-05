package report_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eumarumar/concurtest/internal/engine"
	"github.com/eumarumar/concurtest/internal/report"
)

func TestConjunctionReportsRetainEachResultAndOverallVerdict(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, body, status, first, second, third string
		complete, stopped                        bool
	}{
		{"all pass", `{"balance":3,"ledger":0}`, "passed", "passed", "passed", "passed", true, false},
		{"ledger violates", `{"balance":3,"ledger":-7}`, "violated", "passed", "violated", "passed", true, false},
		{"missing ledger", `{"balance":3}`, "errored", "passed", "errored", "passed", false, false},
		{"violation with missing ledger", `{"balance":-1}`, "violated", "violated", "errored", "passed", false, false},
		{"completion fails", `{"balance":3,"ledger":0}`, "violated", "not_evaluated", "not_evaluated", "violated", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body string
				switch r.URL.Path {
				case "/state":
					body = test.body
				case "/done":
					body = `{"pending":1}`
				default:
					w.WriteHeader(http.StatusCreated)
				}
				if _, err := w.Write([]byte(body)); err != nil {
					t.Errorf("write response: %v", err)
				}
			}))
			t.Cleanup(server.Close)
			limit := 2
			if test.stopped {
				limit = 1
			}
			scenario := engine.Scenario{
				Operation: engine.Operation{Name: "Transfer", Request: engine.HTTPRequest{Method: http.MethodPost, URL: server.URL + "/operation"}},
				Attempts:  2, Concurrency: 2,
				Observation: &engine.HTTPRequest{Method: http.MethodGet, URL: server.URL + "/state"},
				Invariant: engine.Invariant{All: []engine.Invariant{
					{JSONInteger: &engine.JSONIntegerInvariant{Name: "Balance", Path: []string{"balance"}, Minimum: new(int64(0))}},
					{JSONInteger: &engine.JSONIntegerInvariant{Name: "Ledger", Path: []string{"ledger"}, Equals: new(int64(0))}},
					{MaximumSuccessfulAttempts: &engine.MaximumSuccessfulAttemptsInvariant{Name: "Replies", Maximum: limit}},
				}},
			}
			if test.stopped {
				scenario.Completion = &engine.WaitCondition{Request: engine.HTTPRequest{Method: http.MethodGet, URL: server.URL + "/done"}, Assertion: engine.ResponseAssertion{JSONInteger: &engine.JSONIntegerInvariant{Name: "Settled", Path: []string{"pending"}, Equals: new(int64(0))}}, Timeout: time.Second, Interval: time.Millisecond, MaxPolls: 1}
			}
			result, err := engine.RunTrials(context.Background(), server.Client(), scenario, 3)
			if err != nil {
				t.Fatal(err)
			}
			input := report.Input{ScenarioPath: "scenario.yaml", Scenario: scenario, Result: result}
			var output bytes.Buffer
			if err := report.WriteJSON(&output, input); err != nil {
				t.Fatal(err)
			}
			validateReportJSON(t, output.Bytes())
			var document map[string]any
			decodeJSON(t, output.Bytes(), &document)
			if document["status"] != test.status {
				t.Fatalf("wrong aggregate verdict: %s", &output)
			}
			definition := document["scenario"].(map[string]any)["invariant"].(map[string]any)
			if definition["type"] != "all" || len(definition["checks"].([]any)) != 3 {
				t.Fatal("group definition lost")
			}
			evidence := document["trials"].([]any)[0].(map[string]any)["evidence"].(map[string]any)
			evaluation := evidence["invariant_evaluation"].(map[string]any)
			if evaluation["complete"] != test.complete || evaluation["violated"] != (test.status == "violated") {
				t.Fatalf("wrong completeness or violation: %#v", evaluation)
			}
			checks := evaluation["checks"].([]any)
			for index, want := range []string{test.first, test.second, test.third} {
				check := checks[index].(map[string]any)
				if check["status"] != want || check["name"] != []string{"Balance", "Ledger", "Replies"}[index] {
					t.Fatalf("check result lost: %#v", check)
				}
				if want == "errored" || want == "not_evaluated" {
					if check["evaluation"] != nil || (check["error"] != nil) != (want == "errored") {
						t.Fatalf("unknown check fabricated evidence: %#v", check)
					}
				}
			}
			if test.stopped && (len(result.Trials) != 1 || evidence["observation"] != nil) {
				t.Fatal("completion failure allowed final observation or further trials")
			}
			for _, verbose := range []bool{false, true} {
				output.Reset()
				if err := report.WriteTextWithOptions(&output, input, report.TextOptions{Verbose: verbose}); err != nil {
					t.Fatal(err)
				}
				assertContains(t, output.String(), "All 3 checks must hold", "Balance", "Ledger", "Replies")
				if test.second == "errored" {
					assertContains(t, output.String(), "ERRORED", `evaluate check \"Ledger\"`)
				}
				if !verbose && test.status == "violated" && !test.complete {
					assertContains(t, output.String(), "Additional error")
				}
				if test.stopped {
					assertContains(t, output.String(), "NOT EVALUATED", "Completion")
				}
			}
			schema := compiledReportSchema(t)
			for _, raw := range checks {
				member := raw.(map[string]any)
				original := member["status"]
				member["status"] = "unknown"
				if err := schema.Validate(document); err == nil {
					t.Fatal("schema accepted unknown member status")
				}
				if original == "passed" {
					member["status"] = "violated"
				} else {
					member["status"] = "passed"
				}
				if err := schema.Validate(document); err == nil {
					t.Fatal("schema accepted a status inconsistent with member evidence")
				}
				member["status"] = original
			}
			for _, field := range []string{"complete", "violated"} {
				original := evaluation[field]
				evaluation[field] = !original.(bool)
				if err := schema.Validate(document); err == nil {
					t.Fatalf("schema accepted inconsistent %s", field)
				}
				evaluation[field] = original
			}
			originalChecks := definition["checks"]
			oversized := make([]any, engine.MaxInvariantChecks+1)
			for index := range oversized {
				oversized[index] = originalChecks.([]any)[0]
			}
			for _, invalid := range []any{nil, []any{}, []any{originalChecks.([]any)[0]}, oversized, []any{map[string]any{"type": "all", "checks": []any{}}, originalChecks.([]any)[0]}} {
				definition["checks"] = invalid
				if err := schema.Validate(document); err == nil {
					t.Fatalf("schema accepted invalid flat group: %#v", invalid)
				}
			}
		})
	}
}

func TestConjunctionHistoryViolationRetainsContextObservationError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/context" {
			w.WriteHeader(http.StatusInternalServerError)
		} else {
			w.WriteHeader(http.StatusCreated)
		}
	}))
	t.Cleanup(server.Close)
	scenario := engine.Scenario{
		Operation: engine.Operation{Name: "Execute", Request: engine.HTTPRequest{Method: http.MethodPost, URL: server.URL + "/operation"}},
		Attempts:  2, Concurrency: 2, Observation: &engine.HTTPRequest{Method: http.MethodGet, URL: server.URL + "/context"},
		Invariant: engine.Invariant{All: []engine.Invariant{
			{MaximumSuccessfulAttempts: &engine.MaximumSuccessfulAttemptsInvariant{Name: "At most one reply", Maximum: 1}},
			{MaximumSuccessfulAttempts: &engine.MaximumSuccessfulAttemptsInvariant{Name: "At most two replies", Maximum: 2}},
		}},
	}
	result, err := engine.RunTrials(context.Background(), server.Client(), scenario, 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != engine.TrialStatusViolated || result.Trials[0].Err == nil || !result.Trials[0].Run.Evaluation.Complete() {
		t.Fatalf("context failure or known violation was lost: %+v", result)
	}
	input := report.Input{ScenarioPath: "scenario.yaml", Scenario: scenario, Result: result}
	var output bytes.Buffer
	if err := report.WriteText(&output, input); err != nil {
		t.Fatal(err)
	}
	assertContains(t, output.String(), "VIOLATED", "Additional error", "HTTP status 500")
	output.Reset()
	if err := report.WriteJSON(&output, input); err != nil {
		t.Fatal(err)
	}
	validateReportJSON(t, output.Bytes())
}
