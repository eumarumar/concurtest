package engine_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eumarumar/concurtest/internal/engine"
)

func TestRunTrialsRunsSequentialIndependentTrials(t *testing.T) {
	t.Parallel()

	const (
		trialCount  = 4
		attempts    = 4
		concurrency = 2
	)
	var (
		mu               sync.Mutex
		currentTrial     int
		trialOpen        bool
		activeOperations int
		maxActive        int
		setupCalls       int
		observationCalls int
	)
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		switch request.URL.Path {
		case "/setup":
			if trialOpen {
				t.Errorf("trial %d setup started before the prior observation", currentTrial+1)
			}
			currentTrial++
			setupCalls++
			trialOpen = true
			mu.Unlock()
			return testHTTPResponse(http.StatusNoContent, ""), nil
		case "/purchase":
			if !trialOpen {
				t.Error("operation ran outside an active trial")
			}
			activeOperations++
			if activeOperations > maxActive {
				maxActive = activeOperations
			}
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			activeOperations--
			mu.Unlock()
			return testHTTPResponse(http.StatusCreated, ""), nil
		case "/state":
			if activeOperations != 0 {
				t.Errorf("trial %d observation started with %d active operations", currentTrial, activeOperations)
			}
			observationCalls++
			trialOpen = false
			mu.Unlock()
			return testHTTPResponse(http.StatusOK, `{"stock":0}`), nil
		default:
			mu.Unlock()
			return testHTTPResponse(http.StatusNotFound, ""), nil
		}
	})}
	setup := engine.HTTPRequest{Method: http.MethodPost, URL: "http://example.test/setup"}
	scenario := scenarioWithoutSetup()
	scenario.Setup = &setup
	scenario.Operation.Request.URL = "http://example.test/purchase"
	scenario.Observation.URL = "http://example.test/state"
	scenario.Attempts = attempts
	scenario.Concurrency = concurrency

	result, err := engine.RunTrials(context.Background(), client, scenario, trialCount)
	if err != nil {
		t.Fatalf("RunTrials() error = %v", err)
	}
	if result.Requested != trialCount || len(result.Trials) != trialCount {
		t.Fatalf("trial counts = requested %d recorded %d, want %d", result.Requested, len(result.Trials), trialCount)
	}
	if result.Status != engine.TrialStatusPassed {
		t.Errorf("aggregate status = %q, want %q", result.Status, engine.TrialStatusPassed)
	}
	for index, trial := range result.Trials {
		if trial.Number != index+1 || trial.Status != engine.TrialStatusPassed || trial.Err != nil {
			t.Errorf("trial %d = %#v", index+1, trial)
		}
		if trial.Run.Setup == nil || len(trial.Run.History.Attempts) != attempts || trial.Run.Evaluation == nil {
			t.Errorf("trial %d did not retain complete evidence: %#v", index+1, trial.Run)
		}
	}
	if setupCalls != trialCount || observationCalls != trialCount {
		t.Errorf("stage calls = setup %d observation %d, want %d each", setupCalls, observationCalls, trialCount)
	}
	if maxActive != concurrency {
		t.Errorf("maximum active operations = %d, want %d", maxActive, concurrency)
	}
	if result.StartedAt.IsZero() || result.CompletedAt.IsZero() || result.Duration() < 0 {
		t.Errorf("invalid timing: start=%v completed=%v duration=%v", result.StartedAt, result.CompletedAt, result.Duration())
	}
}

