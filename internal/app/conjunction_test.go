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
	"time"

	"github.com/eumarumar/concurtest/internal/app"
)

func TestCLIBalanceAndLedgerOnTheSameExecution(t *testing.T) {
	t.Parallel()
	for _, combined := range []bool{false, true} {
		t.Run(fmt.Sprint(combined), func(t *testing.T) {
			var mu sync.Mutex
			sender, recipient, transfers, arrivals, observations := 10000, 0, 0, 0, 0
			gate := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/reset":
					mu.Lock()
					sender, recipient, transfers, arrivals = 10000, 0, 0, 0
					gate = make(chan struct{})
					mu.Unlock()
					w.WriteHeader(http.StatusNoContent)
				case "/transfer":
					// Force both requests to read the old balance before either
					// writes. The ledger retains both transfers while one debit is lost.
					mu.Lock()
					initial := sender
					arrivals++
					currentGate := gate
					if arrivals == 2 {
						close(currentGate)
					}
					mu.Unlock()
					select {
					case <-currentGate:
					case <-r.Context().Done():
						return
					}
					mu.Lock()
					sender = initial - 7000
					recipient += 7000
					transfers++
					mu.Unlock()
					w.WriteHeader(http.StatusCreated)
				case "/state":
					mu.Lock()
					balance, credit, mismatch := sender, recipient, (10000-sender)-transfers*7000
					observations++
					mu.Unlock()
					if _, err := fmt.Fprintf(w, `{"sender_cents":%d,"recipient_cents":%d,"ledger_mismatch_cents":%d}`, balance, credit, mismatch); err != nil {
						t.Errorf("write state: %v", err)
					}
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			invariant := "name: Sender remains nonnegative\n  json_integer_path: [sender_cents]\n  minimum: 0"
			if combined {
				invariant = "all:\n    - {name: Sender remains nonnegative, json_integer_path: [sender_cents], minimum: 0}\n    - {name: Ledger reconciles, json_integer_path: [ledger_mismatch_cents], equals: 0}"
			}
			document := fmt.Sprintf(`version: 1
name: Balance and ledger
target: %s
request_timeout: 2s
setup: {method: POST, path: /reset}
operation: {name: Transfer, method: POST, path: /transfer}
execution: {attempts: 2, concurrency: 2, trials: 3}
observation: {method: GET, path: /state}
invariant:
  %s
`, server.URL, invariant)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var stdout, stderr bytes.Buffer
			code := app.Run(ctx, []string{"run", "--format", "json", writeScenarioFile(t, document)}, &stdout, &stderr)
			var result struct {
				Status string
				Trials []struct {
					Evidence struct {
						Observation struct {
							Response struct {
								Body struct{ Data string } `json:"body_excerpt"`
							}
						}
					}
				}
			}
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatalf("decode result: %v; stdout=%s stderr=%s", err, &stdout, &stderr)
			}
			t.Logf("combined=%v exit=%d status=%s trials=%d", combined, code, result.Status, len(result.Trials))
			wantCode, wantStatus := 0, "passed"
			if combined {
				wantCode, wantStatus = 1, "violated"
			}
			if code != wantCode || result.Status != wantStatus || len(result.Trials) != 3 || stderr.Len() != 0 {
				t.Fatalf("exit=%d status=%s want=%d/%s; stdout=%s stderr=%s", code, result.Status, wantCode, wantStatus, &stdout, &stderr)
			}
			for _, trial := range result.Trials {
				var state struct {
					Balance  int `json:"sender_cents"`
					Mismatch int `json:"ledger_mismatch_cents"`
				}
				if err := json.Unmarshal([]byte(trial.Evidence.Observation.Response.Body.Data), &state); err != nil {
					t.Fatal(err)
				}
				if state.Balance != 3000 || state.Mismatch != -7000 {
					t.Fatalf("unexpected target state: %+v", state)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if observations != 3 || transfers != 2 {
				t.Fatalf("observations=%d transfers=%d", observations, transfers)
			}
		})
	}
}

func TestCLIConjunctionVerdictsInEveryFormat(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, body, status string
		httpStatus, exit   int
	}{
		{"all pass", `{"balance":3,"ledger":0}`, "passed", 200, 0},
		{"ledger violates", `{"balance":3,"ledger":1}`, "violated", 200, 1},
		{"unreadable ledger", `{"balance":3}`, "errored", 200, 2},
		{"violation plus error", `{"balance":-1}`, "violated", 200, 1},
		{"unusable outcomes", `{"balance":3,"ledger":0}`, "inconclusive", 401, 2},
		{"violation with unusable outcomes", `{"balance":3,"ledger":1}`, "violated", 401, 1},
	} {
		for _, format := range []string{"json", "text", "verbose"} {
			t.Run(test.name+"/"+format, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/state" {
						if _, err := w.Write([]byte(test.body)); err != nil {
							t.Errorf("write state: %v", err)
						}
						return
					}
					w.WriteHeader(test.httpStatus)
				}))
				t.Cleanup(server.Close)
				document := fmt.Sprintf(`version: 1
name: Combined verdict
target: %s
request_timeout: 2s
operation: {name: Transfer, method: POST, path: /operation}
execution: {attempts: 2, concurrency: 2, trials: 1}
observation: {method: GET, path: /state}
invariant:
  all:
    - {name: Balance, json_integer_path: [balance], minimum: 0}
    - {name: Ledger, json_integer_path: [ledger], equals: 0}
`, server.URL)
				args := []string{"run"}
				if format == "json" {
					args = append(args, "--format", "json")
				} else if format == "verbose" {
					args = append(args, "--verbose")
				}
				var stdout, stderr bytes.Buffer
				code := app.Run(context.Background(), append(args, writeScenarioFile(t, document)), &stdout, &stderr)
				if code != test.exit || stderr.Len() != 0 {
					t.Fatalf("exit=%d want=%d; stdout=%s stderr=%s", code, test.exit, &stdout, &stderr)
				}
				if format == "json" {
					var result struct{ Status string }
					if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
						t.Fatal(err)
					}
					if result.Status != test.status {
						t.Fatalf("status=%s want=%s", result.Status, test.status)
					}
				} else {
					for _, want := range []string{strings.ToUpper(test.status), "All 2 checks must hold", "Balance", "Ledger"} {
						if !strings.Contains(stdout.String(), want) {
							t.Fatalf("missing %q: %s", want, &stdout)
						}
					}
				}
			})
		}
	}
}
