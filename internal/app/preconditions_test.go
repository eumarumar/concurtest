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

// A setup acknowledgement is not verified unless the scenario declares checks.
// This fixture preserves the original investigation's invalid initial state and
// serves as a compatibility control for the explicit checks introduced in Step 3.
func TestCLIUncheckedResetCanLeaveInvalidInitialState(t *testing.T) {
	t.Parallel()
	var resets, operations, writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/reset":
			resets.Add(1)
			if _, err := fmt.Fprint(w, `{"ok":false}`); err != nil {
				t.Errorf("write reset response: %v", err)
			}
		case "/operation":
			operations.Add(1)
			w.WriteHeader(http.StatusConflict)
		case "/state":
			if _, err := fmt.Fprint(w, `{"count":2}`); err != nil {
				t.Errorf("write state: %v", err)
			}
		default:
			writes.Add(1)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	document := fmt.Sprintf(`version: 1
name: Unchecked reset
target: %s
request_timeout: 2s
setup: {method: POST, path: /reset}
operation: {name: Operation, method: POST, path: /operation}
execution: {attempts: 2, concurrency: 2, trials: 3}
observation: {method: GET, path: /state}
invariant: {name: At most one effect, json_integer_path: [count], maximum: 1}
`, server.URL)
	var stdout, stderr bytes.Buffer
	code := app.Run(context.Background(), []string{"run", "--format", "json", writeScenarioFile(t, document)}, &stdout, &stderr)
	var report struct {
		Status string
		Trials []struct {
			Status   string
			Evidence struct {
				Setup []struct {
					Execution struct {
						Response struct {
							Body struct{ Data string } `json:"body_excerpt"`
						}
					}
				}
				Evaluation struct{ Observed int } `json:"invariant_evaluation"`
			}
		}
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v; stdout=%s; stderr=%s", err, &stdout, &stderr)
	}
	t.Logf("exit=%d, status=%s, trials=%d, resets=%d, rejected operations=%d, new writes=%d", code, report.Status, len(report.Trials), resets.Load(), operations.Load(), writes.Load())
	if code != 1 || report.Status != "violated" || len(report.Trials) != 3 || resets.Load() != 3 || operations.Load() != 6 || writes.Load() != 0 || stderr.Len() != 0 {
		t.Fatalf("unexpected reproduction: status=%s; stdout=%s; stderr=%s", report.Status, &stdout, &stderr)
	}
	for _, trial := range report.Trials {
		if trial.Status != "violated" || len(trial.Evidence.Setup) != 1 || trial.Evidence.Setup[0].Execution.Response.Body.Data != `{"ok":false}` || trial.Evidence.Evaluation.Observed != 2 {
			t.Fatalf("lost reproduction evidence: %+v", trial)
		}
	}
}

