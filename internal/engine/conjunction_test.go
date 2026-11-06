package engine_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/eumarumar/concurtest/internal/engine"
)

func integerCheck(name, path string, equals int64) engine.Invariant {
	return engine.Invariant{JSONInteger: &engine.JSONIntegerInvariant{Name: name, Path: []string{path}, Equals: new(equals)}}
}

func conjunctionScenario(checks ...engine.Invariant) engine.Scenario {
	scenario := scenarioWithoutSetup()
	scenario.Attempts, scenario.Concurrency = 2, 2
	scenario.Invariant = engine.Invariant{All: checks}
	return scenario
}

func TestConjunctionSharesHistoryBaselineAndFinalObservation(t *testing.T) {
	t.Parallel()
	x, y := integerCheck("X change", "x", 0), integerCheck("Y change", "y", 0)
	x.JSONInteger.Equals, x.JSONInteger.Change = nil, new(int64(1))
	y.JSONInteger.Equals, y.JSONInteger.Change = nil, new(int64(2))
	scenario := conjunctionScenario(x, y, engine.Invariant{MaximumSuccessfulAttempts: &engine.MaximumSuccessfulAttemptsInvariant{Name: "Two replies", Maximum: 2}})
	scenario.Setup = []engine.SetupStep{{Request: engine.HTTPRequest{Method: http.MethodPost, URL: "http://example.test/setup"}}}
	scenario.Readiness, scenario.Completion = pendingCondition("/ready"), pendingCondition("/done")
	var completed, operations, reads, setups, waits atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/setup":
			completed.Store(0)
			setups.Add(1)
		case "/purchase":
			completed.Add(1)
			operations.Add(1)
		case "/ready", "/done":
			waits.Add(1)
			return testHTTPResponse(200, `{"pending":0}`), nil
		case "/state":
			reads.Add(1)
			if completed.Load() == 0 {
				return testHTTPResponse(200, `{"x":10,"y":20}`), nil
			}
			if completed.Load() != 2 {
				t.Error("observation ran before all operations completed")
			}
			return testHTTPResponse(200, `{"x":11,"y":22}`), nil
		}
		return testHTTPResponse(200, ""), nil
	})}
	result, err := engine.RunTrials(context.Background(), client, scenario, 3)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != engine.TrialStatusPassed || operations.Load() != 6 || reads.Load() != 6 || setups.Load() != 3 || waits.Load() != 6 {
		t.Fatalf("unexpected traffic or result: %+v operations=%d reads=%d", result, operations.Load(), reads.Load())
	}
	for _, trial := range result.Trials {
		e := trial.Run.Evaluation
		if e == nil || !e.Complete() || e.Violated || len(e.All) != 3 || trial.Run.BaselineObservation == nil {
			t.Fatalf("missing shared evidence: %+v", trial)
		}
		for index, want := range []struct{ baseline, change int64 }{{10, 1}, {20, 2}} {
			v := e.All[index].Evaluation.JSONInteger
			if *v.Baseline != want.baseline || *v.Change != want.change {
				t.Fatalf("baseline/change mixed between checks: %+v", v)
			}
		}
		if len(e.All[2].Evaluation.MaximumSuccessfulAttempts.SuccessfulAttemptIDs) != 2 {
			t.Fatal("history was not shared")
		}
	}
	*x.JSONInteger.Change = 99
	if *result.Trials[0].Run.Evaluation.All[0].Evaluation.JSONInteger.Invariant.Change != 1 {
		t.Fatal("check evidence shares caller constraints")
	}
}

