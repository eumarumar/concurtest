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

// Work is held in the target until after the runner returns. This reproduces
// delayed effects without relying on a race between a sleep and the CLI.
func TestCLIImmediateObservationDoesNotDrainQueuedEffects(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, timeout, status string
		trials, exit          int
		timeoutOperation      bool
	}{
		{"one trial", "2s", "passed", 1, 0, false},
		{"ten trials", "2s", "passed", 10, 0, false},
		{"request timeout control", "100ms", "inconclusive", 1, 2, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var mu sync.Mutex
			count, pending, resetsWhilePending := 0, 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				switch r.URL.Path {
				case "/reset":
					if pending != 0 {
						resetsWhilePending++
					}
					count = 0 // Intentionally does not drain queued work.
					mu.Unlock()
					w.WriteHeader(http.StatusNoContent)
				case "/operation":
					pending++
					mu.Unlock()
					if test.timeoutOperation {
						<-r.Context().Done()
						return
					}
					w.WriteHeader(http.StatusAccepted)
				case "/state":
					value := count
					mu.Unlock()
					if _, err := fmt.Fprintf(w, `{"count":%d}`, value); err != nil {
						t.Errorf("write state: %v", err)
					}
				default:
					mu.Unlock()
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			document := fmt.Sprintf(`version: 1
name: Deferred effects
target: %s
request_timeout: %s
setup: {method: POST, path: /reset}
precondition:
  request: {method: GET, path: /state}
  assert: {name: Initially empty, json_integer_path: [count], equals: 0}
operation: {name: Queue work, method: POST, path: /operation}
execution: {attempts: 2, concurrency: 2, trials: %d}
observation: {method: GET, path: /state}
invariant: {name: At most one effect, json_integer_path: [count], maximum: 1}
`, server.URL, test.timeout, test.trials)
			var stdout, stderr bytes.Buffer
			code := app.Run(context.Background(), []string{"run", "--format", "json", writeScenarioFile(t, document)}, &stdout, &stderr)
			var result struct {
				Status string
				Trials []struct {
					Status   string
					Evidence struct {
						Evaluation struct{ Observed int } `json:"invariant_evaluation"`
					}
				}
			}
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatalf("decode report: %v; stdout=%s; stderr=%s", err, &stdout, &stderr)
			}
			mu.Lock()
			before, queued, overlappingResets := count, pending, resetsWhilePending
			count += pending // Release the target's deferred work after the run.
			pending = 0
			after := count
			mu.Unlock()
			t.Logf("exit=%d status=%s trials=%d observed=%d queued=%d later effects=%d resets with pending work=%d", code, result.Status, len(result.Trials), before, queued, after, overlappingResets)
			if code != test.exit || result.Status != test.status || len(result.Trials) != test.trials || before != 0 || queued != 2*test.trials || after != 2*test.trials || overlappingResets != test.trials-1 || stderr.Len() != 0 {
				t.Fatalf("unexpected reproduction: stdout=%s stderr=%s", &stdout, &stderr)
			}
			for _, trial := range result.Trials {
				if trial.Status != test.status || trial.Evidence.Evaluation.Observed != 0 {
					t.Fatalf("unexpected observed evidence: %+v", trial)
				}
			}
		})
	}
}

