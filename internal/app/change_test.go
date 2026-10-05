package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eumarumar/concurtest/internal/app"
)

func TestRunChangeReportsPassViolationAndObservationErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, baseline, final string
		code                  int
		delta                 int64
	}{
		{"decrement once", `{"stock":1000}`, `{"stock":999}`, 0, -1},
		{"duplicate decrement", `{"stock":1000}`, `{"stock":998}`, 1, -2},
		{"unchanged", `{"stock":1000}`, `{"stock":1000}`, 1, 0},
		{"failed baseline", `{}`, `{"stock":999}`, 2, 0},
		{"failed final", `{"stock":1000}`, `{"stock":null}`, 2, 0},
	}
	for _, test := range tests {
		for _, format := range []string{"text", "json"} {
			t.Run(test.name+"/"+format, func(t *testing.T) {
				var observations, operations atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/state" {
						body := test.final
						if observations.Add(1) == 1 {
							body = test.baseline
						}
						if _, err := w.Write([]byte(body)); err != nil {
							t.Errorf("write observation: %v", err)
						}
					} else {
						operations.Add(1)
						w.WriteHeader(http.StatusNoContent)
					}
				}))
				t.Cleanup(server.Close)
				document := strings.Replace(scenarioYAML(server.URL, "1s", 2, 2, false), "minimum: 0", "change: -1", 1)
				path := writeScenarioFile(t, document)
				var stdout, stderr bytes.Buffer
				if code := app.Run(context.Background(), []string{"run", "--format", format, path}, &stdout, &stderr); code != test.code || stderr.Len() != 0 {
					t.Fatalf("exit %d want %d; stdout %s; stderr %s", code, test.code, &stdout, &stderr)
				}
				if test.name == "failed baseline" {
					if operations.Load() != 0 || observations.Load() != 1 {
						t.Fatal("CLI sent operations after an invalid baseline")
					}
				} else if observations.Load() != 2 || operations.Load() != 2 {
					t.Fatal("CLI did not run both observations around operations")
				}
				if format == "text" {
					assertOutputContains(t, stdout.String(), `Change in $["stock"] == -1 (final - baseline)`)
					if test.code != 2 {
						assertOutputContains(t, stdout.String(), `Baseline        $["stock"] = 1000`)
					}
					return
				}
				var result struct {
					SchemaVersion string `json:"schema_version"`
					Status        string `json:"status"`
					Trials        []struct {
						Evidence struct {
							Baseline   json.RawMessage `json:"baseline_observation"`
							Evaluation *struct {
								Baseline, Observed, Change int64
								Violated                   bool
							} `json:"invariant_evaluation"`
						} `json:"evidence"`
					} `json:"trials"`
				}
				if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if result.SchemaVersion != "1.0.0" || len(result.Trials) != 1 {
					t.Fatalf("unexpected report: %s", &stdout)
				}
				evidence := result.Trials[0].Evidence
				if len(evidence.Baseline) == 0 || string(evidence.Baseline) == "null" {
					t.Fatal("JSON report lost baseline execution")
				}
				if test.code == 2 {
					if result.Status != "errored" || evidence.Evaluation != nil {
						t.Fatal("observation error reported as an evaluation")
					}
				} else if evidence.Evaluation == nil || evidence.Evaluation.Baseline != 1000 || evidence.Evaluation.Change != test.delta || evidence.Evaluation.Observed != 1000+test.delta || evidence.Evaluation.Violated != (test.code == 1) {
					t.Fatalf("incorrect change evidence: %s", &stdout)
				}
			})
		}
	}
}
