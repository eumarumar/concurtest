package report_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eumarumar/concurtest/internal/engine"
	"github.com/eumarumar/concurtest/internal/report"
)

func TestPreparationReportsPreserveCheckedAndUnverifiedValues(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, setupBody, initialBody string
		want                         any
		state                        bool
	}{
		{"false acknowledgement", `{"ok":false}`, `{"count":0}`, false, false},
		{"unreadable acknowledgement", `{"ok":null}`, `{"count":0}`, nil, false},
		{"wrong initial state", `{"ok":true}`, `{"count":2}`, float64(2), true},
		{"unreadable initial state", `{"ok":true}`, `{"count":"0"}`, nil, true},
		{"truncated initial state", `{"ok":true}`, `{"count":0}` + strings.Repeat(" ", engine.MaxHTTPBodyBytes), nil, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body := test.setupBody
				if r.URL.Path == "/initial" {
					body = test.initialBody
				} else if r.URL.Path != "/setup" {
					t.Errorf("later stage started: %s", r.URL.Path)
				}
				// The bounded reader may close an oversized response while the
				// server is still writing it. That disconnect is expected here.
				if _, err := fmt.Fprint(w, body); err != nil && len(body) <= engine.MaxHTTPBodyBytes {
					t.Errorf("write response: %v", err)
				}
			}))
			t.Cleanup(server.Close)
			scenario := engine.Scenario{
				Setup:        []engine.SetupStep{{Name: "Reset", Request: engine.HTTPRequest{Method: http.MethodPost, URL: server.URL + "/setup", Header: http.Header{"Authorization": {"setup-header-secret"}}, Body: []byte("setup-body-secret")}, Assertion: &engine.ResponseAssertion{JSONBoolean: &engine.JSONBooleanAssertion{Name: "Reset acknowledged", Path: []string{"ok"}, Equals: true}}}},
				Precondition: &engine.Precondition{Request: engine.HTTPRequest{Method: http.MethodGet, URL: server.URL + "/initial", Header: http.Header{"Authorization": {"initial-header-secret"}}, Body: []byte("initial-body-secret")}, Assertion: engine.ResponseAssertion{JSONInteger: &engine.JSONIntegerInvariant{Name: "Initial counter", Path: []string{"count"}, Equals: new(int64(0))}}},
				Operation:    engine.Operation{Name: "Operation", Request: engine.HTTPRequest{Method: http.MethodPost, URL: server.URL + "/operation"}},
				Attempts:     1, Concurrency: 1,
				Invariant: engine.Invariant{MaximumSuccessfulAttempts: &engine.MaximumSuccessfulAttemptsInvariant{Name: "At most one", Maximum: 1}},
			}
			result, err := engine.RunTrials(context.Background(), server.Client(), scenario, 1)
			if err != nil || result.Status != engine.TrialStatusErrored {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			input := report.Input{ScenarioPath: "scenario.yaml", Scenario: scenario, Result: result}
			var output bytes.Buffer
			if err := report.WriteJSON(&output, input); err != nil {
				t.Fatal(err)
			}
			validateReportJSON(t, output.Bytes())
			var document map[string]any
			decodeJSON(t, output.Bytes(), &document)
			definition := document["scenario"].(map[string]any)
			if definition["precondition"] == nil || definition["setup"].([]any)[0].(map[string]any)["assertion"] == nil {
				t.Fatal("checked definitions omitted")
			}
			trial := document["trials"].([]any)[0].(map[string]any)
			if trial["error"].(map[string]any)["code"] != "precondition_failed" {
				t.Fatalf("incorrect error code: %#v", trial["error"])
			}
			evidence := trial["evidence"].(map[string]any)
			if evidence["invariant_evaluation"] != nil || len(evidence["history"].(map[string]any)["attempts"].([]any)) != 0 {
				t.Fatal("failed preparation was evaluated as operation evidence")
			}
			checked := evidence["setup"].([]any)[0].(map[string]any)
			if test.state {
				checked = evidence["precondition"].(map[string]any)
			} else if evidence["precondition"] != nil {
				t.Fatal("precondition attempted after setup failure")
			}
			assertion := checked["assertion"].(map[string]any)
			if assertion["observed"] != test.want || assertion["satisfied"] != false {
				t.Fatalf("assertion lost its value or failure: %#v", assertion)
			}
			if len(test.initialBody) > engine.MaxHTTPBodyBytes {
				excerpt := checked["execution"].(map[string]any)["response"].(map[string]any)["body_excerpt"].(map[string]any)
				if excerpt["retained_bytes"] != float64(512) || excerpt["truncated"] != true {
					t.Fatalf("precondition excerpt was not bounded: %#v", excerpt)
				}
			}
			for _, secret := range []string{"setup-header-secret", "setup-body-secret", "initial-header-secret", "initial-body-secret"} {
				if strings.Contains(output.String(), secret) {
					t.Fatalf("request secret exposed: %s", secret)
				}
			}
			for _, verbose := range []bool{false, true} {
				output.Reset()
				if err := report.WriteTextWithOptions(&output, input, report.TextOptions{Verbose: verbose}); err != nil {
					t.Fatal(err)
				}
				wantStatus := "FAILED"
				if test.want == nil {
					wantStatus = "NOT VERIFIED"
				}
				if !strings.Contains(output.String(), "Check result    "+wantStatus) {
					t.Fatalf("check status omitted: %s", &output)
				}
				if test.state && !strings.Contains(output.String(), "Initial counter") {
					t.Fatalf("initial-state check omitted: %s", &output)
				}
			}
		})
	}
}

func TestSchemaRejectsInvalidAssertionEvidence(t *testing.T) {
	t.Parallel()
	input := completedTextInput(engine.RunOutcomePassed, 0)
	var output bytes.Buffer
	if err := report.WriteJSON(&output, input); err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	decodeJSON(t, output.Bytes(), &document)
	setup := document["trials"].([]any)[0].(map[string]any)["evidence"].(map[string]any)["setup"].([]any)[0].(map[string]any)
	schema := compiledReportSchema(t)
	for _, value := range []string{
		`{"type":"json_boolean","observed":1,"satisfied":false}`,
		`{"type":"json_integer","observed":false,"satisfied":false}`,
		`{"type":"json_integer","observed":null,"satisfied":true}`,
		`{"type":"json_boolean","observed":true}`,
		`{"type":"json_boolean","observed":false,"satisfied":false,"unknown":true}`,
	} {
		var invalid any
		if err := json.Unmarshal([]byte(value), &invalid); err != nil {
			t.Fatal(err)
		}
		setup["assertion"] = invalid
		if err := schema.Validate(document); err == nil {
			t.Fatalf("schema accepted invalid assertion: %s", value)
		}
	}
}