func TestConjunctionRetainsErrorsAndLaterViolations(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, body       string
		status           int
		historyViolation bool
		want             engine.TrialStatus
	}{
		{"later state violation", `{"count":2}`, 200, false, engine.TrialStatusViolated},
		{"no violation with unreadable sibling", `{"count":0}`, 200, false, engine.TrialStatusErrored},
		{"all scalars unreadable", `{"count":"bad"}`, 200, false, engine.TrialStatusErrored},
		{"bad HTTP with history violation", `{}`, 500, true, engine.TrialStatusViolated},
		{"truncated with history violation", strings.Repeat(" ", engine.MaxHTTPBodyBytes) + `{"count":2}`, 200, true, engine.TrialStatusViolated},
	} {
		t.Run(test.name, func(t *testing.T) {
			limit := 2
			if test.historyViolation {
				limit = 1
			}
			scenario := conjunctionScenario(integerCheck("Unreadable", "missing", 0), integerCheck("Later", "count", 0), engine.Invariant{MaximumSuccessfulAttempts: &engine.MaximumSuccessfulAttemptsInvariant{Name: "Replies", Maximum: limit}})
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/state" {
					return testHTTPResponse(test.status, test.body), nil
				}
				return testHTTPResponse(200, ""), nil
			})}
			result, err := engine.RunTrials(context.Background(), client, scenario, 1)
			if err != nil {
				t.Fatal(err)
			}
			trial := result.Trials[0]
			if trial.Status != test.want || result.Status != test.want || trial.Err == nil || trial.Run.Evaluation == nil || trial.Run.Evaluation.Complete() {
				t.Fatalf("incomplete check became a pass or hid violation: %+v", trial)
			}
			if trial.Run.Evaluation.All[0].Err == nil || trial.Run.Evaluation.All[0].Evaluation != nil {
				t.Fatal("unknown scalar was fabricated")
			}
			if test.want == engine.TrialStatusViolated && !trial.Run.Evaluation.Violated {
				t.Fatal("known violation lost")
			}
		})
	}
}

func TestConjunctionBaselineFailurePreventsOperations(t *testing.T) {
	t.Parallel()
	for _, body := range []string{`{"x":0}`, `{"x":0,"y":null}`, `{"x":0,"y":1.5}`} {
		x, y := integerCheck("X", "x", 0), integerCheck("Y", "y", 0)
		x.JSONInteger.Equals, x.JSONInteger.Change = nil, new(int64(1))
		y.JSONInteger.Equals, y.JSONInteger.Change = nil, new(int64(1))
		scenario := conjunctionScenario(x, y)
		calls := 0
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.URL.Path != "/state" {
				t.Errorf("unexpected operation %s", r.URL)
			}
			return testHTTPResponse(200, body), nil
		})}
		result, err := engine.Run(context.Background(), client, scenario)
		if err == nil || result.BaselineObservation == nil || len(result.History.Attempts) != 0 || calls != 1 {
			t.Fatalf("invalid baseline allowed work: %+v error=%v calls=%d", result, err, calls)
		}
	}
}

func TestConjunctionChangeOverflowDoesNotHideAnotherFailure(t *testing.T) {
	t.Parallel()
	x := integerCheck("Overflow", "x", 0)
	x.JSONInteger.Equals, x.JSONInteger.Change = nil, new(int64(1))
	scenario := conjunctionScenario(x, integerCheck("Ledger", "ledger", 0))
	var operations atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/purchase" {
			operations.Add(1)
			return testHTTPResponse(200, ""), nil
		}
		if operations.Load() == 0 {
			return testHTTPResponse(200, fmt.Sprintf(`{"x":%d,"ledger":0}`, int64(math.MinInt64))), nil
		}
		return testHTTPResponse(200, fmt.Sprintf(`{"x":%d,"ledger":1}`, int64(math.MaxInt64))), nil
	})}
	result, err := engine.Run(context.Background(), client, scenario)
	if err == nil || result.Outcome != engine.RunOutcomeViolated || result.Evaluation.All[0].Err == nil || result.Evaluation.All[1].Evaluation.JSONInteger.Observed != 1 {
		t.Fatalf("overflow lost error or sibling failure: %+v %v", result, err)
	}
}

