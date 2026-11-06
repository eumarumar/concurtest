package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/eumarumar/concurtest/internal/app"
)

// The target owns one count for the fixed test key. Replays can repeat the
// original status and operation ID without adding another stored effect.
func TestCLIResponseCountsAndPerKeyEffects(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, invariant, outcomes             string
		replayStatus, effects, exit, matching int
		duplicate, noEffect                   bool
	}{
		{"replayed response history", "maximum_successful_attempts: 1", "", 201, 1, 1, 2, false, false},
		{"replayed response state", "json_integer_path: [effect_count]\n  equals: 1", "outcomes: {accepted_status_codes: [201], minimum_accepted_attempts: 2}\n", 201, 1, 0, 0, false, false},
		{"replayed response change", "json_integer_path: [effect_count]\n  change: 1", "", 201, 1, 0, 0, false, false},
		{"empty replay default filter", "maximum_successful_attempts: 1", "", 204, 1, 1, 2, false, false},
		{"empty replay explicit filter", "maximum_successful_attempts: 1\n  successful_status_codes: [201]", "outcomes: {accepted_status_codes: [201, 204]}\n", 204, 1, 0, 1, false, false},
		{"duplicate effects same response ID", "json_integer_path: [effect_count]\n  equals: 1", "", 201, 2, 1, 0, true, false},
		{"successful responses without effects", "json_integer_path: [effect_count]\n  equals: 1", "", 201, 0, 1, 0, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var mu sync.Mutex
			count, operations, setups := 0, 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/reset":
					if r.URL.Query().Get("key") != "replay-control" {
						t.Error("reset lost its test key")
					}
					mu.Lock()
					count = 0
					setups++
					mu.Unlock()
					w.WriteHeader(http.StatusNoContent)
				case "/operation":
					if r.Header.Get("Idempotency-Key") != "replay-control" {
						t.Error("operation lost its idempotency key")
					}
					mu.Lock()
					first := count == 0
					if !test.noEffect && (first || test.duplicate) {
						count++
					}
					operations++
					mu.Unlock()
					status := test.replayStatus
					if first {
						status = http.StatusCreated
					}
					w.WriteHeader(status)
					if status != http.StatusNoContent {
						if _, err := w.Write([]byte(`{"operation_id":"order-1"}`)); err != nil {
							t.Errorf("write operation: %v", err)
						}
					}
				case "/effects":
					if r.URL.Query().Get("key") != "replay-control" {
						t.Error("observation lost its test key")
					}
					mu.Lock()
					value := count
					mu.Unlock()
					if _, err := fmt.Fprintf(w, `{"effect_count":%d}`, value); err != nil {
						t.Errorf("write effects: %v", err)
					}
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			document := fmt.Sprintf(`version: 1
name: Idempotent replay control
target: %s
request_timeout: 2s
setup: {method: POST, path: '/reset?key=replay-control'}
precondition:
  request: {method: GET, path: '/effects?key=replay-control'}
  assert: {name: Initially empty, json_integer_path: [effect_count], equals: 0}
operation:
  name: Repeat the same operation
  method: POST
  path: /operation
  headers: {Idempotency-Key: replay-control}
execution: {attempts: 2, concurrency: 2, trials: 3}
%sobservation: {method: GET, path: '/effects?key=replay-control'}
invariant:
  name: Declared replay property
  %s
`, server.URL, test.outcomes, test.invariant)
			var stdout, stderr bytes.Buffer
			code := app.Run(context.Background(), []string{"run", "--format", "json", writeScenarioFile(t, document)}, &stdout, &stderr)
			var result struct {
				Status string
				Trials []struct {
					Evidence struct {
						History struct {
							Attempts []struct {
								Execution struct {
									Response struct {
										Status int                   `json:"status_code"`
										Body   struct{ Data string } `json:"body_excerpt"`
									}
								}
							}
						}
						Outcomes struct {
							Accepted []int `json:"accepted_attempt_ids"`
							Met      bool  `json:"requirements_met"`
						} `json:"outcome_evaluation"`
						Observation struct {
							Response struct {
								Body struct{ Data string } `json:"body_excerpt"`
							}
						}
						Evaluation struct {
							Type        string
							Measurement string
							IDs         []int `json:"successful_attempt_ids"`
							Observed    int
							Change      *int
						} `json:"invariant_evaluation"`
					}
				}
			}
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatalf("decode report: %v; stdout=%s stderr=%s", err, &stdout, &stderr)
			}
			if code != test.exit || stderr.Len() != 0 || len(result.Trials) != 3 {
				t.Fatalf("exit=%d want=%d; stdout=%s stderr=%s", code, test.exit, &stdout, &stderr)
			}
			wantStatus := "passed"
			if test.exit == 1 {
				wantStatus = "violated"
			}
			if result.Status != wantStatus {
				t.Fatalf("status=%s want=%s", result.Status, wantStatus)
			}
			for _, trial := range result.Trials {
				evidence := trial.Evidence
				if !evidence.Outcomes.Met || len(evidence.Outcomes.Accepted) != 2 || len(evidence.History.Attempts) != 2 {
					t.Fatalf("replay did not meet its HTTP outcome contract: %+v", evidence)
				}
				created := 0
				for _, attempt := range evidence.History.Attempts {
					response := attempt.Execution.Response
					if response.Status == http.StatusCreated {
						created++
						if response.Body.Data != `{"operation_id":"order-1"}` {
							t.Fatalf("replayed response identity was lost: %+v", response)
						}
					} else if response.Status != http.StatusNoContent || response.Body.Data != "" {
						t.Fatalf("unexpected replay response: %+v", response)
					}
				}
				wantCreated := 2
				if test.replayStatus == http.StatusNoContent {
					wantCreated = 1
				}
				if created != wantCreated {
					t.Fatalf("created response count=%d want=%d", created, wantCreated)
				}
				if evidence.Observation.Response.Body.Data != fmt.Sprintf(`{"effect_count":%d}`, test.effects) {
					t.Fatalf("incorrect per-key count: %+v", evidence)
				}
				if evidence.Evaluation.Type == "maximum_successful_attempts" {
					if len(evidence.Evaluation.IDs) != test.matching || evidence.Evaluation.Measurement != "matching_http_response_count" {
						t.Fatalf("response count changed: %+v", evidence.Evaluation)
					}
				} else if evidence.Evaluation.Observed != test.effects || (strings.Contains(test.invariant, "change:") && (evidence.Evaluation.Change == nil || *evidence.Evaluation.Change != 1)) {
					t.Fatalf("incorrect scalar evidence: %+v", evidence.Evaluation)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			t.Logf("exit=%d status=%s trials=%d operations=%d stored effects per trial=%d invariant=%s", code, result.Status, len(result.Trials), operations, count, result.Trials[0].Evidence.Evaluation.Type)
			if operations != 6 || setups != 3 || count != test.effects {
				t.Fatalf("incorrect target activity: operations=%d setups=%d count=%d", operations, setups, count)
			}
		})
	}
}
