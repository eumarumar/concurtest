package report_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eumarumar/concurtest/internal/engine"
	"github.com/eumarumar/concurtest/internal/reduction"
	"github.com/eumarumar/concurtest/internal/report"
)

func TestWaitReportsPreservePollsAndStoppedReduction(t *testing.T) {
	t.Parallel()
	for _, stopped := range []bool{false, true} {
		t.Run(fmt.Sprint(stopped), func(t *testing.T) {
			var polls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/operation" {
					w.WriteHeader(http.StatusAccepted)
					return
				}
				if r.Header.Get("X-Check") != "request-header-secret" {
					t.Error("poll request header was not sent")
				}
				pending := 1
				if polls.Add(1) >= 2 && !stopped {
					pending = 0
				}
				if _, err := fmt.Fprintf(w, `{"padding":"%s","pending":%d}`, strings.Repeat("x", 700), pending); err != nil {
					t.Errorf("write poll: %v", err)
				}
			}))
			t.Cleanup(server.Close)
			scenario := engine.Scenario{Operation: engine.Operation{Name: "Queue work", Request: engine.HTTPRequest{Method: http.MethodPost, URL: server.URL + "/operation"}}, Attempts: 2, Concurrency: 2, Invariant: engine.Invariant{MaximumSuccessfulAttempts: &engine.MaximumSuccessfulAttemptsInvariant{Name: "At most one", Maximum: 1}}}
			scenario.Completion = &engine.WaitCondition{Request: engine.HTTPRequest{Method: http.MethodGet, URL: server.URL + "/pending", Header: http.Header{"X-Check": {"request-header-secret"}}, Body: []byte("request-body-secret")}, Assertion: engine.ResponseAssertion{JSONInteger: &engine.JSONIntegerInvariant{Name: "Pending work finished", Path: []string{"pending"}, Equals: new(int64(0))}}, Timeout: time.Second, Interval: time.Millisecond, MaxPolls: 3}
			result, err := engine.RunTrials(context.Background(), server.Client(), scenario, 3)
			if err != nil {
				t.Fatal(err)
			}
			input := report.Input{ScenarioPath: "scenario.yaml", Scenario: scenario, Result: result}
			if stopped {
				input.Reduction = &reduction.Result{Status: reduction.StatusStopped, Baseline: result}
			}
			var output bytes.Buffer
			if err := report.WriteJSON(&output, input); err != nil {
				t.Fatal(err)
			}
			validateReportJSON(t, output.Bytes())
			var document map[string]any
			decodeJSON(t, output.Bytes(), &document)
			if document["status"] != "violated" {
				t.Fatalf("known response-count violation hidden: %s", &output)
			}
			if stopped && document["reduction"].(map[string]any)["status"] != "stopped" {
				t.Fatal("stopped reduction was not identified")
			}
			trial := document["trials"].([]any)[0].(map[string]any)
			wait := trial["evidence"].(map[string]any)["completion"].(map[string]any)
			retained := wait["polls"].([]any)
			wantPolls := 2
			if stopped {
				wantPolls = 3
			}
			if len(retained) != wantPolls || wait["satisfied"] != !stopped {
				t.Fatalf("wait evidence lost: %#v", wait)
			}
			for index, raw := range retained {
				poll := raw.(map[string]any)
				if poll["number"] != float64(index+1) {
					t.Fatal("poll order lost")
				}
				excerpt := poll["execution"].(map[string]any)["response"].(map[string]any)["body_excerpt"].(map[string]any)
				if excerpt["retained_bytes"] != float64(engine.MaxWaitBodyBytes) || excerpt["truncated"] != true {
					t.Fatal("wait body evidence is not bounded")
				}
			}
			if strings.Contains(output.String(), "request-header-secret") || strings.Contains(output.String(), "request-body-secret") {
				t.Fatal("wait request secrets exposed")
			}
			for _, verbose := range []bool{false, true} {
				output.Reset()
				if err := report.WriteTextWithOptions(&output, input, report.TextOptions{Verbose: verbose}); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(output.String(), "Completion · ") || !strings.Contains(output.String(), "Pending work finished") {
					t.Fatalf("wait evidence omitted: %s", &output)
				}
				if stopped && !strings.Contains(output.String(), "NOT VERIFIED") {
					t.Fatalf("failed wait hidden: %s", &output)
				}
			}
		})
	}
}

