package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eumarumar/concurtest/internal/engine"
)

func TestRunExecutesSetupOperationsObservationAndFindsViolation(t *testing.T) {
	t.Parallel()

	var stock atomic.Int64
	var setupComplete atomic.Bool
	var completedOperations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/setup":
			stock.Store(1)
			setupComplete.Store(true)
			writer.WriteHeader(http.StatusNoContent)
		case "/purchase":
			if !setupComplete.Load() {
				t.Error("operation started before setup completed")
			}
			stock.Add(-1)
			completedOperations.Add(1)
			writer.WriteHeader(http.StatusCreated)
		case "/state":
			if completedOperations.Load() != 2 {
				t.Errorf("observation started after %d operations, want 2", completedOperations.Load())
			}
			writer.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(writer).Encode(map[string]int64{"stock": stock.Load()}); err != nil {
				t.Errorf("encode observation: %v", err)
			}
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)

	setup := engine.HTTPRequest{Method: http.MethodPost, URL: server.URL + "/setup"}
	result, err := engine.Run(context.Background(), server.Client(), engine.Scenario{
		Setup: &setup,
		Operation: engine.Operation{
			Name:    "purchase",
			Request: engine.HTTPRequest{Method: http.MethodPost, URL: server.URL + "/purchase"},
		},
		Attempts:    2,
		Concurrency: 2,
		Observation: &engine.HTTPRequest{Method: http.MethodGet, URL: server.URL + "/state"},
		Invariant: engine.Invariant{
			JSONIntegerMinimum: &engine.JSONIntegerMinimumInvariant{
				Name:    "stock must be non-negative",
				Path:    []string{"stock"},
				Minimum: 0,
			},
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.Outcome != engine.RunOutcomeViolated {
		t.Errorf("outcome = %q, want %q", result.Outcome, engine.RunOutcomeViolated)
	}
	if result.Setup == nil || result.Setup.Response == nil || result.Setup.Response.StatusCode != http.StatusNoContent {
		t.Errorf("setup = %#v, want successful setup execution", result.Setup)
	}
	if len(result.History.Attempts) != 2 {
		t.Errorf("history attempt count = %d, want 2", len(result.History.Attempts))
	}
	if result.Observation == nil || result.Observation.Response == nil {
		t.Fatal("observation was not recorded")
	}
	if result.Evaluation == nil {
		t.Fatal("evaluation was not recorded")
	}
	if result.Evaluation.JSONIntegerMinimum == nil ||
		result.Evaluation.JSONIntegerMinimum.Observed != -1 ||
		!result.Evaluation.Violated {
		t.Errorf("evaluation = %#v, want observed -1 violation", result.Evaluation)
	}
	if result.StartedAt.IsZero() || result.CompletedAt.IsZero() || result.Duration() < 0 {
		t.Errorf("invalid run timing: start=%v completion=%v duration=%v", result.StartedAt, result.CompletedAt, result.Duration())
	}
	if result.Setup.StartedAt.Before(result.StartedAt) {
		t.Error("setup started before run")
	}
	if result.Observation.StartedAt.Before(result.History.CompletedAt) {
		t.Error("observation started before concurrent history completed")
	}
	if result.Observation.CompletedAt.After(result.CompletedAt) {
		t.Error("observation completed after run")
	}
}

func TestRunClassifiesPassAndInconclusiveOperationErrors(t *testing.T) {
	t.Parallel()

	operationErr := errors.New("operation transport failed")
	tests := []struct {
		name          string
		observedStock int
		operationErr  error
		wantOutcome   engine.RunOutcome
	}{
		{name: "clean pass", observedStock: 0, wantOutcome: engine.RunOutcomePassed},
		{name: "operation error makes pass inconclusive", observedStock: 0, operationErr: operationErr, wantOutcome: engine.RunOutcomeInconclusive},
		{name: "violation takes precedence over operation error", observedStock: -1, operationErr: operationErr, wantOutcome: engine.RunOutcomeViolated},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				switch request.URL.Path {
				case "/purchase":
					if test.operationErr != nil {
						return nil, test.operationErr
					}
					return testHTTPResponse(http.StatusCreated, "purchased"), nil
				case "/state":
					return testHTTPResponse(http.StatusOK, `{"stock":`+strconv.Itoa(test.observedStock)+`}`), nil
				default:
					return testHTTPResponse(http.StatusNotFound, "not found"), nil
				}
			})}

			result, err := engine.Run(context.Background(), client, scenarioWithoutSetup())
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if result.Setup != nil {
				t.Errorf("setup = %#v, want nil", result.Setup)
			}
			if result.Outcome != test.wantOutcome {
				t.Errorf("outcome = %q, want %q", result.Outcome, test.wantOutcome)
			}
		})
	}
}

