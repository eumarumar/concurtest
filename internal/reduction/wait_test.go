package reduction

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eumarumar/concurtest/internal/engine"
)

func reductionWait(path string) *engine.WaitCondition {
	return &engine.WaitCondition{Request: engine.HTTPRequest{Method: http.MethodGet, URL: "http://example.test" + path}, Assertion: engine.ResponseAssertion{JSONInteger: &engine.JSONIntegerInvariant{Name: "No pending work", Path: []string{"pending"}, Equals: new(int64(0))}}, Timeout: time.Second, Interval: time.Millisecond, MaxPolls: 2}
}

func TestReductionStopsAfterUnverifiedReadinessOrCompletion(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		failed                              string
		history                             bool
		setups, operations, ready, complete int32
	}{
		{"none", false, 6, 15, 6, 6},
		{"baseline readiness", false, 0, 0, 2, 0},
		{"baseline completion", false, 1, 3, 1, 2},
		{"candidate readiness", false, 3, 9, 5, 3},
		{"candidate completion", false, 4, 11, 4, 5},
		{"candidate completion", true, 4, 11, 4, 5},
	} {
		t.Run(fmt.Sprintf("%s/history=%t", test.failed, test.history), func(t *testing.T) {
			var setups, operations, ready, complete atomic.Int32
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				pending := 0
				switch r.URL.Path {
				case "/ready":
					ready.Add(1)
					if strings.HasSuffix(test.failed, "readiness") && (strings.HasPrefix(test.failed, "baseline") || setups.Load() >= 3) {
						pending = 1
					}
				case "/complete":
					complete.Add(1)
					if strings.HasSuffix(test.failed, "completion") && (strings.HasPrefix(test.failed, "baseline") || setups.Load() > 3) {
						pending = 1
					}
				case "/setup":
					setups.Add(1)
					return response(http.StatusNoContent, ""), nil
				case "/operation":
					operations.Add(1)
					return response(http.StatusCreated, ""), nil
				case "/state":
					return response(http.StatusOK, `{"stock":-1}`), nil
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					return response(404, ""), nil
				}
				return response(http.StatusOK, fmt.Sprintf(`{"pending":%d}`, pending)), nil
			})}
			scenario := scenarioWithSetup(3, 2)
			scenario.Readiness, scenario.Completion = reductionWait("/ready"), reductionWait("/complete")
			if test.history {
				scenario.Invariant = engine.Invariant{MaximumSuccessfulAttempts: &engine.MaximumSuccessfulAttemptsInvariant{Name: "At most one accepted response", Maximum: 1}}
			}
			result, err := Reduce(context.Background(), client, scenario, 3)
			if err != nil {
				t.Fatal(err)
			}
			if setups.Load() != test.setups || operations.Load() != test.operations || ready.Load() != test.ready || complete.Load() != test.complete {
				t.Fatalf("traffic after unverified wait: setups=%d operations=%d readiness=%d completion=%d", setups.Load(), operations.Load(), ready.Load(), complete.Load())
			}
			if test.failed == "none" {
				if result.Status != StatusReduced || result.SelectedTrials == nil {
					t.Fatalf("valid waits did not qualify: %+v", result)
				}
				for _, trial := range result.SelectedTrials.Trials {
					if !trial.Run.Readiness.Satisfied || !trial.Run.Completion.Satisfied {
						t.Fatal("selected result lost wait evidence")
					}
				}
				return
			}
			if result.Status != StatusStopped || result.SelectedTrials != nil {
				t.Fatalf("search claimed completion after failed condition: %+v", result)
			}
			if strings.HasPrefix(test.failed, "baseline") {
				if len(result.Candidates) != 0 || len(result.Baseline.Trials) != 1 {
					t.Fatal("baseline failure did not stop reduction")
				}
			} else {
				if len(result.Candidates) != 1 || result.Candidates[0].Accepted || result.Candidates[0].Err == nil || result.Candidates[0].Trials == nil || len(result.Candidates[0].Trials.Trials) != 1 {
					t.Fatalf("stopped candidate evidence lost: %+v", result)
				}
				if test.history && result.Candidates[0].Trials.Status != engine.TrialStatusViolated {
					t.Fatal("demonstrated history violation was hidden")
				}
			}
		})
	}
}
