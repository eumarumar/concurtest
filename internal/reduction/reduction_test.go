package reduction

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eumarumar/concurtest/internal/engine"
)

func TestReduceSelectsFirstQualifyingCandidateInStableOrder(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	operations := 0
	setups := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		switch request.URL.Path {
		case "/setup":
			operations = 0
			setups++
			return response(http.StatusNoContent, ""), nil
		case "/operation":
			operations++
			return response(http.StatusCreated, ""), nil
		case "/state":
			stock := "0"
			if operations >= 3 {
				stock = "-1"
			}
			return response(http.StatusOK, `{"stock":`+stock+`}`), nil
		default:
			return response(http.StatusNotFound, ""), nil
		}
	})}

	result, err := Reduce(context.Background(), client, scenarioWithSetup(4, 4), 3)
	if err != nil {
		t.Fatalf("Reduce() error = %v", err)
	}
	if result.Status != StatusReduced {
		t.Fatalf("status = %q, want %q", result.Status, StatusReduced)
	}
	if result.Selected != (Candidate{Attempts: 3, Concurrency: 2}) {
		t.Errorf("selected = %#v, want 3 attempts and concurrency 2", result.Selected)
	}
	wantCandidates := []Candidate{
		{Attempts: 2, Concurrency: 2},
		{Attempts: 3, Concurrency: 2},
	}
	if len(result.Candidates) != len(wantCandidates) {
		t.Fatalf("candidate count = %d, want %d", len(result.Candidates), len(wantCandidates))
	}
	for index, want := range wantCandidates {
		candidate := result.Candidates[index]
		if candidate.Candidate != want {
			t.Errorf("candidate %d = %#v, want %#v", index+1, candidate.Candidate, want)
		}
		if candidate.Summary.Completed != 3 {
			t.Errorf("candidate %d completed = %d, want 3", index+1, candidate.Summary.Completed)
		}
	}
	if result.Candidates[0].Accepted || result.Candidates[0].Trials != nil {
		t.Errorf("rejected candidate retained full evidence: %#v", result.Candidates[0])
	}
	if !result.Candidates[1].Accepted || result.Candidates[1].Trials == nil || result.SelectedTrials == nil {
		t.Error("selected candidate did not retain complete evidence")
	}
	if len(result.Baseline.Trials) != 3 || len(result.SelectedTrials.Trials) != 3 {
		t.Error("baseline or selected trial evidence is incomplete")
	}
	if setups != 9 {
		t.Errorf("setup calls = %d, want 9 across baseline and candidates", setups)
	}
	if result.StartedAt.IsZero() || result.CompletedAt.IsZero() || result.Duration() < 0 {
		t.Errorf("invalid timing: %#v", result)
	}
}

func TestReduceSkipsSearchWithoutCleanMajority(t *testing.T) {
	t.Parallel()

	var trial atomic.Int32
	var operations atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/setup":
			operations.Store(0)
			trial.Add(1)
			return response(http.StatusNoContent, ""), nil
		case "/operation":
			operations.Add(1)
			return response(http.StatusCreated, ""), nil
		case "/state":
			stock := "0"
			if trial.Load() == 1 {
				stock = "-1"
			}
			return response(http.StatusOK, `{"stock":`+stock+`}`), nil
		default:
			return response(http.StatusNotFound, ""), nil
		}
	})}

	result, err := Reduce(context.Background(), client, scenarioWithSetup(4, 4), 3)
	if err != nil {
		t.Fatalf("Reduce() error = %v", err)
	}
	if result.Status != StatusSkipped || len(result.Candidates) != 0 || result.SelectedTrials != nil {
		t.Errorf("result = %#v, want skipped baseline only", result)
	}
	if trial.Load() != 3 {
		t.Errorf("trials started = %d, want baseline 3 only", trial.Load())
	}
}

func TestReduceContinuesAfterUncleanCandidate(t *testing.T) {
	t.Parallel()

	var setup atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/setup":
			setup.Add(1)
			return response(http.StatusNoContent, ""), nil
		case "/operation":
			return response(http.StatusCreated, ""), nil
		case "/state":
			if current := setup.Load(); current >= 4 && current <= 6 {
				return response(http.StatusOK, `{"stock":`), nil
			}
			return response(http.StatusOK, `{"stock":-1}`), nil
		default:
			return response(http.StatusNotFound, ""), nil
		}
	})}

	result, err := Reduce(context.Background(), client, scenarioWithSetup(3, 3), 3)
	if err != nil {
		t.Fatalf("Reduce() error = %v", err)
	}
	if result.Status != StatusReduced || len(result.Candidates) != 2 {
		t.Fatalf("status/candidates = %q/%d, want reduced/2", result.Status, len(result.Candidates))
	}
	if result.Candidates[0].Summary.Errored != 3 || result.Candidates[0].Trials != nil {
		t.Errorf("unclean candidate = %#v, want summary-only errors", result.Candidates[0])
	}
	if result.Selected != (Candidate{Attempts: 3, Concurrency: 2}) {
		t.Errorf("selected = %#v, want later clean candidate", result.Selected)
	}
}