func TestRunEvaluatesMaximumSuccessfulAttemptsWithoutObservation(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return testHTTPResponse(http.StatusCreated, "accepted"), nil
		}
		return testHTTPResponse(http.StatusConflict, "rejected"), nil
	})}
	scenario := maximumSuccessfulAttemptsScenario(0)
	scenario.Attempts = 2

	result, err := engine.Run(context.Background(), client, scenario)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("HTTP calls = %d, want only 2 operation calls", calls.Load())
	}
	if result.Observation != nil {
		t.Errorf("observation = %#v, want nil", result.Observation)
	}
	if result.Outcome != engine.RunOutcomeViolated || result.Evaluation == nil ||
		result.Evaluation.MaximumSuccessfulAttempts == nil {
		t.Fatalf("result = %#v, want history violation", result)
	}
	evaluation := result.Evaluation.MaximumSuccessfulAttempts
	if len(evaluation.SuccessfulAttemptIDs) != 1 || evaluation.SuccessfulAttemptIDs[0] != 1 ||
		len(evaluation.OverLimitAttemptIDs) != 1 || evaluation.OverLimitAttemptIDs[0] != 1 {
		t.Errorf("evaluation = %#v, want attempt 1 successful and over limit", evaluation)
	}
}

func TestRunMaximumSuccessfulAttemptsOutcomePrecedence(t *testing.T) {
	t.Parallel()

	operationErr := errors.New("operation unavailable")
	observationErr := errors.New("observation unavailable")
	type operationReply struct {
		status int
		err    error
	}
	tests := []struct {
		name             string
		maximum          int
		operationReplies []operationReply
		observationReply func() (*http.Response, error)
		wantOutcome      engine.RunOutcome
		wantErr          bool
	}{
		{
			name:             "violation over operation error",
			maximum:          0,
			operationReplies: []operationReply{{status: http.StatusCreated}, {err: operationErr}},
			wantOutcome:      engine.RunOutcomeViolated,
		},
		{
			name:             "clean history with operation error is inconclusive",
			maximum:          1,
			operationReplies: []operationReply{{status: http.StatusCreated}, {err: operationErr}},
			wantOutcome:      engine.RunOutcomeInconclusive,
		},
		{
			name:             "violation over optional observation error",
			maximum:          0,
			operationReplies: []operationReply{{status: http.StatusCreated}},
			observationReply: func() (*http.Response, error) { return nil, observationErr },
			wantOutcome:      engine.RunOutcomeViolated,
		},
		{
			name:             "passing history with optional observation error is errored",
			maximum:          1,
			operationReplies: []operationReply{{status: http.StatusCreated}},
			observationReply: func() (*http.Response, error) { return nil, observationErr },
			wantErr:          true,
		},
		{
			name:             "truncated optional observation does not affect pass",
			maximum:          1,
			operationReplies: []operationReply{{status: http.StatusCreated}},
			observationReply: func() (*http.Response, error) {
				return testHTTPResponse(http.StatusOK, strings.Repeat("x", engine.MaxHTTPBodyBytes+1)), nil
			},
			wantOutcome: engine.RunOutcomePassed,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var operationCall atomic.Int32
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path == "/state" {
					return test.observationReply()
				}
				reply := test.operationReplies[int(operationCall.Add(1))-1]
				if reply.err != nil {
					return nil, reply.err
				}
				return testHTTPResponse(reply.status, "operation"), nil
			})}
			scenario := maximumSuccessfulAttemptsScenario(test.maximum)
			scenario.Attempts = len(test.operationReplies)
			if test.observationReply != nil {
				scenario.Observation = &engine.HTTPRequest{Method: http.MethodGet, URL: "http://example.test/state"}
			}

			result, err := engine.Run(context.Background(), client, scenario)
			if test.wantErr {
				if err == nil {
					t.Fatal("Run() error = nil, want observation error")
				}
				if result.Evaluation == nil || result.Observation == nil {
					t.Errorf("partial evidence was not retained: %#v", result)
				}
				return
			}
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if result.Outcome != test.wantOutcome {
				t.Errorf("outcome = %q, want %q", result.Outcome, test.wantOutcome)
			}
		})
	}
}

