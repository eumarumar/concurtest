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

func TestCLIReductionRejectsDirectionDriftWithoutHidingViolations(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, reductionStatus, finding string
		trials                         int
	}{
		{"drift", "unchanged", "No smaller tested configuration", 3},
		{"split", "skipped", "No single failure direction had a strict majority", 4},
		{"rare", "skipped", "1 violations in 10 trials", 10},
	} {
		for _, format := range []string{"json", "text", "verbose"} {
			t.Run(test.name+"/"+format, func(t *testing.T) {
				var epoch, count atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/reset":
						epoch.Add(1)
						count.Store(0)
					case "/operation":
						value := int32(0)
						switch test.name {
						case "drift":
							if epoch.Load() <= 3 {
								value = 2
							}
						case "split":
							if epoch.Load()%2 == 0 {
								value = 2
							}
						case "rare":
							value = 1
							if epoch.Load() == 1 {
								value = 2
							}
						}
						count.Store(value)
					case "/state":
						if _, err := fmt.Fprintf(w, `{"count":%d}`, count.Load()); err != nil {
							t.Errorf("write state: %v", err)
						}
					default:
						http.NotFound(w, r)
					}
				}))
				t.Cleanup(server.Close)
				document := fmt.Sprintf(`version: 1
name: Reduction direction
target: %s
request_timeout: 2s
setup: {method: POST, path: /reset}
precondition:
  request: {method: GET, path: /state}
  assert: {name: Initially empty, json_integer_path: [count], equals: 0}
operation: {name: Execute, method: POST, path: /operation}
execution: {attempts: 4, concurrency: 4, trials: %d, reduce: true}
observation: {method: GET, path: /state}
invariant: {name: One effect, json_integer_path: [count], equals: 1}
`, server.URL, test.trials)
				args := []string{"run"}
				if format == "json" {
					args = append(args, "--format", "json")
				} else if format == "verbose" {
					args = append(args, "--verbose")
				}
				var stdout, stderr bytes.Buffer
				code := app.Run(context.Background(), append(args, writeScenarioFile(t, document)), &stdout, &stderr)
				if code != 1 || stderr.Len() != 0 {
					t.Fatalf("violation lost: exit=%d stdout=%s stderr=%s", code, &stdout, &stderr)
				}
				if format == "json" {
					var result struct {
						Status    string
						Reduction struct {
							Status     string
							Direction  *string `json:"failure_direction"`
							Selection  *struct{ Attempts, Concurrency int }
							Candidates []struct {
								Accepted bool
								Matching int  `json:"matching_violations"`
								Outcomes bool `json:"outcome_requirements_met"`
							}
						}
					}
					if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
						t.Fatal(err)
					}
					search := result.Reduction
					if result.Status != "violated" || search.Status != test.reductionStatus {
						t.Fatalf("wrong verdict: %s", &stdout)
					}
					if test.name == "drift" {
						if search.Direction == nil || *search.Direction != "above" || search.Selection == nil || search.Selection.Attempts != 4 || search.Selection.Concurrency != 4 || len(search.Candidates) != 5 {
							t.Fatalf("opposite failure selected: %s", &stdout)
						}
						for _, candidate := range search.Candidates {
							if candidate.Accepted || candidate.Matching != 0 || !candidate.Outcomes {
								t.Fatalf("wrong candidate rejection: %+v", candidate)
							}
						}
					} else if search.Direction != nil || search.Selection != nil || len(search.Candidates) != 0 || epoch.Load() != int32(test.trials) {
						t.Fatalf("skipped search started candidates or claimed selection: %s", &stdout)
					}
				} else {
					if !strings.Contains(stdout.String(), test.finding) || !strings.Contains(stdout.String(), "VIOLATED") {
						t.Fatalf("missing finding: %s", &stdout)
					}
					if test.name == "drift" && (!strings.Contains(stdout.String(), "--attempts 4 --concurrency 4 --no-reduce") || (format == "verbose" && !strings.Contains(stdout.String(), "0 matching violations"))) {
						t.Fatalf("missing direction rejection or baseline recipe: %s", &stdout)
					}
				}
			})
		}
	}
}