func TestCLIPreparationChecksPreventInvalidTrials(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, resetBody, status string
		initial                 int32
		setupCheck, history     bool
		exit                    int
	}{
		{"negative reset acknowledgement", `{"ok":false}`, "errored", 2, true, false, 2},
		{"unreadable reset acknowledgement", `{"ok":"true"}`, "errored", 2, true, false, 2},
		{"reset claims success but state is wrong", `{"ok":true}`, "errored", 2, true, false, 2},
		{"initial state check for history invariant", `{"ok":true}`, "errored", 2, false, true, 2},
		{"valid preparation preserves state violation", `{"ok":true}`, "violated", 0, true, false, 1},
		{"valid preparation with history and no final observer", `{"ok":true}`, "passed", 0, true, true, 0},
	} {
		for _, format := range []string{"text", "verbose", "json"} {
			t.Run(test.name+"/"+format, func(t *testing.T) {
				var resets, later, operations, observations, count atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/reset":
						resets.Add(1)
						count.Store(test.initial)
						if _, err := fmt.Fprint(w, test.resetBody); err != nil {
							t.Errorf("write reset: %v", err)
						}
					case "/later":
						later.Add(1)
						w.WriteHeader(http.StatusNoContent)
					case "/operation":
						operations.Add(1)
						count.Add(1)
						w.WriteHeader(http.StatusCreated)
					case "/state":
						observations.Add(1)
						if _, err := fmt.Fprintf(w, `{"count":%d}`, count.Load()); err != nil {
							t.Errorf("write state: %v", err)
						}
					default:
						http.NotFound(w, r)
					}
				}))
				t.Cleanup(server.Close)
				assertion := ""
				if test.setupCheck {
					assertion = "    assert: {name: Reset acknowledged, json_boolean_path: [ok], equals: true}\n"
				}
				observation := "observation: {method: GET, path: /state}\n"
				invariant := "json_integer_path: [count], maximum: 1"
				if test.history {
					observation, invariant = "", "maximum_successful_attempts: 2"
				}
				document := fmt.Sprintf(`version: 1
name: Verified preparation
target: %s
request_timeout: 2s
setup:
  - name: Reset
    request: {method: POST, path: /reset}
%s  - name: Prepare basket
    request: {method: POST, path: /later}
precondition:
  request: {method: GET, path: /state}
  assert: {name: Initial count, json_integer_path: [count], equals: 0}
operation: {name: Operation, method: POST, path: /operation}
execution: {attempts: 2, concurrency: 2, trials: 3}
%sinvariant: {name: Operation rule, %s}
`, server.URL, assertion, observation, invariant)
				args := []string{"run", "--format", format}
				if format == "verbose" {
					args = []string{"run", "--verbose"}
				}
				var stdout, stderr bytes.Buffer
				code := app.Run(context.Background(), append(args, writeScenarioFile(t, document)), &stdout, &stderr)
				if code != test.exit || stderr.Len() != 0 {
					t.Fatalf("exit=%d want %d; stdout=%s; stderr=%s", code, test.exit, &stdout, &stderr)
				}
				failedSetup := test.setupCheck && test.resetBody != `{"ok":true}`
				wantLater, wantOperations, wantObservations := int32(3), int32(0), int32(3)
				if failedSetup {
					wantLater, wantObservations = 0, 0
				}
				if test.status != "errored" {
					wantOperations = 6
					if !test.history {
						wantObservations = 6
					}
				}
				if resets.Load() != 3 || later.Load() != wantLater || operations.Load() != wantOperations || observations.Load() != wantObservations {
					t.Fatalf("resets=%d later=%d operations=%d state reads=%d", resets.Load(), later.Load(), operations.Load(), observations.Load())
				}
				if format != "json" {
					if !strings.Contains(stdout.String(), strings.ToUpper(test.status)) {
						t.Fatalf("missing verdict: %s", &stdout)
					}
					if test.status == "errored" {
						want := "Check result    FAILED"
						if test.resetBody == `{"ok":"true"}` {
							want = "Check result    NOT VERIFIED"
						}
						if !strings.Contains(stdout.String(), want) {
							t.Fatalf("missing %q: %s", want, &stdout)
						}
					}
					return
				}
				var result struct {
					Status string
					Trials []struct {
						Status   string
						Error    *struct{ Code string }
						Evidence struct {
							Setup        []struct{ Assertion *preparationAssertionEvidence }
							Precondition *struct{ Assertion *preparationAssertionEvidence }
							History      struct{ Attempts []json.RawMessage }
							Evaluation   json.RawMessage `json:"invariant_evaluation"`
						}
					}
				}
				if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if result.Status != test.status || len(result.Trials) != 3 {
					t.Fatalf("unexpected report: %s", &stdout)
				}
				for _, trial := range result.Trials {
					if trial.Status != test.status {
						t.Fatalf("unexpected trial: %+v", trial)
					}
					if test.status == "errored" {
						if trial.Error == nil || trial.Error.Code != "precondition_failed" || len(trial.Evidence.History.Attempts) != 0 || string(trial.Evidence.Evaluation) != "null" {
							t.Fatalf("failed check was attributed to operations: %+v", trial)
						}
					}
					if failedSetup {
						check := trial.Evidence.Setup[0].Assertion
						if check == nil || check.Satisfied || trial.Evidence.Precondition != nil {
							t.Fatalf("setup assertion evidence lost: %+v", trial)
						}
						if test.resetBody == `{"ok":false}` && check.Observed != false {
							t.Fatalf("false observation lost: %+v", check)
						}
					} else {
						if trial.Evidence.Precondition == nil || trial.Evidence.Precondition.Assertion == nil {
							t.Fatalf("precondition evidence lost: %+v", trial)
						}
						check := trial.Evidence.Precondition.Assertion
						if check.Satisfied != (test.initial == 0) || check.Observed != float64(test.initial) {
							t.Fatalf("precondition observation lost: %+v", check)
						}
					}
				}
			})
		}
	}
}

type preparationAssertionEvidence struct {
	Type      string
	Observed  any
	Satisfied bool
}
