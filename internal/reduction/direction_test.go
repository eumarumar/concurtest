package reduction

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/eumarumar/concurtest/internal/engine"
)

// Match the research schedule: three baseline trials observe two, then an
// external condition changes and all smaller candidates observe zero.
func TestReduceRejectsOppositeEqualityFailure(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusConflict, http.StatusOK} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var trial atomic.Int32
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/setup":
					trial.Add(1)
					return response(http.StatusOK, ""), nil
				case "/operation":
					if trial.Load() > 3 {
						return response(status, ""), nil
					}
					return response(http.StatusOK, ""), nil
				case "/state":
					if trial.Load() > 3 {
						return response(http.StatusOK, `{"stock":0}`), nil
					}
					return response(http.StatusOK, `{"stock":2}`), nil
				default:
					t.Errorf("unexpected request: %s", r.URL)
					return response(http.StatusNotFound, ""), nil
				}
			})}
			scenario := scenarioWithSetup(4, 4)
			scenario.Invariant = engine.Invariant{JSONInteger: &engine.JSONIntegerInvariant{
				Name: "one effect", Path: []string{"stock"}, Equals: new(int64(1)),
			}}
			result, err := Reduce(context.Background(), client, scenario, 3)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("status=%s baseline=%d selected=%d first candidate violations=%d outcome requirements met=%v",
				result.Status, result.Baseline.Trials[0].Run.Evaluation.JSONInteger.Observed,
				result.SelectedTrials.Trials[0].Run.Evaluation.JSONInteger.Observed,
				result.Candidates[0].Summary.Violated, result.Candidates[0].OutcomeRequirementsMet)
			if result.Status != StatusUnchanged || result.Selected != (Candidate{Attempts: 4, Concurrency: 4}) {
				t.Fatalf("opposite failure replaced baseline: status=%s selected=%+v", result.Status, result.Selected)
			}
			for _, candidate := range result.Candidates {
				if candidate.Accepted || candidate.Summary.Violated != 3 || candidate.Trials != nil {
					t.Fatalf("incorrect rejected candidate evidence: %+v", candidate)
				}
			}
		})
	}
}

func TestReduceContinuesPastOppositeDirection(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		invariant engine.JSONIntegerInvariant
		baseline  int64
		opposite  int64
		matching  int64
		direction FailureDirection
	}{
		{"equality above", engine.JSONIntegerInvariant{Equals: new(int64(1))}, 2, 0, 3, FailureAbove},
		{"equality below", engine.JSONIntegerInvariant{Equals: new(int64(1))}, 0, 2, -1, FailureBelow},
		{"range above", engine.JSONIntegerInvariant{Minimum: new(int64(1)), Maximum: new(int64(3))}, 4, 0, 5, FailureAbove},
		{"range below", engine.JSONIntegerInvariant{Minimum: new(int64(1)), Maximum: new(int64(3))}, 0, 4, -1, FailureBelow},
		// The final values are all positive; only the measured change determines direction.
		{"change above", engine.JSONIntegerInvariant{Change: new(int64(1))}, 2, 0, 3, FailureAbove},
		{"change below", engine.JSONIntegerInvariant{Change: new(int64(1))}, 0, 2, -1, FailureBelow},
		{"negative change above", engine.JSONIntegerInvariant{Change: new(int64(-1))}, 0, -2, 1, FailureAbove},
		{"negative change below", engine.JSONIntegerInvariant{Change: new(int64(-1))}, -2, 0, -3, FailureBelow},
		{"extreme equality above", engine.JSONIntegerInvariant{Equals: new(int64(-1))}, math.MaxInt64, math.MinInt64, 0, FailureAbove},
		{"extreme equality below", engine.JSONIntegerInvariant{Equals: new(int64(1))}, math.MinInt64, math.MaxInt64, 0, FailureBelow},
	} {
		t.Run(test.name, func(t *testing.T) {
			var trial, operations atomic.Int32
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/setup":
					trial.Add(1)
					operations.Store(0)
				case "/operation":
					operations.Add(1)
				case "/state":
					value := test.baseline
					if trial.Load() > 6 {
						value = test.matching
					} else if trial.Load() > 3 {
						value = test.opposite
					}
					if test.invariant.Change != nil {
						value += 10
						if operations.Load() == 0 {
							value = 10
						}
					}
					return response(http.StatusOK, fmt.Sprintf(`{"stock":%d}`, value)), nil
				}
				return response(http.StatusOK, ""), nil
			})}
			scenario := scenarioWithSetup(4, 4)
			invariant := test.invariant
			invariant.Name, invariant.Path = "same predicate", []string{"stock"}
			scenario.Invariant.JSONInteger = &invariant
			result, err := Reduce(context.Background(), client, scenario, 3)
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != StatusReduced || result.Selected != (Candidate{Attempts: 3, Concurrency: 2}) || result.FailureDirection != test.direction || len(result.Candidates) != 2 {
				t.Fatalf("wrong selection/direction: %+v", result)
			}
			rejected, accepted := result.Candidates[0], result.Candidates[1]
			if rejected.Accepted || rejected.MatchingViolations != 0 || rejected.Summary.Violated != 3 || !rejected.OutcomeRequirementsMet || rejected.Trials != nil {
				t.Fatalf("opposite direction not rejected: %+v", rejected)
			}
			if !accepted.Accepted || accepted.MatchingViolations != 3 || accepted.Trials == nil || !result.MatchesFailure(accepted.Trials.Trials[0]) {
				t.Fatalf("matching direction not retained: %+v", accepted)
			}
		})
	}
}

