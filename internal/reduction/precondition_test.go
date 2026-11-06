package reduction

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eumarumar/concurtest/internal/engine"
)

func TestReductionRechecksPreparationForEveryTrialAndCandidate(t *testing.T) {
	t.Parallel()
	for _, failed := range []string{"none", "baseline setup", "baseline precondition", "candidate setup", "candidate precondition"} {
		t.Run(failed, func(t *testing.T) {
			var setups, checks, operations atomic.Int32
			bad := func() bool {
				return strings.HasPrefix(failed, "baseline") || (strings.HasPrefix(failed, "candidate") && setups.Load() > 3)
			}
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/setup":
					setups.Add(1)
					return response(http.StatusOK, fmt.Sprintf(`{"ok":%t}`, !(strings.HasSuffix(failed, "setup") && bad()))), nil
				case "/initial":
					checks.Add(1)
					stock := 10
					if strings.HasSuffix(failed, "precondition") && bad() {
						stock = 2
					}
					return response(http.StatusOK, fmt.Sprintf(`{"stock":%d}`, stock)), nil
				case "/operation":
					operations.Add(1)
					return response(http.StatusOK, ""), nil
				case "/state":
					return response(http.StatusOK, `{"stock":-1}`), nil
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					return response(http.StatusNotFound, ""), nil
				}
			})}
			scenario := scenarioWithSetup(3, 2)
			scenario.Setup[0].Assertion = &engine.ResponseAssertion{JSONBoolean: &engine.JSONBooleanAssertion{Name: "Reset acknowledged", Path: []string{"ok"}, Equals: true}}
			scenario.Precondition = &engine.Precondition{Request: engine.HTTPRequest{Method: http.MethodGet, URL: "http://example.test/initial"}, Assertion: engine.ResponseAssertion{JSONInteger: &engine.JSONIntegerInvariant{Name: "Initial stock", Path: []string{"stock"}, Equals: new(int64(10))}}}
			result, err := Reduce(context.Background(), client, scenario, 3)
			if err != nil {
				t.Fatal(err)
			}
			wantSetups, wantChecks, wantOperations := int32(6), int32(6), int32(15)
			if strings.HasPrefix(failed, "baseline") {
				wantSetups, wantChecks, wantOperations = 3, 3, 0
				if result.Status != StatusSkipped || result.Baseline.Status != engine.TrialStatusErrored || len(result.Candidates) != 0 {
					t.Fatalf("bad preparation qualified baseline: %+v", result)
				}
			} else if strings.HasPrefix(failed, "candidate") {
				wantOperations = 9
				if result.Status != StatusUnchanged || len(result.Candidates) != 1 || result.Candidates[0].Accepted || result.Candidates[0].Summary.Errored != 3 {
					t.Fatalf("bad preparation qualified candidate: %+v", result)
				}
			} else if result.Status != StatusReduced {
				t.Fatalf("valid preparation failed reduction: %+v", result)
			}
			if strings.HasSuffix(failed, "setup") {
				wantChecks -= 3
			}
			if setups.Load() != wantSetups || checks.Load() != wantChecks || operations.Load() != wantOperations {
				t.Fatalf("setups=%d checks=%d operations=%d, want %d/%d/%d", setups.Load(), checks.Load(), operations.Load(), wantSetups, wantChecks, wantOperations)
			}
			if result.SelectedTrials != nil {
				for _, trial := range result.SelectedTrials.Trials {
					if trial.Run.Setup[0].Assertion == nil || !trial.Run.Setup[0].Assertion.Satisfied || trial.Run.Precondition == nil || !trial.Run.Precondition.Assertion.Satisfied {
						t.Fatalf("selected result omitted checked preconditions: %+v", trial)
					}
				}
			}
		})
	}
}