func TestSchemaRejectsInvalidWaitEvidence(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	if err := report.WriteJSON(&output, completedTextInput(engine.RunOutcomePassed, 0)); err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	decodeJSON(t, output.Bytes(), &document)
	evidence := document["trials"].([]any)[0].(map[string]any)["evidence"].(map[string]any)
	schema := compiledReportSchema(t)
	for _, value := range []string{
		`{"timing":null,"polls":[],"satisfied":"true","error":null}`,
		`{"timing":null,"polls":[],"satisfied":false,"error":null,"unknown":true}`,
		`{"timing":null,"polls":[{"number":0,"execution":null,"assertion":null}],"satisfied":false,"error":null}`,
	} {
		var invalid any
		if err := json.Unmarshal([]byte(value), &invalid); err != nil {
			t.Fatal(err)
		}
		evidence["completion"] = invalid
		if err := schema.Validate(document); err == nil {
			t.Fatalf("schema accepted invalid wait evidence: %s", value)
		}
	}
}

func TestStoppedCandidateReportsKeepKnownViolationAndWaitFailure(t *testing.T) {
	t.Parallel()
	var resets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/reset":
			resets.Add(1)
			w.WriteHeader(http.StatusNoContent)
		case "/operation":
			w.WriteHeader(http.StatusAccepted)
		case "/pending":
			pending := 0
			if resets.Load() > 3 {
				pending = 1
			}
			if _, err := fmt.Fprintf(w, `{"pending":%d}`, pending); err != nil {
				t.Errorf("write completion state: %v", err)
			}
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(server.Close)
	scenario := engine.Scenario{
		Setup:     []engine.SetupStep{{Request: engine.HTTPRequest{Method: http.MethodPost, URL: server.URL + "/reset"}}},
		Operation: engine.Operation{Name: "Queue work", Request: engine.HTTPRequest{Method: http.MethodPost, URL: server.URL + "/operation"}},
		Attempts:  3, Concurrency: 2,
		Invariant:  engine.Invariant{MaximumSuccessfulAttempts: &engine.MaximumSuccessfulAttemptsInvariant{Name: "At most one response", Maximum: 1}},
		Completion: &engine.WaitCondition{Request: engine.HTTPRequest{Method: http.MethodGet, URL: server.URL + "/pending"}, Assertion: engine.ResponseAssertion{JSONInteger: &engine.JSONIntegerInvariant{Name: "All work finished", Path: []string{"pending"}, Equals: new(int64(0))}}, Timeout: time.Second, Interval: time.Millisecond, MaxPolls: 2},
	}
	reduced, err := reduction.Reduce(context.Background(), server.Client(), scenario, 3)
	if err != nil || reduced.Status != reduction.StatusStopped || resets.Load() != 4 {
		t.Fatalf("reduction=%+v resets=%d error=%v", reduced, resets.Load(), err)
	}
	input := report.Input{ScenarioPath: "scenario.yaml", Scenario: scenario, Result: reduced.Baseline, Reduction: &reduced}
	var output bytes.Buffer
	if err := report.WriteJSON(&output, input); err != nil {
		t.Fatal(err)
	}
	validateReportJSON(t, output.Bytes())
	var document map[string]any
	decodeJSON(t, output.Bytes(), &document)
	if document["status"] != "violated" {
		t.Fatal("baseline violation hidden by stopped reduction")
	}
	r := document["reduction"].(map[string]any)
	if r["status"] != "stopped" || r["selection"] != nil {
		t.Fatal("stopped search claimed a selected reproduction")
	}
	candidate := r["candidates"].([]any)[0].(map[string]any)
	if candidate["accepted"] != false || candidate["error"].(map[string]any)["code"] != "completion_failed" || len(candidate["trials"].([]any)) != 1 {
		t.Fatalf("candidate failure lost: %#v", candidate)
	}
	for _, verbose := range []bool{false, true} {
		output.Reset()
		if err := report.WriteTextWithOptions(&output, input, report.TextOptions{Verbose: verbose}); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"VIOLATED", "STOPPED", "Completion · NOT VERIFIED", "no further candidates"} {
			if !strings.Contains(output.String(), want) {
				t.Fatalf("missing %q: %s", want, &output)
			}
		}
	}
}