func TestReduceReportsUnchangedAndLimitedSearches(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		limit     int
		want      Status
		wantCount int
	}{
		{name: "complete", limit: MaxCandidates, want: StatusUnchanged, wantCount: 5},
		{name: "limited", limit: 2, want: StatusLimited, wantCount: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var trial atomic.Int32
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				switch request.URL.Path {
				case "/setup":
					trial.Add(1)
					return response(http.StatusNoContent, ""), nil
				case "/operation":
					return response(http.StatusCreated, ""), nil
				case "/state":
					stock := "0"
					if trial.Load() <= 3 {
						stock = "-1"
					}
					return response(http.StatusOK, `{"stock":`+stock+`}`), nil
				default:
					return response(http.StatusNotFound, ""), nil
				}
			})}

			result, err := reduce(context.Background(), client, scenarioWithSetup(4, 4), 3, test.limit)
			if err != nil {
				t.Fatalf("reduce() error = %v", err)
			}
			if result.Status != test.want || len(result.Candidates) != test.wantCount {
				t.Errorf("status/count = %q/%d, want %q/%d", result.Status, len(result.Candidates), test.want, test.wantCount)
			}
			if result.Selected != (Candidate{Attempts: 4, Concurrency: 4}) || result.SelectedTrials == nil {
				t.Errorf("selected = %#v, want original baseline", result.Selected)
			}
		})
	}
}

func TestReduceStopsOnCancellationAndPreservesActiveCandidate(t *testing.T) {
	t.Parallel()

	operationStarted := make(chan struct{})
	var signal sync.Once
	var setupCalls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/setup":
			setupCalls.Add(1)
			return response(http.StatusNoContent, ""), nil
		case "/operation":
			if setupCalls.Load() > 3 {
				signal.Do(func() { close(operationStarted) })
				<-request.Context().Done()
				return nil, request.Context().Err()
			}
			return response(http.StatusCreated, ""), nil
		case "/state":
			return response(http.StatusOK, `{"stock":-1}`), nil
		default:
			return response(http.StatusNotFound, ""), nil
		}
	})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct {
		result Result
		err    error
	}, 1)
	go func() {
		result, err := Reduce(ctx, client, scenarioWithSetup(3, 3), 3)
		done <- struct {
			result Result
			err    error
		}{result: result, err: err}
	}()

	select {
	case <-operationStarted:
		cancel()
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("candidate operation did not start")
	}

	select {
	case response := <-done:
		if !errors.Is(response.err, context.Canceled) {
			t.Fatalf("Reduce() error = %v, want context.Canceled", response.err)
		}
		if len(response.result.Candidates) != 1 {
			t.Fatalf("candidate count = %d, want 1", len(response.result.Candidates))
		}
		candidate := response.result.Candidates[0]
		if candidate.Candidate != (Candidate{Attempts: 2, Concurrency: 2}) || candidate.Trials == nil || candidate.Err == nil {
			t.Errorf("active candidate evidence = %#v", candidate)
		}
		if len(candidate.Trials.Trials) != 1 {
			t.Errorf("recorded active trials = %d, want 1", len(candidate.Trials.Trials))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Reduce() did not stop after cancellation")
	}
	if setupCalls.Load() != 4 {
		t.Errorf("setup calls = %d, want baseline 3 and active candidate 1", setupCalls.Load())
	}
}

