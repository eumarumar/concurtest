package engine_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eumarumar/concurtest/internal/engine"
	"github.com/eumarumar/concurtest/internal/failure"
)

func TestEvaluateJSONIntegerChange(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                      string
		baseline, final, expected int64
		violated, overflow        bool
	}{
		{name: "decrement once", baseline: 1000, final: 999, expected: -1},
		{name: "duplicate decrement", baseline: 1000, final: 998, expected: -1, violated: true},
		{name: "no decrement", baseline: 1000, final: 1000, expected: -1, violated: true},
		{name: "increment", baseline: -1, final: 1, expected: 2},
		{name: "unchanged zero", baseline: 0, final: 0, expected: 0},
		{name: "negative baseline", baseline: -10, final: -11, expected: -1},
		{name: "exact large integer", baseline: math.MaxInt64, final: math.MaxInt64 - 1, expected: -1},
		{name: "minimum change", baseline: 0, final: math.MinInt64, expected: math.MinInt64},
		{name: "maximum change", baseline: math.MinInt64, final: -1, expected: math.MaxInt64},
		{name: "minimum baseline unchanged", baseline: math.MinInt64, final: math.MinInt64},
		{name: "maximum baseline unchanged", baseline: math.MaxInt64, final: math.MaxInt64},
		{name: "positive overflow", baseline: -1, final: math.MaxInt64, expected: math.MinInt64, overflow: true},
		{name: "negative overflow", baseline: 1, final: math.MinInt64, expected: math.MaxInt64, overflow: true},
		{name: "full positive span", baseline: math.MinInt64, final: math.MaxInt64, expected: -1, overflow: true},
		{name: "full negative span", baseline: math.MaxInt64, final: math.MinInt64, expected: 1, overflow: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			invariant := engine.JSONIntegerInvariant{Name: test.name, Path: []string{"data", "quantity"}, Change: new(test.expected)}
			result, err := engine.EvaluateJSONIntegerChange(invariant, test.baseline, []byte(fmt.Sprintf(`{"data":{"quantity":%d}}`, test.final)))
			if test.overflow {
				if err == nil || !strings.Contains(err.Error(), "not representable as int64") {
					t.Fatalf("overflow error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.Observed != test.final || result.Baseline == nil || *result.Baseline != test.baseline || result.Change == nil || *result.Change != test.final-test.baseline || result.Violated != test.violated {
				t.Fatalf("unexpected evaluation: %#v", result)
			}
			*invariant.Change = 123
			if *result.Invariant.Change != test.expected {
				t.Fatal("evaluation shares constraint with caller")
			}
		})
	}
}

func TestEvaluateJSONIntegerChangeRequiresExclusiveConstraintAndBaseline(t *testing.T) {
	t.Parallel()
	invariant := engine.JSONIntegerInvariant{Name: "stock change", Path: []string{"stock"}, Change: new(int64(-1))}
	if _, err := engine.EvaluateJSONInteger(invariant, []byte(`{"stock":1}`)); err == nil {
		t.Fatal("change evaluated without a baseline")
	}
	for _, field := range []string{"minimum", "maximum", "equals"} {
		definition := invariant
		switch field {
		case "minimum":
			definition.Minimum = new(int64(0))
		case "maximum":
			definition.Maximum = new(int64(10))
		case "equals":
			definition.Equals = new(int64(1))
		}
		if _, err := engine.EvaluateJSONIntegerChange(definition, 2, []byte(`{"stock":1}`)); err == nil {
			t.Fatalf("accepted change with %s", field)
		}
	}
	invariant.Change = nil
	invariant.Equals = new(int64(1))
	if _, err := engine.EvaluateJSONIntegerChange(invariant, 2, []byte(`{"stock":1}`)); err == nil {
		t.Fatal("change evaluation accepted a non-change invariant")
	}
}

func TestRunChangeCapturesFreshBaselineAfterSetupInEveryTrial(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var stock int64
	setups, observations, operations := 0, 0, 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/setup":
			setups++
			stock = int64(1000 * setups)
			if observations != (setups-1)*2 || operations != (setups-1)*2 {
				t.Error("setup did not follow the previous final observation")
			}
			return testHTTPResponse(http.StatusNoContent, ""), nil
		case "/state":
			observations++
			if observations%2 == 1 && operations != (setups-1)*2 {
				t.Error("baseline did not precede operations")
			}
			if observations%2 == 0 && operations != setups*2 {
				t.Error("final observation did not follow all operations")
			}
			return testHTTPResponse(http.StatusOK, fmt.Sprintf(`{"stock":%d}`, stock)), nil
		case "/purchase":
			if observations != 2*setups-1 {
				t.Error("operation ran outside the observation pair")
			}
			stock--
			operations++
			return testHTTPResponse(http.StatusCreated, ""), nil
		default:
			return nil, fmt.Errorf("unexpected path %s", r.URL.Path)
		}
	})}
	scenario := scenarioWithoutSetup()
	scenario.Setup = &engine.HTTPRequest{Method: http.MethodPost, URL: "http://example.test/setup"}
	scenario.Attempts, scenario.Concurrency = 2, 2
	scenario.Invariant.JSONInteger.Minimum = nil
	scenario.Invariant.JSONInteger.Change = new(int64(-1))
	result, err := engine.RunTrials(context.Background(), client, scenario, 3)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != engine.TrialStatusViolated || len(result.Trials) != 3 {
		t.Fatalf("unexpected trials: %#v", result)
	}
	for i, trial := range result.Trials {
		baseline := trial.Run.BaselineObservation
		if baseline == nil || trial.Run.Observation == nil || trial.Run.Evaluation == nil {
			t.Fatal("missing observation/evaluation evidence")
		}
		value := trial.Run.Evaluation.JSONInteger
		if value.Baseline == nil || *value.Baseline != int64((i+1)*1000) || value.Change == nil || *value.Change != -2 {
			t.Fatalf("trial %d did not retain its baseline and change: %#v", i+1, value)
		}
		if baseline.StartedAt.Before(trial.Run.Setup.CompletedAt) || trial.Run.History.StartedAt.Before(baseline.CompletedAt) || trial.Run.Observation.StartedAt.Before(trial.Run.History.CompletedAt) {
			t.Fatal("stage timing order is incorrect")
		}
	}
}

