package report_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eumarumar/concurtest/internal/engine"
	"github.com/eumarumar/concurtest/internal/report"
)

func TestReportPreservesOutcomePolicyAndEvidence(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	scenario := engine.Scenario{
		Operation: engine.Operation{Name: "operation", Request: engine.HTTPRequest{Method: http.MethodPost, URL: server.URL}},
		Attempts:  2, Concurrency: 2,
		Invariant: engine.Invariant{MaximumSuccessfulAttempts: &engine.MaximumSuccessfulAttemptsInvariant{
			Name: "one accepted response", Maximum: 1, SuccessfulStatusCodes: []int{201},
		}},
		Outcomes: engine.OutcomePolicy{RejectedStatusCodes: []int{409}},
	}
	result, err := engine.RunTrials(context.Background(), server.Client(), scenario, 1)
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
	policy := document["scenario"].(map[string]any)["outcomes"].(map[string]any)
	if policy["minimum_accepted_attempts"] != float64(1) || policy["accepted_status_codes"].([]any)[0] != float64(201) || policy["rejected_status_codes"].([]any)[0] != float64(409) {
		t.Fatalf("policy not preserved: %#v", policy)
	}
	outcomes := document["trials"].([]any)[0].(map[string]any)["evidence"].(map[string]any)["outcome_evaluation"].(map[string]any)
	if outcomes["requirements_met"] != false || len(outcomes["unexpected_attempt_ids"].([]any)) != 2 || len(outcomes["unknown_attempt_ids"].([]any)) != 0 {
		t.Fatalf("outcomes not preserved: %#v", outcomes)
	}
	for _, verbose := range []bool{false, true} {
		output.Reset()
		if err := report.WriteTextWithOptions(&output, input, report.TextOptions{Verbose: verbose}); err != nil {
			t.Fatal(err)
		}
		for _, finding := range []string{"2 unexpected", "HTTP 500", "Required at least 1 accepted response; observed 0", "whether effects committed"} {
			if !strings.Contains(output.String(), finding) {
				t.Fatalf("missing %q: %s", finding, &output)
			}
		}
	}
}

func TestReportSchemaRejectsInvalidOutcomeEvidence(t *testing.T) {
	t.Parallel()
	input := completedTextInput(engine.RunOutcomePassed, 0)
	var output bytes.Buffer
	if err := report.WriteJSON(&output, input); err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	decodeJSON(t, output.Bytes(), &document)
	evidence := document["trials"].([]any)[0].(map[string]any)["evidence"].(map[string]any)
	schema := compiledReportSchema(t)
	for _, test := range []struct {
		field string
		value any
	}{
		{"accepted_attempt_ids", []int{0}},
		{"accepted_attempt_ids", []int{1, 1}},
		{"unknown_attempt_ids", nil},
		{"minimum_accepted_attempts", -1},
		{"requirements_met", "true"},
		{"unknown_field", true},
	} {
		var invalid map[string]any
		if err := json.Unmarshal([]byte(`{"accepted_attempt_ids":[],"rejected_attempt_ids":[],"unexpected_attempt_ids":[],"unknown_attempt_ids":[],"minimum_accepted_attempts":1,"requirements_met":false}`), &invalid); err != nil {
			t.Fatal(err)
		}
		invalid[test.field] = test.value
		evidence["outcome_evaluation"] = invalid
		if err := schema.Validate(document); err == nil {
			t.Fatalf("schema accepted invalid outcome evidence: %+v", invalid)
		}
	}
}
