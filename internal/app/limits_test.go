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

func TestCLIResourceLimitsAreVisibleAndReturnIncompleteExecution(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ name, limits, resource string }{
		{"exchanges", "max_exchanges: 3", "max_exchanges"},
		{"bodies", "max_retained_bytes: 4096", "max_retained_bytes"},
		{"duration", "max_duration: 10ms", "max_duration"},
	} {
		for _, format := range []string{"json", "text", "verbose"} {
			t.Run(test.name+"/"+format, func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if test.name == "duration" {
						<-r.Context().Done()
						return
					}
					if _, err := w.Write([]byte(strings.Repeat("x", 1000))); err != nil {
						t.Errorf("write response: %v", err)
					}
				}))
				t.Cleanup(server.Close)
				document := fmt.Sprintf(`version: 1
name: Bounded execution
target: %s
request_timeout: 2s
operation: {name: Execute, method: POST, path: /operation}
execution: {attempts: 2, concurrency: 1, trials: 5}
invariant: {name: Matching replies, maximum_successful_attempts: 2}
limits: {%s}
`, server.URL, test.limits)
				args := []string{"run"}
				if format == "json" {
					args = append(args, "--format", "json")
				} else if format == "verbose" {
					args = append(args, "--verbose")
				}
				var stdout, stderr bytes.Buffer
				code := app.Run(context.Background(), append(args, writeScenarioFile(t, document)), &stdout, &stderr)
				if code != 2 || stderr.Len() != 0 || calls.Load() >= 10 {
					t.Fatalf("resource exhaustion became success or ran all work: exit=%d calls=%d stdout=%s stderr=%s", code, calls.Load(), &stdout, &stderr)
				}
				if format == "json" {
					var result struct {
						Status    string
						Resources struct{ Exhausted string }
					}
					if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
						t.Fatal(err)
					}
					if result.Status != "errored" || result.Resources.Exhausted != test.resource {
						t.Fatalf("limit cause lost: %s", &stdout)
					}
				} else if !strings.Contains(stdout.String(), "Limit reached   "+test.resource) || !strings.Contains(stdout.String(), "execution is incomplete") {
					t.Fatalf("missing resource result: %s", &stdout)
				}
			})
		}
	}
}

func TestCLIReportBudgetProducesSmallJSONErrorAndKeepsKnownFailureNotice(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusCreated) }))
	t.Cleanup(server.Close)
	document := fmt.Sprintf(`version: 1
name: Bounded report
target: %s
request_timeout: 2s
operation: {name: Execute, method: POST, path: /operation}
execution: {attempts: 2, concurrency: 1, trials: 1}
invariant: {name: No matching replies, maximum_successful_attempts: 0}
limits: {max_report_bytes: 1}
`, server.URL)
	var stdout, stderr bytes.Buffer
	code := app.Run(context.Background(), []string{"run", "--format", "json", writeScenarioFile(t, document)}, &stdout, &stderr)
	var result struct {
		ReportType string `json:"report_type"`
		Status     string
		Error      struct{ Code, Message string }
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("report limit produced invalid JSON: %v; %s", err, &stdout)
	}
	if code != 2 || stderr.Len() != 0 || result.ReportType != "error" || result.Status != "errored" || result.Error.Code != "resource_limit" || !strings.Contains(result.Error.Message, "violations exist") || stdout.Len() > 2048 {
		t.Fatalf("unexpected report fallback: exit=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
}

func TestCLIOverridesCannotBypassConfiguredWorkLimit(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(200) }))
	t.Cleanup(server.Close)
	document := fmt.Sprintf(`version: 1
name: Override limit
target: %s
request_timeout: 2s
operation: {name: Execute, method: POST, path: /operation}
execution: {attempts: 2, concurrency: 1, trials: 1}
invariant: {name: Matching replies, maximum_successful_attempts: 2}
limits: {max_attempts: 2}
`, server.URL)
	var stdout, stderr bytes.Buffer
	code := app.Run(context.Background(), []string{"run", "--format", "json", "--attempts", "3", writeScenarioFile(t, document)}, &stdout, &stderr)
	if code != 2 || calls.Load() != 0 || !strings.Contains(stdout.String(), "max_attempts") {
		t.Fatalf("override bypassed validation: exit=%d calls=%d stdout=%s stderr=%s", code, calls.Load(), &stdout, &stderr)
	}
}