func TestRunStopsAfterSetupFailure(t *testing.T) {
	t.Parallel()

	setupErr := errors.New("setup transport failed")
	tests := []struct {
		name       string
		setupReply func() (*http.Response, error)
	}{
		{
			name: "transport error",
			setupReply: func() (*http.Response, error) {
				return nil, setupErr
			},
		},
		{
			name: "non-2xx response",
			setupReply: func() (*http.Response, error) {
				return testHTTPResponse(http.StatusInternalServerError, "setup failed"), nil
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var laterCalls atomic.Int32
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path == "/setup" {
					return test.setupReply()
				}
				laterCalls.Add(1)
				return testHTTPResponse(http.StatusOK, `{"stock":0}`), nil
			})}
			setup := engine.HTTPRequest{Method: http.MethodPost, URL: "http://example.test/setup"}
			scenario := scenarioWithoutSetup()
			scenario.Setup = &setup

			result, err := engine.Run(context.Background(), client, scenario)
			if err == nil {
				t.Fatal("Run() error = nil, want setup error")
			}
			if result.Setup == nil {
				t.Fatal("failed setup execution was not preserved")
			}
			if len(result.History.Attempts) != 0 || result.Observation != nil || result.Evaluation != nil {
				t.Errorf("later stages ran after setup failure: %#v", result)
			}
			if laterCalls.Load() != 0 {
				t.Errorf("calls after setup = %d, want 0", laterCalls.Load())
			}
		})
	}
}

func TestRunStopsAfterObservationFailure(t *testing.T) {
	t.Parallel()

	observationErr := errors.New("observation transport failed")
	tests := []struct {
		name             string
		observationReply func() (*http.Response, error)
	}{
		{
			name: "transport error",
			observationReply: func() (*http.Response, error) {
				return nil, observationErr
			},
		},
		{
			name: "non-2xx response",
			observationReply: func() (*http.Response, error) {
				return testHTTPResponse(http.StatusServiceUnavailable, "unavailable"), nil
			},
		},
		{
			name: "malformed JSON",
			observationReply: func() (*http.Response, error) {
				return testHTTPResponse(http.StatusOK, `{"stock":`), nil
			},
		},
		{
			name: "truncated response",
			observationReply: func() (*http.Response, error) {
				return testHTTPResponse(
					http.StatusOK,
					`{"stock":0}`+strings.Repeat(" ", engine.MaxHTTPBodyBytes),
				), nil
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path == "/state" {
					return test.observationReply()
				}
				return testHTTPResponse(http.StatusCreated, "purchased"), nil
			})}

			result, err := engine.Run(context.Background(), client, scenarioWithoutSetup())
			if err == nil {
				t.Fatal("Run() error = nil, want observation or evaluation error")
			}
			if len(result.History.Attempts) != 1 {
				t.Errorf("history attempt count = %d, want 1", len(result.History.Attempts))
			}
			if result.Observation == nil {
				t.Fatal("failed observation execution was not preserved")
			}
			if result.Evaluation != nil || result.Outcome != "" {
				t.Errorf("evaluation completed after observation failure: %#v", result)
			}
		})
	}
}

func TestRunCancellationSkipsObservation(t *testing.T) {
	t.Parallel()

	const concurrency = 2
	var active atomic.Int32
	requestsStarted := make(chan struct{})
	var signalStarted sync.Once
	var observations atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/purchase":
			if active.Add(1) == concurrency {
				signalStarted.Do(func() { close(requestsStarted) })
			}
			defer active.Add(-1)
			<-request.Context().Done()
		case "/state":
			observations.Add(1)
			if _, err := io.WriteString(writer, `{"stock":0}`); err != nil {
				t.Errorf("write observation: %v", err)
			}
		}
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithCancel(context.Background())
	type runResponse struct {
		result engine.RunResult
		err    error
	}
	done := make(chan runResponse, 1)
	go func() {
		scenario := scenarioWithoutSetup()
		scenario.Operation.Request.URL = server.URL + "/purchase"
		scenario.Observation.URL = server.URL + "/state"
		scenario.Attempts = 4
		scenario.Concurrency = concurrency
		result, err := engine.Run(ctx, server.Client(), scenario)
		done <- runResponse{result: result, err: err}
	}()

	select {
	case <-requestsStarted:
		cancel()
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("concurrent operations did not start")
	}

	select {
	case response := <-done:
		if !errors.Is(response.err, context.Canceled) {
			t.Fatalf("Run() error = %v, want context.Canceled", response.err)
		}
		if len(response.result.History.Attempts) != 4 {
			t.Errorf("history attempt count = %d, want 4", len(response.result.History.Attempts))
		}
		if response.result.Observation != nil || response.result.Evaluation != nil {
			t.Error("observation or evaluation ran after cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after cancellation")
	}

	if observations.Load() != 0 {
		t.Errorf("observation requests = %d, want 0", observations.Load())
	}
}