func TestReduceCancellationDuringBaselineStartsNoCandidate(t *testing.T) {
	t.Parallel()

	requestStarted := make(chan struct{})
	var signal sync.Once
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/setup" {
			signal.Do(func() { close(requestStarted) })
			<-request.Context().Done()
			return nil, request.Context().Err()
		}
		return response(http.StatusOK, ""), nil
	})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct {
		result Result
		err    error
	}, 1)
	go func() {
		result, err := Reduce(ctx, client, scenarioWithSetup(3, 3), 3)
		done <- struct {
			result Result
			err    error
		}{result: result, err: err}
	}()

	select {
	case <-requestStarted:
		cancel()
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("baseline setup did not start")
	}

	select {
	case response := <-done:
		if !errors.Is(response.err, context.Canceled) {
			t.Fatalf("Reduce() error = %v, want context.Canceled", response.err)
		}
		if len(response.result.Baseline.Trials) != 1 || len(response.result.Candidates) != 0 {
			t.Errorf("result = %#v, want one partial baseline trial and no candidate", response.result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Reduce() did not stop after baseline cancellation")
	}
}

func TestReduceValidatesBeforeRequests(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return response(http.StatusOK, ""), nil
	})}
	tests := []struct {
		name     string
		scenario engine.Scenario
		trials   int
	}{
		{name: "missing setup", scenario: reductionScenario(2, 2), trials: 3},
		{name: "too few trials", scenario: scenarioWithSetup(2, 2), trials: 2},
		{name: "one attempt", scenario: scenarioWithSetup(1, 1), trials: 3},
		{name: "one worker", scenario: scenarioWithSetup(2, 1), trials: 3},
		{name: "concurrency exceeds attempts", scenario: scenarioWithSetup(2, 3), trials: 3},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Reduce(context.Background(), client, test.scenario, test.trials); err == nil {
				t.Fatal("Reduce() error = nil, want validation error")
			}
		})
	}
	if calls.Load() != 0 {
		t.Errorf("HTTP calls = %d, want 0", calls.Load())
	}
}

func TestReproductionQualificationRequiresCleanStrictMajority(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		summary TrialSummary
		want    bool
	}{
		{name: "two of three", summary: TrialSummary{Requested: 3, Completed: 3, Violated: 2, Passed: 1}, want: true},
		{name: "three of four", summary: TrialSummary{Requested: 4, Completed: 4, Violated: 3, Passed: 1}, want: true},
		{name: "tie", summary: TrialSummary{Requested: 4, Completed: 4, Violated: 2, Passed: 2}},
		{name: "incomplete", summary: TrialSummary{Requested: 3, Completed: 2, Violated: 2}},
		{name: "inconclusive", summary: TrialSummary{Requested: 3, Completed: 3, Violated: 2, Inconclusive: 1}},
		{name: "errored", summary: TrialSummary{Requested: 3, Completed: 3, Violated: 2, Errored: 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := qualifies(test.summary); got != test.want {
				t.Errorf("qualifies() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestSmallerCandidatesUsesAttemptsFirstOrderingAndLimit(t *testing.T) {
	t.Parallel()

	candidates, limited := smallerCandidates(reductionScenario(5, 4), 100)
	want := []Candidate{
		{Attempts: 2, Concurrency: 2},
		{Attempts: 3, Concurrency: 2},
		{Attempts: 3, Concurrency: 3},
		{Attempts: 4, Concurrency: 2},
		{Attempts: 4, Concurrency: 3},
		{Attempts: 4, Concurrency: 4},
		{Attempts: 5, Concurrency: 2},
		{Attempts: 5, Concurrency: 3},
	}
	if limited || len(candidates) != len(want) {
		t.Fatalf("candidate count/limit = %d/%t, want %d/false", len(candidates), limited, len(want))
	}
	for index := range want {
		if candidates[index] != want[index] {
			t.Errorf("candidate %d = %#v, want %#v", index+1, candidates[index], want[index])
		}
	}

	candidates, limited = smallerCandidates(reductionScenario(5, 4), 3)
	if !limited || len(candidates) != 3 {
		t.Errorf("limited candidates = %d/%t, want 3/true", len(candidates), limited)
	}
}

func reductionScenario(attempts, concurrency int) engine.Scenario {
	return engine.Scenario{
		Operation: engine.Operation{
			Name:    "operation",
			Request: engine.HTTPRequest{Method: http.MethodPost, URL: "http://example.test/operation"},
		},
		Attempts:    attempts,
		Concurrency: concurrency,
		Observation: &engine.HTTPRequest{Method: http.MethodGet, URL: "http://example.test/state"},
		Invariant: engine.Invariant{
			JSONIntegerMinimum: &engine.JSONIntegerMinimumInvariant{
				Name:    "stock must be non-negative",
				Path:    []string{"stock"},
				Minimum: 0,
			},
		},
	}
}

func scenarioWithSetup(attempts, concurrency int) engine.Scenario {
	scenario := reductionScenario(attempts, concurrency)
	setup := engine.HTTPRequest{Method: http.MethodPost, URL: "http://example.test/setup"}
	scenario.Setup = &setup
	return scenario
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
