package app_test

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

	"github.com/eumarumar/concurtest/internal/app"
)

func TestCLIUnexpectedOutcomesCannotPass(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, invariant, status string
		httpStatus, exit        int
		commit                  bool
	}{
		{"all unauthorized history", "maximum_successful_attempts: 1", "inconclusive", 401, 2, false},
		{"all server errors history", "maximum_successful_attempts: 1", "inconclusive", 500, 2, false},
		{"committed server errors history", "maximum_successful_attempts: 1", "inconclusive", 500, 2, true},
		{"all conflicts without useful work", "maximum_successful_attempts: 1", "inconclusive", 409, 2, false},
		{"unauthorized state cannot pass", "json_integer_path: [count]\n  maximum: 1", "inconclusive", 401, 2, false},
		{"committed server errors state violation", "json_integer_path: [count]\n  maximum: 1", "violated", 500, 1, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var effects, calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/reset":
					effects.Store(0)
					w.WriteHeader(http.StatusNoContent)
				case "/operation":
					calls.Add(1)
					if test.commit {
						effects.Add(1)
					}
					w.WriteHeader(test.httpStatus)
				case "/state":
					if _, err := fmt.Fprintf(w, `{"count":%d}`, effects.Load()); err != nil {
						t.Errorf("write state: %v", err)
					}
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			document := fmt.Sprintf(`version: 1
name: Unexpected outcomes
target: %s
request_timeout: 2s
setup: {method: POST, path: /reset}
operation: {name: Execute operation, method: POST, path: /operation}
execution: {attempts: 2, concurrency: 2, trials: 3, reduce: false}
observation: {method: GET, path: /state}
invariant:
  name: At most one effect
  %s
`, server.URL, test.invariant)
			var stdout, stderr bytes.Buffer
			code := app.Run(context.Background(), []string{"run", "--format", "json", writeScenarioFile(t, document)}, &stdout, &stderr)
			var result struct {
				Status string
				Trials []struct {
					Status   string
					Evidence struct {
						History struct {
							Attempts []struct{ Execution transportExecution }
						}
						Observation struct {
							Response struct {
								BodyExcerpt struct{ Data string } `json:"body_excerpt"`
							}
						}
					}
				}
			}
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatalf("decode report: %v; stdout=%s; stderr=%s", err, &stdout, &stderr)
			}
			t.Logf("exit=%d, status=%s, trials=%d, target calls=%d, final effects=%d", code, result.Status, len(result.Trials), calls.Load(), effects.Load())
			if code != test.exit || result.Status != test.status || stderr.Len() != 0 {
				t.Errorf("exit=%d status=%s, want %d/%s; stderr=%s", code, result.Status, test.exit, test.status, &stderr)
			}
			if len(result.Trials) != 3 || calls.Load() != 6 {
				t.Fatalf("trials=%d target calls=%d, want 3/6", len(result.Trials), calls.Load())
			}
			wantEffects := 0
			if test.commit {
				wantEffects = 2
			}
			for _, trial := range result.Trials {
				if trial.Status != test.status || len(trial.Evidence.History.Attempts) != 2 {
					t.Errorf("trial status=%s attempts=%d", trial.Status, len(trial.Evidence.History.Attempts))
				}
				for _, attempt := range trial.Evidence.History.Attempts {
					if attempt.Execution.Response.StatusCode != test.httpStatus || attempt.Execution.Error != nil {
						t.Errorf("HTTP outcome lost or confused with an exchange error: %+v", attempt.Execution)
					}
				}
				if got := trial.Evidence.Observation.Response.BodyExcerpt.Data; got != fmt.Sprintf(`{"count":%d}`, wantEffects) {
					t.Errorf("observed state=%q, want count %d", got, wantEffects)
				}
			}
		})
	}
}

