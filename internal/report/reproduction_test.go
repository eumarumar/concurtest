package report_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/eumarumar/concurtest/internal/engine"
	"github.com/eumarumar/concurtest/internal/reduction"
	"github.com/eumarumar/concurtest/internal/report"
)

func TestReportsReproduceEffectiveExecution(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name             string
		concurrency      int
		reductionEnabled bool
		skipped          bool
		selected         bool
		want             string
	}{
		{name: "sequential baseline", concurrency: 1, want: "--attempts 4 --concurrency 1 --no-reduce"},
		{name: "concurrent baseline", concurrency: 4, want: "--attempts 4 --concurrency 4 --no-reduce"},
		{name: "reduction enabled", concurrency: 4, reductionEnabled: true, want: "--attempts 4 --concurrency 4"},
		{name: "reduction skipped", concurrency: 4, reductionEnabled: true, skipped: true, want: "--attempts 4 --concurrency 4"},
		{name: "selected reduction", concurrency: 4, reductionEnabled: true, selected: true, want: "--attempts 2 --concurrency 2 --no-reduce"},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := completedTextInput(engine.RunOutcomeViolated, -1)
			input.Scenario.Attempts = 4
			input.Scenario.Concurrency = test.concurrency
			input.ReductionEnabled = test.reductionEnabled
			if test.skipped {
				input.Reduction = &reduction.Result{Baseline: input.Result, Status: reduction.StatusSkipped}
			}
			if test.selected {
				selected := completedTextInput(engine.RunOutcomeViolated, -1).Result
				input.Reduction = &reduction.Result{
					Baseline: input.Result, Status: reduction.StatusReduced,
					Selected: reduction.Candidate{Attempts: 2, Concurrency: 2}, SelectedTrials: &selected,
					Candidates: []reduction.CandidateResult{{
						Candidate: reduction.Candidate{Attempts: 2, Concurrency: 2},
						Accepted:  true, Trials: &selected,
					}},
				}
			}

			var textOutput, jsonOutput bytes.Buffer
			if err := report.WriteText(&textOutput, input); err != nil {
				t.Fatal(err)
			}
			wantText := "Reproduce\n  concurtest run " + test.want + " " + input.ScenarioPath + "\n"
			assertContains(t, textOutput.String(), wantText)
			if err := report.WriteJSON(&jsonOutput, input); err != nil {
				t.Fatal(err)
			}
			validateReportJSON(t, jsonOutput.Bytes())
			var document map[string]any
			decodeJSON(t, jsonOutput.Bytes(), &document)
			arguments := document["reproduction"].(map[string]any)["arguments"].([]any)
			wantJSON := "concurtest run --format json " + test.want + " " + input.ScenarioPath
			if got := strings.Join(anyStrings(arguments), " "); got != wantJSON {
				t.Fatalf("reproduction arguments = %q, want %q", got, wantJSON)
			}
		})
	}
}