func TestRunCancellationDuringOptionalObservationPreservesHistoryEvaluation(t *testing.T) {
	t.Parallel()

	observationStarted := make(chan struct{})
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/purchase" {
			return testHTTPResponse(http.StatusCreated, "accepted"), nil
		}
		close(observationStarted)
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	scenario := maximumSuccessfulAttemptsScenario(0)
	scenario.Observation = &engine.HTTPRequest{Method: http.MethodGet, URL: "http://example.test/state"}

	type runResponse struct {
		result engine.RunResult
		err    error
	}
	done := make(chan runResponse, 1)
	go func() {
		result, err := engine.Run(ctx, client, scenario)
		done <- runResponse{result: result, err: err}
	}()

	select {
	case <-observationStarted:
		cancel()
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("optional observation did not start")
	}

	select {
	case response := <-done:
		if !errors.Is(response.err, context.Canceled) {
			t.Fatalf("Run() error = %v, want context.Canceled", response.err)
		}
		if response.result.Evaluation == nil ||
			response.result.Evaluation.MaximumSuccessfulAttempts == nil ||
			!response.result.Evaluation.Violated ||
			response.result.Observation == nil {
			t.Errorf("partial history and observation evidence was not retained: %#v", response.result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after observation cancellation")
	}
}

func TestRunValidatesBeforeSetup(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return testHTTPResponse(http.StatusNoContent, ""), nil
	})}
	setup := engine.HTTPRequest{Method: http.MethodPost, URL: "http://example.test/setup"}
	scenario := scenarioWithoutSetup()
	scenario.Setup = &setup
	scenario.Invariant.JSONIntegerMinimum.Name = ""

	result, err := engine.Run(context.Background(), client, scenario)
	if err == nil {
		t.Fatal("Run() error = nil, want validation error")
	}
	if calls.Load() != 0 {
		t.Errorf("HTTP calls = %d, want 0", calls.Load())
	}
	if result.Setup != nil || len(result.History.Attempts) != 0 {
		t.Errorf("scenario stages ran before validation completed: %#v", result)
	}
}

func TestRunValidatesInvariantVariantBeforeRequests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		change func(*engine.Scenario)
	}{
		{
			name: "no invariant",
			change: func(scenario *engine.Scenario) {
				scenario.Invariant = engine.Invariant{}
			},
		},
		{
			name: "two invariants",
			change: func(scenario *engine.Scenario) {
				scenario.Invariant.MaximumSuccessfulAttempts = &engine.MaximumSuccessfulAttemptsInvariant{
					Name:    "at most one success",
					Maximum: 1,
				}
			},
		},
		{
			name: "state invariant without observation",
			change: func(scenario *engine.Scenario) {
				scenario.Observation = nil
			},
		},
		{
			name: "invalid successful status",
			change: func(scenario *engine.Scenario) {
				*scenario = maximumSuccessfulAttemptsScenario(1)
				scenario.Invariant.MaximumSuccessfulAttempts.SuccessfulStatusCodes = []int{600}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return testHTTPResponse(http.StatusNoContent, ""), nil
			})}
			scenario := scenarioWithoutSetup()
			setup := engine.HTTPRequest{Method: http.MethodPost, URL: "http://example.test/setup"}
			scenario.Setup = &setup
			test.change(&scenario)

			if _, err := engine.Run(context.Background(), client, scenario); err == nil {
				t.Fatal("Run() error = nil, want validation error")
			}
			if calls.Load() != 0 {
				t.Errorf("HTTP calls = %d, want 0", calls.Load())
			}
		})
	}
}

func scenarioWithoutSetup() engine.Scenario {
	return engine.Scenario{
		Operation: engine.Operation{
			Name:    "purchase",
			Request: engine.HTTPRequest{Method: http.MethodPost, URL: "http://example.test/purchase"},
		},
		Attempts:    1,
		Concurrency: 1,
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

func maximumSuccessfulAttemptsScenario(maximum int) engine.Scenario {
	return engine.Scenario{
		Operation: engine.Operation{
			Name:    "purchase",
			Request: engine.HTTPRequest{Method: http.MethodPost, URL: "http://example.test/purchase"},
		},
		Attempts:    1,
		Concurrency: 1,
		Invariant: engine.Invariant{
			MaximumSuccessfulAttempts: &engine.MaximumSuccessfulAttemptsInvariant{
				Name:                  "accepted purchases must not exceed stock",
				Maximum:               maximum,
				SuccessfulStatusCodes: []int{http.StatusCreated},
			},
		},
	}
}
