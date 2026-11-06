package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eumarumar/concurtest/internal/app"
)

func TestCLITransportEvidenceAndVerdicts(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, path, invariant, status string
		redirect                      bool
		exit, wantOperations          int
	}{
		{"retry history", "/purchase", "maximum_successful_attempts: 1", "inconclusive", false, 2, 2},
		{"retry state cannot pass", "/purchase", "json_integer_path: [count]\n  minimum: 0", "inconclusive", false, 2, 2},
		{"retry state violation remains visible", "/purchase", "json_integer_path: [count]\n  maximum: 1", "violated", false, 1, 2},
		{"retry setup stops operations", "/prepare", "maximum_successful_attempts: 1", "errored", false, 2, 0},
		{"retry observation stops evaluation", "/state", "json_integer_path: [count]\n  minimum: 0", "errored", false, 2, 1},
		{"redirect operation retains 302", "/purchase", "maximum_successful_attempts: 0", "inconclusive", true, 2, 0},
		{"redirect setup stops operations", "/prepare", "maximum_successful_attempts: 1", "errored", true, 2, 0},
		{"redirect observation stops evaluation", "/state", "json_integer_path: [count]\n  minimum: 0", "errored", true, 2, 1},
	} {
		for _, format := range []string{"text", "verbose", "json"} {
			t.Run(test.name+"/"+format, func(t *testing.T) {
				var operations, affectedCalls, destinationCalls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if _, err := io.Copy(io.Discard, r.Body); err != nil {
						t.Errorf("read request: %v", err)
						return
					}
					if r.URL.Path == "/login" {
						destinationCalls.Add(1)
						w.WriteHeader(http.StatusOK)
						return
					}
					if r.URL.Path == test.path && test.redirect {
						affectedCalls.Add(1)
						w.Header().Set("Location", "/login")
						w.WriteHeader(http.StatusFound)
						return
					}
					if r.URL.Path == "/purchase" {
						operations.Add(1)
					}
					if r.URL.Path == test.path {
						if r.Header.Get("Idempotency-Key") != "test-key" {
							t.Error("idempotency key was changed or removed")
						}
						if affectedCalls.Add(1) == 1 {
							conn, _, err := w.(http.Hijacker).Hijack()
							if err != nil {
								t.Errorf("hijack: %v", err)
								return
							}
							if err := conn.Close(); err != nil {
								t.Errorf("close connection: %v", err)
							}
							return
						}
					}
					if r.URL.Path == "/state" {
						if _, err := fmt.Fprintf(w, `{"count":%d}`, operations.Load()); err != nil {
							t.Errorf("write state: %v", err)
						}
						return
					}
					w.WriteHeader(http.StatusOK)
				}))
				t.Cleanup(server.Close)
				document := fmt.Sprintf(`version: 1
name: Transport evidence
target: %s
request_timeout: 2s
setup:
  - name: Warm connection
    request: {method: POST, path: /warm}
  - name: Prepare trial
    request:
      method: POST
      path: /prepare
      headers: {Idempotency-Key: test-key}
operation:
  name: Purchase
  method: POST
  path: /purchase
  headers: {Idempotency-Key: test-key}
execution: {attempts: 1, concurrency: 1, trials: 1, reduce: false}
observation:
  method: GET
  path: /state
  headers: {Idempotency-Key: test-key}
invariant:
  name: Transport rule
  %s
`, server.URL, test.invariant)
				path := writeScenarioFile(t, document)
				var stdout, stderr bytes.Buffer
				args := []string{"run", "--format", "text", path}
				if format == "verbose" {
					args = []string{"run", "--verbose", path}
				}
				if format == "json" {
					args = []string{"run", "--format", format, path}
				}
				if code := app.Run(context.Background(), args, &stdout, &stderr); code != test.exit || stderr.Len() != 0 {
					t.Fatalf("exit=%d, want %d; stdout=%s; stderr=%s", code, test.exit, &stdout, &stderr)
				}
				if int(operations.Load()) != test.wantOperations || destinationCalls.Load() != 0 {
					t.Fatalf("operations=%d, want %d; redirected requests=%d", operations.Load(), test.wantOperations, destinationCalls.Load())
				}
				if format != "json" {
					want := "Request writes  2"
					if test.redirect {
						want = "302"
						if format == "text" && test.path == "/purchase" {
							want = "Observed        0 matching HTTP responses"
						}
					}
					if !strings.Contains(stdout.String(), want) {
						t.Fatalf("missing %q: %s", want, &stdout)
					}
					return
				}
				var result struct {
					Status string `json:"status"`
					Trials []struct {
						Evidence struct {
							Setup   []struct{ Execution transportExecution } `json:"setup"`
							History struct {
								Attempts []struct{ Execution transportExecution }
							} `json:"history"`
							Observation transportExecution `json:"observation"`
						} `json:"evidence"`
					} `json:"trials"`
				}
				if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if result.Status != test.status || len(result.Trials) != 1 {
					t.Fatalf("unexpected verdict: %s", &stdout)
				}
				trial := result.Trials[0].Evidence
				var execution transportExecution
				switch test.path {
				case "/prepare":
					execution = trial.Setup[1].Execution
				case "/purchase":
					execution = trial.History.Attempts[0].Execution
				case "/state":
					execution = trial.Observation
				}
				if test.redirect {
					if execution.Response.StatusCode != 302 || execution.RequestWrites != 1 || execution.Error != nil {
						t.Fatalf("lost redirect evidence: %+v", execution)
					}
				} else if execution.RequestWrites != 2 || execution.Response.StatusCode != 200 || execution.Error == nil || execution.Error.Code != "request_retried" {
					t.Fatalf("lost retry evidence: %+v", execution)
				}
			})
		}
	}
}

type transportExecution struct {
	RequestWrites int `json:"request_writes"`
	Response      struct {
		StatusCode int `json:"status_code"`
	} `json:"response"`
	Error *struct {
		Code string `json:"code"`
	} `json:"error"`
}