func TestRunTrialsContinuesAfterErrorsAndClassifiesAggregate(t *testing.T) {
	t.Parallel()

	var trial atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/setup":
			number := trial.Add(1)
			if number == 4 {
				return testHTTPResponse(http.StatusServiceUnavailable, "setup unavailable"), nil
			}
			return testHTTPResponse(http.StatusNoContent, ""), nil
		case "/purchase":
			if trial.Load() == 3 {
				return nil, errors.New("operation transport failed")
			}
			return testHTTPResponse(http.StatusCreated, ""), nil
		case "/state":
			stock := 0
			if trial.Load() == 2 {
				stock = -1
			}
			return testHTTPResponse(http.StatusOK, fmt.Sprintf(`{"stock":%d}`, stock)), nil
		default:
			return testHTTPResponse(http.StatusNotFound, ""), nil
		}
	})}
	setup := engine.HTTPRequest{Method: http.MethodPost, URL: "http://example.test/setup"}
	scenario := scenarioWithoutSetup()
	scenario.Setup = &setup

	result, err := engine.RunTrials(context.Background(), client, scenario, 4)
	if err != nil {
		t.Fatalf("RunTrials() error = %v", err)
	}
	want := []engine.TrialStatus{
		engine.TrialStatusPassed,
		engine.TrialStatusViolated,
		engine.TrialStatusInconclusive,
		engine.TrialStatusErrored,
	}
	for index, status := range want {
		if result.Trials[index].Status != status {
			t.Errorf("trial %d status = %q, want %q", index+1, result.Trials[index].Status, status)
		}
	}
	if result.Trials[1].Run.Evaluation == nil || !result.Trials[1].Run.Evaluation.Violated {
		t.Error("violated trial did not retain its evaluation")
	}
	if result.Trials[3].Err == nil || result.Trials[3].Run.Setup == nil {
		t.Error("errored trial did not retain its error and setup evidence")
	}
	if result.Status != engine.TrialStatusViolated {
		t.Errorf("aggregate status = %q, want violation precedence", result.Status)
	}
}

func TestRunTrialsAggregateStatusPrecedence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		observations []string
		operationErr map[int]bool
		want         engine.TrialStatus
	}{
		{name: "all passed", observations: []string{`{"stock":0}`, `{"stock":1}`}, want: engine.TrialStatusPassed},
		{name: "inconclusive over passed", observations: []string{`{"stock":0}`, `{"stock":0}`}, operationErr: map[int]bool{2: true}, want: engine.TrialStatusInconclusive},
		{name: "error over inconclusive", observations: []string{`{"stock":0}`, `not-json`}, operationErr: map[int]bool{1: true}, want: engine.TrialStatusErrored},
		{name: "violation over error", observations: []string{`not-json`, `{"stock":-1}`}, want: engine.TrialStatusViolated},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var current atomic.Int32
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				switch request.URL.Path {
				case "/setup":
					current.Add(1)
					return testHTTPResponse(http.StatusNoContent, ""), nil
				case "/purchase":
					if test.operationErr[int(current.Load())] {
						return nil, errors.New("operation failed")
					}
					return testHTTPResponse(http.StatusCreated, ""), nil
				case "/state":
					return testHTTPResponse(http.StatusOK, test.observations[int(current.Load())-1]), nil
				default:
					return testHTTPResponse(http.StatusNotFound, ""), nil
				}
			})}
			setup := engine.HTTPRequest{Method: http.MethodPost, URL: "http://example.test/setup"}
			scenario := scenarioWithoutSetup()
			scenario.Setup = &setup
			result, err := engine.RunTrials(context.Background(), client, scenario, len(test.observations))
			if err != nil {
				t.Fatalf("RunTrials() error = %v", err)
			}
			if result.Status != test.want {
				t.Errorf("status = %q, want %q", result.Status, test.want)
			}
		})
	}
}

func TestRunTrialsValidatesAllInputsBeforeRequests(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return testHTTPResponse(http.StatusOK, ""), nil
	})}
	tests := []struct {
		name     string
		ctx      context.Context
		client   *http.Client
		scenario engine.Scenario
		count    int
	}{
		{name: "nil context", client: client, scenario: scenarioWithoutSetup(), count: 1},
		{name: "nil client", ctx: context.Background(), scenario: scenarioWithoutSetup(), count: 1},
		{name: "invalid scenario", ctx: context.Background(), client: client, scenario: engine.Scenario{}, count: 1},
		{name: "zero trials", ctx: context.Background(), client: client, scenario: scenarioWithoutSetup(), count: 0},
		{name: "negative trials", ctx: context.Background(), client: client, scenario: scenarioWithoutSetup(), count: -1},
		{name: "too many trials", ctx: context.Background(), client: client, scenario: scenarioWithoutSetup(), count: engine.MaxTrials + 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := engine.RunTrials(test.ctx, test.client, test.scenario, test.count); err == nil {
				t.Fatal("RunTrials() error = nil, want validation error")
			}
		})
	}
	if calls.Load() != 0 {
		t.Errorf("HTTP calls = %d, want 0", calls.Load())
	}
}