func TestConjunctionCompletionFailureAndCancellation(t *testing.T) {
	t.Parallel()
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelled), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			scenario := conjunctionScenario(integerCheck("State", "count", 0), engine.Invariant{MaximumSuccessfulAttempts: &engine.MaximumSuccessfulAttemptsInvariant{Name: "Replies", Maximum: 1}})
			scenario.Completion = pendingCondition("/done")
			scenario.Completion.MaxPolls = 1
			var observations, operations atomic.Int32
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/done" {
					if cancelled {
						cancel()
						return nil, context.Canceled
					}
					return testHTTPResponse(200, `{"pending":1}`), nil
				}
				if r.URL.Path == "/state" {
					observations.Add(1)
				} else {
					operations.Add(1)
				}
				return testHTTPResponse(200, `{"count":0}`), nil
			})}
			result, err := engine.RunTrials(ctx, client, scenario, 3)
			if cancelled && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation identity lost: %v", err)
			}
			if !cancelled && err != nil {
				t.Fatal(err)
			}
			if len(result.Trials) != 1 || operations.Load() != 2 || observations.Load() != 0 || !result.WaitFailed() {
				t.Fatalf("unverified completion allowed further work: %+v", result)
			}
			e := result.Trials[0].Run.Evaluation
			if !e.Violated || e.Complete() || e.All[0].Evaluation != nil || e.All[0].Err != nil || !e.All[1].Evaluation.Violated {
				t.Fatalf("pending and completed checks confused: %+v", e)
			}
		})
	}
}

func TestConjunctionValidationPreventsTraffic(t *testing.T) {
	t.Parallel()
	a, b := integerCheck("A", "a", 0), integerCheck("B", "b", 0)
	for _, group := range []engine.Invariant{
		{All: []engine.Invariant{}}, {All: []engine.Invariant{a}}, {All: make([]engine.Invariant, engine.MaxInvariantChecks+1)},
		{All: []engine.Invariant{a, a}}, {All: []engine.Invariant{a, {All: []engine.Invariant{a, b}}}},
		{All: []engine.Invariant{a, b}, JSONInteger: a.JSONInteger}, {All: []engine.Invariant{a, {}}},
	} {
		calls := 0
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return testHTTPResponse(200, ""), nil })}
		scenario := conjunctionScenario(a, b)
		scenario.Invariant = group
		if _, err := engine.Run(context.Background(), client, scenario); err == nil || calls != 0 {
			t.Fatalf("invalid group sent traffic: %+v calls=%d error=%v", group, calls, err)
		}
	}
	scenario := conjunctionScenario(a, b)
	scenario.Observation = nil
	if _, err := engine.Run(context.Background(), &http.Client{}, scenario); err == nil {
		t.Fatal("state group allowed missing observer")
	}
}

func TestConjunctionHistoryOnlyAndOutcomeDefaults(t *testing.T) {
	t.Parallel()
	scenario := conjunctionScenario(
		engine.Invariant{MaximumSuccessfulAttempts: &engine.MaximumSuccessfulAttemptsInvariant{Name: "Conflicts", Maximum: 2, SuccessfulStatusCodes: []int{409}}},
		engine.Invariant{MaximumSuccessfulAttempts: &engine.MaximumSuccessfulAttemptsInvariant{Name: "All 2xx", Maximum: 2}},
	)
	scenario.Observation = nil
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { calls.Add(1); return testHTTPResponse(409, ""), nil })}
	result, err := engine.Run(context.Background(), client, scenario)
	if err != nil || result.Outcome != engine.RunOutcomeInconclusive || !result.Evaluation.Complete() || calls.Load() != 2 || result.Observation != nil {
		t.Fatalf("group silently inherited one status filter: %+v error=%v", result, err)
	}
	scenario.Outcomes.AcceptedStatusCodes = []int{409}
	result, err = engine.Run(context.Background(), client, scenario)
	if err != nil || result.Outcome != engine.RunOutcomePassed || !reflect.DeepEqual(result.Outcomes.Policy.AcceptedStatusCodes, []int{409}) {
		t.Fatalf("explicit outcomes ignored: %+v error=%v", result, err)
	}
}