func TestCLIWaitConditionsProtectObservationAndLaterResets(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, failed, status string
		history              bool
		exit, wantTrials     int
	}{
		{"deferred state effects", "", "violated", false, 1, 3},
		{"unverified readiness", "readiness", "errored", false, 2, 1},
		{"unverified completion", "completion", "errored", false, 2, 1},
		{"history violation survives unverified completion", "completion", "violated", true, 1, 1},
		{"history completion without final observer", "", "violated", true, 1, 3},
	} {
		for _, format := range []string{"text", "verbose", "json"} {
			t.Run(test.name+"/"+format, func(t *testing.T) {
				var mu sync.Mutex
				pending, count, readyPolls, completionPolls, resets, operations, observations := 1, 0, 0, 0, 0, 0, 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					defer mu.Unlock()
					body := ""
					switch r.URL.Path {
					case "/ready":
						readyPolls++
						if readyPolls >= 2 && test.failed != "readiness" {
							pending = 0
						}
						body = fmt.Sprintf(`{"pending":%d}`, pending)
					case "/reset":
						if pending != 0 {
							t.Error("reset overlapped pending work")
						}
						resets++
						count, completionPolls = 0, 0
						w.WriteHeader(http.StatusNoContent)
						return
					case "/initial":
						body = fmt.Sprintf(`{"count":%d}`, count)
					case "/operation":
						operations++
						pending++
						w.WriteHeader(http.StatusAccepted)
						return
					case "/complete":
						completionPolls++
						// Release queued effects only after the runner has observed
						// them pending, making the schedule deterministic.
						if completionPolls >= 2 && test.failed != "completion" {
							count += pending
							pending = 0
						}
						body = fmt.Sprintf(`{"pending":%d}`, pending)
					case "/state":
						observations++
						if pending != 0 {
							t.Error("final state observed before completion")
						}
						body = fmt.Sprintf(`{"count":%d}`, count)
					default:
						t.Errorf("unexpected request %s", r.URL.Path)
						w.WriteHeader(404)
						return
					}
					if _, err := fmt.Fprint(w, body); err != nil {
						t.Errorf("write fixture: %v", err)
					}
				}))
				t.Cleanup(server.Close)
				observer, invariant := "observation: {method: GET, path: /state}\n", "json_integer_path: [count], maximum: 1"
				if test.history {
					observer, invariant = "", "maximum_successful_attempts: 1"
				}
				document := fmt.Sprintf(`version: 1
name: Wait for deferred effects
target: %s
request_timeout: 2s
readiness:
  request: {method: GET, path: /ready}
  assert: {name: No pending work before reset, json_integer_path: [pending], equals: 0}
  timeout: 1s
  interval: 1ms
  max_polls: 3
setup: {method: POST, path: /reset}
precondition:
  request: {method: GET, path: /initial}
  assert: {name: Initially empty, json_integer_path: [count], equals: 0}
operation: {name: Queue work, method: POST, path: /operation}
execution: {attempts: 2, concurrency: 2, trials: 3}
completion:
  request: {method: GET, path: /complete}
  assert: {name: All accepted work finished, json_integer_path: [pending], equals: 0}
  timeout: 1s
  interval: 1ms
  max_polls: 3
%sinvariant: {name: At most one effect, %s}
`, server.URL, observer, invariant)
				args := []string{"run", "--format", format}
				if format == "verbose" {
					args = []string{"run", "--verbose"}
				}
				var stdout, stderr bytes.Buffer
				code := app.Run(context.Background(), append(args, writeScenarioFile(t, document)), &stdout, &stderr)
				if code != test.exit || stderr.Len() != 0 {
					t.Fatalf("exit=%d want %d stdout=%s stderr=%s", code, test.exit, &stdout, &stderr)
				}
				mu.Lock()
				gotResets, gotOperations, gotObservations := resets, operations, observations
				mu.Unlock()
				wantResets, wantOperations, wantObservations := test.wantTrials, 2*test.wantTrials, test.wantTrials
				if test.history || test.failed != "" {
					wantObservations = 0
				}
				if test.failed == "readiness" {
					wantResets, wantOperations = 0, 0
				}
				if gotResets != wantResets || gotOperations != wantOperations || gotObservations != wantObservations {
					t.Fatalf("resets=%d operations=%d observations=%d, want %d/%d/%d", gotResets, gotOperations, gotObservations, wantResets, wantOperations, wantObservations)
				}
				if format != "json" {
					wantLabel := strings.ToUpper(test.status)
					if test.status == "errored" {
						wantLabel = "ERROR"
					}
					waitLabel := "Readiness · "
					if test.failed == "completion" {
						waitLabel = "Completion · "
					}
					if !strings.Contains(stdout.String(), wantLabel) || !strings.Contains(stdout.String(), waitLabel) {
						t.Fatalf("wait evidence missing: %s", &stdout)
					}
					if test.failed != "" && !strings.Contains(stdout.String(), "NOT VERIFIED") {
						t.Fatalf("unverified condition hidden: %s", &stdout)
					}
					continueCheck := "no further trials were started"
					if test.failed != "" && !strings.Contains(stdout.String(), continueCheck) {
						t.Fatalf("stop reason missing: %s", &stdout)
					}
					return
				}
				var result struct {
					Status string
					Trials []struct {
						Status   string
						Error    *struct{ Code string }
						Evidence struct {
							Readiness, Completion *cliWaitEvidence
							Evaluation            json.RawMessage `json:"invariant_evaluation"`
						}
					}
				}
				if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if result.Status != test.status || len(result.Trials) != test.wantTrials {
					t.Fatalf("unexpected result: %s", &stdout)
				}
				for _, trial := range result.Trials {
					if trial.Status != test.status || trial.Evidence.Readiness == nil {
						t.Fatalf("missing readiness: %+v", trial)
					}
					if test.failed != "" {
						if trial.Error == nil || trial.Error.Code != test.failed+"_failed" {
							t.Fatalf("incorrect wait error: %+v", trial)
						}
						if !test.history && string(trial.Evidence.Evaluation) != "null" {
							t.Fatal("state invariant evaluated before completion")
						}
					} else if !trial.Evidence.Readiness.Satisfied || trial.Evidence.Completion == nil || !trial.Evidence.Completion.Satisfied || len(trial.Evidence.Completion.Polls) != 2 {
						t.Fatalf("completion evidence missing: %+v", trial)
					}
				}
			})
		}
	}
}

type cliWaitEvidence struct {
	Satisfied bool
	Error     *struct{ Code string }
	Polls     []struct {
		Number    int
		Assertion *preparationAssertionEvidence
		Execution transportExecution
	}
}