func TestRunChangeObservationFailures(t *testing.T) {
	t.Parallel()
	transportErr := errors.New("observation unavailable")
	tests := []struct {
		name  string
		reply func() (*http.Response, error)
		code  failure.Code
	}{
		{"transport", func() (*http.Response, error) { return nil, transportErr }, failure.CodeObservationFailed},
		{"HTTP status", func() (*http.Response, error) { return testHTTPResponse(503, "unavailable"), nil }, failure.CodeUnexpectedHTTPStatus},
		{"malformed JSON", func() (*http.Response, error) { return testHTTPResponse(200, `{"stock":`), nil }, failure.CodeInvariantEvaluationFailed},
		{"missing path", func() (*http.Response, error) { return testHTTPResponse(200, `{}`), nil }, failure.CodeInvariantEvaluationFailed},
		{"null", func() (*http.Response, error) { return testHTTPResponse(200, `{"stock":null}`), nil }, failure.CodeInvariantEvaluationFailed},
		{"non-integer", func() (*http.Response, error) { return testHTTPResponse(200, `{"stock":1.5}`), nil }, failure.CodeInvariantEvaluationFailed},
		{"out of int64 range", func() (*http.Response, error) { return testHTTPResponse(200, `{"stock":9223372036854775808}`), nil }, failure.CodeInvariantEvaluationFailed},
		{"trailing value", func() (*http.Response, error) { return testHTTPResponse(200, `{"stock":1} {}`), nil }, failure.CodeInvariantEvaluationFailed},
		{"truncated", func() (*http.Response, error) {
			return testHTTPResponse(200, `{"stock":1}`+strings.Repeat(" ", engine.MaxHTTPBodyBytes)), nil
		}, failure.CodeResponseTruncated},
	}
	for _, test := range tests {
		for _, stage := range []string{"baseline", "final"} {
			t.Run(stage+"/"+test.name, func(t *testing.T) {
				var operations atomic.Int32
				observations := 0
				client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/state" {
						observations++
						if stage == "baseline" || observations == 2 {
							return test.reply()
						}
						return testHTTPResponse(200, `{"stock":1000}`), nil
					}
					operations.Add(1)
					return testHTTPResponse(201, ""), nil
				})}
				scenario := scenarioWithoutSetup()
				scenario.Invariant.JSONInteger.Minimum = nil
				scenario.Invariant.JSONInteger.Change = new(int64(-1))
				result, err := engine.Run(context.Background(), client, scenario)
				if err == nil || failure.CodeOf(err) != test.code {
					t.Fatalf("error = %v, want code %s", err, test.code)
				}
				if result.BaselineObservation == nil || result.Evaluation != nil || result.Outcome != "" {
					t.Fatal("invalid partial evidence")
				}
				if stage == "baseline" && (operations.Load() != 0 || len(result.History.Attempts) != 0 || result.Observation != nil) {
					t.Fatal("operations ran after failed baseline")
				}
				if stage == "final" && (operations.Load() != 1 || len(result.History.Attempts) != 1 || result.Observation == nil) {
					t.Fatal("final failure discarded operation evidence")
				}
			})
		}
	}
}

func TestRunChangeCancellationDuringBaseline(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	var operations atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/state" {
			operations.Add(1)
			return testHTTPResponse(201, ""), nil
		}
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	scenario := scenarioWithoutSetup()
	scenario.Invariant.JSONInteger.Minimum = nil
	scenario.Invariant.JSONInteger.Change = new(int64(-1))
	type response struct {
		result engine.RunResult
		err    error
	}
	done := make(chan response, 1)
	go func() { result, err := engine.Run(ctx, client, scenario); done <- response{result, err} }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("baseline did not start")
	}
	cancel()
	select {
	case response := <-done:
		if !errors.Is(response.err, context.Canceled) || response.result.BaselineObservation == nil || response.result.Observation != nil || response.result.Evaluation != nil || operations.Load() != 0 {
			t.Fatalf("unexpected canceled result: %#v, %v", response.result, response.err)
		}
	case <-time.After(time.Second):
		t.Fatal("run did not stop after cancellation")
	}
}
