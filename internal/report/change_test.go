package report_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/eumarumar/concurtest/internal/engine"
	"github.com/eumarumar/concurtest/internal/failure"
	"github.com/eumarumar/concurtest/internal/report"
)

func TestChangeReportsIncludeBothObservationsAndValues(t *testing.T) {
	t.Parallel()
	for _, delta := range []int64{-1, 0} {
		input := completedTextInput(engine.RunOutcomeViolated, 998)
		definition := input.Scenario.Invariant.JSONInteger
		definition.Minimum = nil
		definition.Change = new(int64(-1))
		run := &input.Result.Trials[0].Run
		run.Observation.Response.Body = []byte(fmt.Sprintf(`{"stock":%d}`, 1000+delta))
		baseline := *run.Observation
		baseline.Response = &engine.HTTPResponse{StatusCode: 200, Body: []byte(`{"stock":1000}`)}
		run.BaselineObservation = &baseline
		run.Evaluation.Violated = delta != -1
		run.Evaluation.JSONInteger = &engine.JSONIntegerEvaluation{Invariant: *definition, Observed: 1000 + delta, Baseline: new(int64(1000)), Change: new(delta), Violated: delta != -1}
		if delta == -1 {
			run.Outcome = engine.RunOutcomePassed
			input.Result.Status = engine.TrialStatusPassed
			input.Result.Trials[0].Status = engine.TrialStatusPassed
		}
		var output bytes.Buffer
		if err := report.WriteJSON(&output, input); err != nil {
			t.Fatal(err)
		}
		validateReportJSON(t, output.Bytes())
		var document map[string]any
		decodeJSON(t, output.Bytes(), &document)
		invariant := document["scenario"].(map[string]any)["invariant"].(map[string]any)
		if invariant["change"] != float64(-1) {
			t.Fatalf("missing change definition: %v", invariant)
		}
		evidence := document["trials"].([]any)[0].(map[string]any)["evidence"].(map[string]any)
		if evidence["baseline_observation"] == nil || evidence["observation"] == nil {
			t.Fatal("missing observation evidence")
		}
		evaluation := evidence["invariant_evaluation"].(map[string]any)
		if evaluation["baseline"] != float64(1000) || evaluation["change"] != float64(delta) || evaluation["observed"] != float64(1000+delta) {
			t.Fatalf("incomplete evaluation: %v", evaluation)
		}
		for _, verbose := range []bool{false, true} {
			output.Reset()
			if err := report.WriteTextWithOptions(&output, input, report.TextOptions{Verbose: verbose}); err != nil {
				t.Fatal(err)
			}
			assertContains(t, output.String(), `Change in $["stock"] == -1 (final - baseline)`, `Baseline        $["stock"] = 1000`)
			if verbose || delta != -1 {
				assertContains(t, output.String(), "Baseline observation", `{\"stock\":1000}`)
			}
			if strings.Contains(output.String(), "request-secret") || strings.Contains(output.String(), "response-secret") {
				t.Fatal("report leaked headers")
			}
		}
		// A failed final observation must retain the baseline without an evaluation.
		input.Result.Status = engine.TrialStatusErrored
		input.Result.Trials[0].Status = engine.TrialStatusErrored
		input.Result.Trials[0].Err = failure.New(failure.CodeObservationFailed, "final observation failed")
		run.Outcome = ""
		run.Observation = nil
		run.Evaluation = nil
		output.Reset()
		if err := report.WriteJSON(&output, input); err != nil {
			t.Fatal(err)
		}
		validateReportJSON(t, output.Bytes())
		var partial map[string]any
		if err := json.Unmarshal(output.Bytes(), &partial); err != nil {
			t.Fatal(err)
		}
		partialEvidence := partial["trials"].([]any)[0].(map[string]any)["evidence"].(map[string]any)
		if partialEvidence["baseline_observation"] == nil || partialEvidence["invariant_evaluation"] != nil {
			t.Fatal("partial baseline evidence lost")
		}
	}
}