func TestCLIOutcomePolicyControls(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, policy, status, finding        string
		codes                                []int
		exit, accepted, rejected, unexpected int
	}{
		{"expected conflict", "rejected_status_codes: [409]", "passed", "1 accepted · 1 rejected", []int{201, 409}, 0, 1, 1, 0},
		{"expected bad request", "rejected_status_codes: [400]", "passed", "1 accepted · 1 rejected", []int{201, 400}, 0, 1, 1, 0},
		{"all expected rejections lack coverage", "rejected_status_codes: [409]", "inconclusive", "Required at least 1 accepted response; observed 0", []int{409, 409}, 2, 0, 2, 0},
		{"explicit rejection only", "rejected_status_codes: [409], minimum_accepted_attempts: 0", "passed", "0 accepted · 2 rejected", []int{409, 409}, 0, 0, 2, 0},
		{"zero minimum cannot excuse unauthorized", "minimum_accepted_attempts: 0", "inconclusive", "2 unexpected", []int{401, 401}, 2, 0, 0, 2},
		{"mixed accepted and server error", "rejected_status_codes: [409]", "inconclusive", "1 unexpected", []int{201, 500}, 2, 1, 0, 1},
		{"custom acceptance excludes empty responses", "accepted_status_codes: [201], rejected_status_codes: [204]", "inconclusive", "Required at least 1 accepted response; observed 0", []int{204, 204}, 2, 0, 2, 0},
	} {
		for _, format := range []string{"text", "verbose", "json"} {
			t.Run(test.name+"/"+format, func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/reset" {
						calls.Store(0)
						w.WriteHeader(http.StatusNoContent)
						return
					}
					index := calls.Add(1) - 1
					if int(index) >= len(test.codes) {
						t.Errorf("unexpected operation request %d", index)
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					w.WriteHeader(test.codes[index])
				}))
				t.Cleanup(server.Close)
				document := fmt.Sprintf(`version: 1
name: Outcome policy controls
target: %s
request_timeout: 2s
setup: {method: POST, path: /reset}
operation: {name: Operation, method: POST, path: /operation}
execution: {attempts: 2, concurrency: 2, trials: 2}
outcomes: {%s}
invariant: {name: At most one matching response, maximum_successful_attempts: 1, successful_status_codes: [201]}
`, server.URL, test.policy)
				args := []string{"run", "--format", format}
				if format == "verbose" {
					args = []string{"run", "--verbose"}
				}
				var stdout, stderr bytes.Buffer
				code := app.Run(context.Background(), append(args, writeScenarioFile(t, document)), &stdout, &stderr)
				if code != test.exit || stderr.Len() != 0 {
					t.Fatalf("exit=%d, want %d; stdout=%s; stderr=%s", code, test.exit, &stdout, &stderr)
				}
				if format != "json" {
					if !strings.Contains(stdout.String(), strings.ToUpper(test.status)) {
						t.Fatalf("missing verdict: %s", &stdout)
					}
					if (format == "verbose" || test.status == "inconclusive") && !strings.Contains(stdout.String(), test.finding) {
						t.Fatalf("missing finding %q: %s", test.finding, &stdout)
					}
					if test.status == "inconclusive" && strings.Contains(stdout.String(), "0/2 attempts failed or did not start") {
						t.Fatalf("misleading explanation of outcome failure: %s", &stdout)
					}
					return
				}
				var result struct {
					Status string
					Trials []struct {
						Status   string
						Evidence struct {
							Outcomes struct {
								Accepted        []int `json:"accepted_attempt_ids"`
								Rejected        []int `json:"rejected_attempt_ids"`
								Unexpected      []int `json:"unexpected_attempt_ids"`
								Unknown         []int `json:"unknown_attempt_ids"`
								RequirementsMet bool  `json:"requirements_met"`
							} `json:"outcome_evaluation"`
						}
					}
				}
				if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if result.Status != test.status || len(result.Trials) != 2 {
					t.Fatalf("unexpected verdict: %s", &stdout)
				}
				for _, trial := range result.Trials {
					o := trial.Evidence.Outcomes
					if trial.Status != test.status || len(o.Accepted) != test.accepted || len(o.Rejected) != test.rejected ||
						len(o.Unexpected) != test.unexpected || len(o.Unknown) != 0 || o.RequirementsMet != (test.status == "passed") {
						t.Fatalf("unexpected outcomes: %+v", o)
					}
				}
			})
		}
	}
}