func TestReduceRequiresMajorityInOneDirection(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		baseline  []int64
		candidate []int64
		status    Status
		matching  int
	}{
		{"split baseline", []int64{0, 2, 0, 2}, nil, StatusSkipped, 0},
		{"rare baseline", []int64{2, 1, 1, 1, 1, 1, 1, 1, 1, 1}, nil, StatusSkipped, 0},
		{"opposite candidate majority", []int64{0, 2, 2}, []int64{0, 0, 2}, StatusUnchanged, 1},
		{"split candidate", []int64{2, 2, 2, 1}, []int64{0, 2, 0, 2}, StatusUnchanged, 2},
		{"matching candidate majority", []int64{0, 2, 2}, []int64{0, 2, 2}, StatusReduced, 2},
		{"passing minority", []int64{2, 1, 2}, []int64{1, 2, 2}, StatusReduced, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			var trial atomic.Int32
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/setup" {
					trial.Add(1)
				}
				if r.URL.Path == "/state" {
					index := int(trial.Load()) - 1
					values := test.baseline
					if index >= len(values) {
						index -= len(values)
						values = test.candidate
					}
					return response(http.StatusOK, fmt.Sprintf(`{"stock":%d}`, values[index%len(values)])), nil
				}
				return response(http.StatusOK, ""), nil
			})}
			scenario := scenarioWithSetup(3, 3)
			scenario.Invariant.JSONInteger = &engine.JSONIntegerInvariant{Name: "one effect", Path: []string{"stock"}, Equals: new(int64(1))}
			result, err := Reduce(context.Background(), client, scenario, len(test.baseline))
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != test.status || result.Baseline.Status != engine.TrialStatusViolated {
				t.Fatalf("incorrect reduction status or hidden baseline violation: %+v", result)
			}
			if test.status == StatusSkipped {
				if len(result.Candidates) != 0 || result.SelectedTrials != nil || trial.Load() != int32(len(test.baseline)) {
					t.Fatalf("search started without majority: %+v", result)
				}
			} else {
				for _, candidate := range result.Candidates {
					if candidate.MatchingViolations != test.matching {
						t.Fatalf("incorrect matching count: %+v", candidate)
					}
				}
			}
		})
	}
}

func TestReduceMatchesHistoryFailure(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return response(http.StatusCreated, ""), nil
	})}
	scenario := scenarioWithSetup(4, 4)
	scenario.Observation = nil
	scenario.Invariant = engine.Invariant{MaximumSuccessfulAttempts: &engine.MaximumSuccessfulAttemptsInvariant{Name: "one success", Maximum: 1}}
	result, err := Reduce(context.Background(), client, scenario, 3)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusReduced || result.FailureDirection != FailureAbove || result.Candidates[0].MatchingViolations != 3 || !result.MatchesFailure(result.SelectedTrials.Trials[0]) {
		t.Fatalf("response-count failure did not reduce: %+v", result)
	}
}
