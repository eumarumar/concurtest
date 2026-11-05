package reduction

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/eumarumar/concurtest/internal/engine"
)

func memberCheck(name, path string, equals int64) engine.Invariant {
	return engine.Invariant{JSONInteger: &engine.JSONIntegerInvariant{Name: name, Path: []string{path}, Equals: new(equals)}}
}

func TestReduceConjunctionPreservesCheckIdentityAndDirection(t *testing.T) {
	t.Parallel()
	for _, drift := range []string{"different member", "opposite direction", "unreadable sibling"} {
		t.Run(drift, func(t *testing.T) {
			var trial atomic.Int32
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/setup" {
					trial.Add(1)
				}
				if r.URL.Path == "/state" {
					body := `{"a":2,"b":0}`
					if trial.Load() >= 4 && trial.Load() <= 6 {
						switch drift {
						case "different member":
							body = `{"a":1,"b":2}`
						case "opposite direction":
							body = `{"a":0,"b":0}`
						case "unreadable sibling":
							body = `{"a":2}`
						}
					}
					return response(200, body), nil
				}
				return response(200, ""), nil
			})}
			scenario := scenarioWithSetup(4, 4)
			scenario.Invariant = engine.Invariant{All: []engine.Invariant{memberCheck("A", "a", 1), memberCheck("B", "b", 0)}}
			result, err := Reduce(context.Background(), client, scenario, 3)
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != StatusReduced || result.FailureCheck != "A" || result.FailureDirection != FailureAbove || result.Selected != (Candidate{Attempts: 3, Concurrency: 2}) || len(result.Candidates) != 2 {
				t.Fatalf("wrong failure selected: %+v", result)
			}
			rejected, accepted := result.Candidates[0], result.Candidates[1]
			if rejected.Accepted || rejected.Summary.Violated != 3 || rejected.Trials != nil || !rejected.OutcomeRequirementsMet {
				t.Fatalf("invalid candidate accepted or evidence lost: %+v", rejected)
			}
			if drift == "unreadable sibling" {
				if rejected.ChecksComplete || rejected.MatchingViolations != 3 {
					t.Fatalf("incomplete evidence counted as clean: %+v", rejected)
				}
			} else if !rejected.ChecksComplete || rejected.MatchingViolations != 0 {
				t.Fatalf("different failure matched target: %+v", rejected)
			}
			if !accepted.Accepted || !accepted.ChecksComplete || accepted.MatchingViolations != 3 || accepted.Trials == nil || !result.MatchesFailure(accepted.Trials.Trials[0]) {
				t.Fatalf("same failure did not qualify: %+v", accepted)
			}
		})
	}
}

func TestReduceConjunctionSkipsIncompleteOrUnrelatedBaselineFailures(t *testing.T) {
	t.Parallel()
	for _, unreadable := range []bool{false, true} {
		t.Run(fmt.Sprint(unreadable), func(t *testing.T) {
			var trial atomic.Int32
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/setup" {
					trial.Add(1)
				}
				if r.URL.Path == "/state" {
					body := `{"a":2}`
					if !unreadable {
						values := []int{0, 0, 0}
						values[int(trial.Load())-1] = 2
						body = fmt.Sprintf(`{"a":%d,"b":%d,"c":%d}`, values[0], values[1], values[2])
					}
					return response(200, body), nil
				}
				return response(200, ""), nil
			})}
			scenario := scenarioWithSetup(4, 4)
			scenario.Invariant = engine.Invariant{All: []engine.Invariant{memberCheck("A", "a", 0), memberCheck("B", "b", 0), memberCheck("C", "c", 0)}}
			result, err := Reduce(context.Background(), client, scenario, 3)
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != StatusSkipped || result.Baseline.Status != engine.TrialStatusViolated || len(result.Candidates) != 0 || result.SelectedTrials != nil || trial.Load() != 3 || result.FailureCheck != "" {
				t.Fatalf("baseline drift or unreadable checks started search: %+v", result)
			}
			if unreadable && result.Baseline.Trials[0].Err == nil {
				t.Fatal("baseline error was lost")
			}
		})
	}
}

func TestReduceConjunctionCapturesFreshSharedBaselines(t *testing.T) {
	t.Parallel()
	var trial, operations, reads atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/setup" {
			trial.Add(1)
			operations.Store(0)
		}
		if r.URL.Path == "/operation" {
			operations.Add(1)
		}
		if r.URL.Path == "/state" {
			reads.Add(1)
			value := int(trial.Load()) * 100
			if operations.Load() > 0 {
				value += 2
			}
			return response(200, fmt.Sprintf(`{"a":%d,"b":0}`, value)), nil
		}
		return response(200, ""), nil
	})}
	scenario := scenarioWithSetup(4, 4)
	change := memberCheck("Change", "a", 0)
	change.JSONInteger.Equals, change.JSONInteger.Change = nil, new(int64(1))
	scenario.Invariant = engine.Invariant{All: []engine.Invariant{change, memberCheck("Other", "b", 0)}}
	result, err := Reduce(context.Background(), client, scenario, 3)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusReduced || trial.Load() != 6 || reads.Load() != 12 || result.FailureCheck != "Change" {
		t.Fatalf("baseline reuse or extra observations: %+v reads=%d", result, reads.Load())
	}
	for index, trial := range result.SelectedTrials.Trials {
		e := trial.Run.Evaluation.All[0].Evaluation.JSONInteger
		if *e.Baseline != int64(index+4)*100 || *e.Change != 2 {
			t.Fatalf("wrong candidate baseline: %+v", e)
		}
	}
}