func TestRunTrialsStopsOnParentCancellationAndPreservesActiveTrial(t *testing.T) {
	t.Parallel()

	operationStarted := make(chan struct{})
	var once sync.Once
	var setupCalls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/setup":
			setupCalls.Add(1)
			return testHTTPResponse(http.StatusNoContent, ""), nil
		case "/purchase":
			once.Do(func() { close(operationStarted) })
			<-request.Context().Done()
			return nil, request.Context().Err()
		default:
			return testHTTPResponse(http.StatusOK, `{"stock":0}`), nil
		}
	})}
	setup := engine.HTTPRequest{Method: http.MethodPost, URL: "http://example.test/setup"}
	scenario := scenarioWithoutSetup()
	scenario.Setup = &setup
	scenario.Attempts = 2
	scenario.Concurrency = 1
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct {
		result engine.TrialsResult
		err    error
	}, 1)
	go func() {
		result, err := engine.RunTrials(ctx, client, scenario, 3)
		done <- struct {
			result engine.TrialsResult
			err    error
		}{result: result, err: err}
	}()

	select {
	case <-operationStarted:
		cancel()
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("operation did not start")
	}

	select {
	case response := <-done:
		if !errors.Is(response.err, context.Canceled) {
			t.Fatalf("RunTrials() error = %v, want context.Canceled", response.err)
		}
		if response.result.Requested != 3 || len(response.result.Trials) != 1 {
			t.Fatalf("trial counts = requested %d recorded %d, want 3 and 1", response.result.Requested, len(response.result.Trials))
		}
		trial := response.result.Trials[0]
		if trial.Number != 1 || trial.Status != engine.TrialStatusErrored || trial.Err == nil || trial.Run.Setup == nil || len(trial.Run.History.Attempts) != 2 {
			t.Errorf("active trial evidence = %#v", trial)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunTrials() did not stop after cancellation")
	}
	if setupCalls.Load() != 1 {
		t.Errorf("setup calls = %d, want no later trial after cancellation", setupCalls.Load())
	}
}

func TestRunTrialsCanceledBeforeFirstTrial(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := engine.RunTrials(ctx, http.DefaultClient, scenarioWithoutSetup(), 3)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunTrials() error = %v, want context.Canceled", err)
	}
	if result.Requested != 3 || len(result.Trials) != 0 {
		t.Errorf("trial counts = requested %d recorded %d, want 3 and 0", result.Requested, len(result.Trials))
	}
}

func TestRunTrialsCancellationBetweenTrialsStartsNoLaterTrial(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	var setupCalls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/setup":
			setupCalls.Add(1)
			return testHTTPResponse(http.StatusNoContent, ""), nil
		case "/purchase":
			return testHTTPResponse(http.StatusCreated, ""), nil
		case "/state":
			cancel()
			return testHTTPResponse(http.StatusOK, `{"stock":0}`), nil
		default:
			return testHTTPResponse(http.StatusNotFound, ""), nil
		}
	})}
	setup := engine.HTTPRequest{Method: http.MethodPost, URL: "http://example.test/setup"}
	scenario := scenarioWithoutSetup()
	scenario.Setup = &setup

	result, err := engine.RunTrials(ctx, client, scenario, 3)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunTrials() error = %v, want context.Canceled", err)
	}
	if len(result.Trials) != 1 || result.Trials[0].Status != engine.TrialStatusPassed {
		t.Errorf("recorded trials = %#v, want one completed pass", result.Trials)
	}
	if setupCalls.Load() != 1 {
		t.Errorf("setup calls = %d, want 1", setupCalls.Load())
	}
}
