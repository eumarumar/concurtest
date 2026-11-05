package report_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eumarumar/concurtest/internal/engine"
	"github.com/eumarumar/concurtest/internal/report"
)

func TestResponseCountReportsDeclareTheirMeasurement(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, statuses, observed string
		filter                   []int
		responses                []int
		matching                 int
		evaluated                bool
	}{
		{"default replay responses", "HTTP 200–299 (default)", "2 matching HTTP responses", nil, []int{200, 200}, 2, true},
		{"explicit response filter", "HTTP 201 Created", "1 matching HTTP response", []int{201}, []int{201, 500}, 1, true},
		{"matching rejection statuses", "HTTP 409 Conflict", "2 matching HTTP responses", []int{409}, []int{409, 409}, 2, true},
		{"zero matching responses", "HTTP 200–299 (default)", "0 matching HTTP responses", nil, []int{500, 500}, 0, true},
		{"not evaluated", "HTTP 200–299 (default)", "Not evaluated", nil, nil, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			definition := engine.MaximumSuccessfulAttemptsInvariant{Name: "Declared response limit", Maximum: 1, SuccessfulStatusCodes: test.filter}
			scenario := engine.Scenario{
				Setup:     []engine.SetupStep{{Request: engine.HTTPRequest{Method: http.MethodPost, URL: "http://example.test/setup"}}},
				Operation: engine.Operation{Name: "repeat", Request: engine.HTTPRequest{Method: http.MethodPost, URL: "http://example.test/operation"}},
				Attempts:  2, Concurrency: 2,
				Invariant: engine.Invariant{MaximumSuccessfulAttempts: &definition},
				Outcomes:  engine.OutcomePolicy{RejectedStatusCodes: []int{500}, MinimumAcceptedAttempts: new(0)},
			}
			client := &http.Client{Transport: &responseCountTransport{statuses: test.responses, setupOK: test.evaluated}}
			result, err := engine.RunTrials(context.Background(), client, scenario, 1)
			if err != nil {
				t.Fatal(err)
			}
			input := report.Input{ScenarioPath: "scenario.yaml", Scenario: scenario, Result: result}
			for _, verbose := range []bool{false, true} {
				var output bytes.Buffer
				if err := report.WriteTextWithOptions(&output, input, report.TextOptions{Verbose: verbose}); err != nil {
					t.Fatal(err)
				}
				assertContains(t, output.String(), "At most 1 matching HTTP response", "Match statuses  "+test.statuses,
					"Observed        "+test.observed, "Counts responses, including replays; verify stored effects separately.")
				if strings.Contains(output.String(), "successful attempt") || strings.Contains(output.String(), "Success statuses") {
					t.Fatalf("ambiguous success wording remains: %s", &output)
				}
			}
			var output bytes.Buffer
			if err := report.WriteJSON(&output, input); err != nil {
				t.Fatal(err)
			}
			validateReportJSON(t, output.Bytes())
			var document map[string]any
			decodeJSON(t, output.Bytes(), &document)
			definitionJSON := document["scenario"].(map[string]any)["invariant"].(map[string]any)
			if definitionJSON["measurement"] != "matching_http_response_count" {
				t.Fatalf("response-count definition lacks its measurement: %#v", definitionJSON)
			}
			evidence := document["trials"].([]any)[0].(map[string]any)["evidence"].(map[string]any)
			if !test.evaluated {
				if evidence["invariant_evaluation"] != nil {
					t.Fatal("unevaluated response count was fabricated")
				}
				return
			}
			evaluationJSON := evidence["invariant_evaluation"].(map[string]any)
			if evaluationJSON["measurement"] != "matching_http_response_count" || len(evaluationJSON["successful_attempt_ids"].([]any)) != test.matching {
				t.Fatalf("response-count evaluation lost meaning or IDs: %#v", evaluationJSON)
			}
			schema := compiledReportSchema(t)
			for _, subject := range []map[string]any{definitionJSON, evaluationJSON} {
				for _, invalid := range []any{"logical_effect_count", "", nil, 1} {
					subject["measurement"] = invalid
					if err := schema.Validate(document); err == nil {
						t.Fatalf("schema accepted invalid measurement %v", invalid)
					}
				}
				delete(subject, "measurement")
				if err := schema.Validate(document); err == nil {
					t.Fatal("schema accepted a response count with no measurement")
				}
				subject["measurement"] = "matching_http_response_count"
			}
		})
	}
}

// Use the actual runner without opening listeners. Concurrent requests consume
// a fixed response multiset; matching counts do not depend on worker order.
type responseCountTransport struct {
	statuses []int
	setupOK  bool
	next     atomic.Int32
}

func (t *responseCountTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	status := http.StatusNoContent
	if request.URL.Path == "/setup" {
		if !t.setupOK {
			status = http.StatusServiceUnavailable
		}
	} else {
		status = t.statuses[int(t.next.Add(1))-1]
	}
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"operation_id":"same"}`))}, nil
}
