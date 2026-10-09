package engine_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/eumarumar/concurtest/internal/engine"
)

type errorReader struct{ err error }

func (reader errorReader) Read([]byte) (int, error) { return 0, reader.err }

// Closing the response body marks setup completion. Checking this before the
// next request catches both overlapping requests and premature advancement.
type setupResponseBody struct {
	io.Reader
	close func() error
}

func (body setupResponseBody) Close() error { return body.close() }

func orderedSetupScenario() engine.Scenario {
	scenario := scenarioWithoutSetup()
	scenario.Attempts = 2
	scenario.Concurrency = 2
	for index, name := range []string{"Reset inventory", "Clear basket", "Prepare basket"} {
		scenario.Setup = append(scenario.Setup, engine.SetupStep{
			Name: name,
			Request: engine.HTTPRequest{Method: http.MethodPost,
				URL: fmt.Sprintf("http://example.test/setup/%d", index+1)},
		})
	}
	scenario.Invariant.JSONInteger.Minimum = nil
	scenario.Invariant.JSONInteger.Change = new(int64(-2))
	return scenario
}

func TestOrderedSetupsCompleteBeforeBaselineAndOperationsEveryTrial(t *testing.T) {
	t.Parallel()
	scenario := orderedSetupScenario()
	var mu sync.Mutex
	completed, stock := 0, 0
	var events []string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, r.URL.Path)
		for index := range scenario.Setup {
			if r.URL.Path != fmt.Sprintf("/setup/%d", index+1) {
				continue
			}
			if index == 0 {
				completed = 0
				stock = 10
			}
			if completed != index {
				t.Errorf("step %d started after %d completed steps", index+1, completed)
			}
			response := testHTTPResponse(http.StatusOK, "")
			response.Body = setupResponseBody{Reader: strings.NewReader("prepared"), close: func() error {
				mu.Lock()
				defer mu.Unlock()
				completed++
				return nil
			}}
			return response, nil
		}
		if completed != len(scenario.Setup) {
			t.Errorf("%s started before all setup responses completed", r.URL.Path)
		}
		if r.Method == http.MethodGet {
			return testHTTPResponse(http.StatusOK, fmt.Sprintf(`{"stock":%d}`, stock)), nil
		}
		stock--
		return testHTTPResponse(http.StatusCreated, ""), nil
	})}
	result, err := engine.RunTrials(context.Background(), client, scenario, 3)
	if err != nil || result.Status != engine.TrialStatusPassed {
		t.Fatalf("RunTrials() = %+v, error = %v", result, err)
	}
	var want []string
	for _, trial := range result.Trials {
		want = append(want, "/setup/1", "/setup/2", "/setup/3", "/state", "/purchase", "/purchase", "/state")
		if len(trial.Run.Setup) != 3 || trial.Run.BaselineObservation == nil {
			t.Fatalf("missing preparation evidence: %+v", trial.Run)
		}
		for index, step := range trial.Run.Setup {
			if step.Name != scenario.Setup[index].Name || string(step.Execution.Response.Body) != "prepared" {
				t.Errorf("setup evidence %d = %+v", index+1, step)
			}
			if index > 0 && step.Execution.StartedAt.Before(trial.Run.Setup[index-1].Execution.CompletedAt) {
				t.Error("setup execution timings overlap")
			}
		}
		if trial.Run.BaselineObservation.StartedAt.Before(trial.Run.Setup[2].Execution.CompletedAt) {
			t.Error("baseline started before final setup completion")
		}
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("request order = %v, want %v", events, want)
	}
}

func TestSetupFailureStopsRemainingStepsAndAllLaterStages(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"status", "transport", "read", "close", "cancellation"} {
		for failed := 1; failed <= 3; failed++ {
			t.Run(fmt.Sprintf("%s/step%d", kind, failed), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				sentinel := errors.New("setup failed")
				calls := 0
				client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					if calls != failed {
						return testHTTPResponse(http.StatusNoContent, ""), nil
					}
					response := testHTTPResponse(http.StatusOK, "")
					switch kind {
					case "status":
						response.StatusCode = http.StatusConflict
					case "transport":
						return nil, sentinel
					case "read":
						response.Body = setupResponseBody{Reader: errorReader{sentinel}, close: func() error { return nil }}
					case "close":
						response.Body = setupResponseBody{Reader: strings.NewReader(""), close: func() error { return sentinel }}
					case "cancellation":
						cancel()
						return nil, r.Context().Err()
					}
					return response, nil
				})}
				scenario := orderedSetupScenario()
				result, err := engine.Run(ctx, client, scenario)
				if err == nil || calls != failed || len(result.Setup) != failed {
					t.Fatalf("calls=%d, setup evidence=%d, error=%v", calls, len(result.Setup), err)
				}
				if !strings.Contains(err.Error(), fmt.Sprintf("setup step %d", failed)) || !strings.Contains(err.Error(), scenario.Setup[failed-1].Name) {
					t.Fatalf("error does not identify failed step: %v", err)
				}
				if kind == "cancellation" && !errors.Is(err, context.Canceled) {
					t.Fatalf("lost cancellation identity: %v", err)
				}
				if result.BaselineObservation != nil || len(result.History.Attempts) != 0 || result.Observation != nil || result.Evaluation != nil {
					t.Fatal("later stage ran after setup failure")
				}
			})
		}
	}
}

func TestSetupCancellationAfterResponseDoesNotStartAnotherRequest(t *testing.T) {
	t.Parallel()
	for _, afterStep := range []int{1, 3} {
		t.Run(fmt.Sprintf("after step %d", afterStep), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				response := testHTTPResponse(http.StatusOK, "")
				response.Body = setupResponseBody{Reader: strings.NewReader(""), close: func() error {
					if calls == afterStep {
						cancel()
					}
					return nil
				}}
				return response, nil
			})}
			result, err := engine.RunTrials(ctx, client, orderedSetupScenario(), 3)
			if !errors.Is(err, context.Canceled) || calls != afterStep || len(result.Trials) != 1 || len(result.Trials[0].Run.Setup) != afterStep {
				t.Fatalf("cancellation: calls=%d, result=%+v, error=%v", calls, result, err)
			}
			if result.Trials[0].Run.BaselineObservation != nil || len(result.Trials[0].Run.History.Attempts) != 0 {
				t.Fatal("later stages ran after setup cancellation")
			}
		})
	}
}

func TestNextTrialRestartsAllSetupsAfterFailure(t *testing.T) {
	t.Parallel()
	scenario := orderedSetupScenario()
	var mu sync.Mutex
	trial, operations := 0, 0
	var events []string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, r.URL.Path)
		if r.URL.Path == "/setup/1" {
			trial++
			operations = 0
		}
		if r.URL.Path == "/setup/2" && trial == 1 {
			return testHTTPResponse(http.StatusServiceUnavailable, "unavailable"), nil
		}
		if r.URL.Path == "/purchase" {
			operations++
		}
		return testHTTPResponse(http.StatusOK, fmt.Sprintf(`{"stock":%d}`, 10-operations)), nil
	})}
	result, err := engine.RunTrials(context.Background(), client, scenario, 2)
	if err != nil || len(result.Trials) != 2 || result.Trials[0].Status != engine.TrialStatusErrored || result.Trials[1].Status != engine.TrialStatusPassed {
		t.Fatalf("RunTrials() = %+v, error = %v", result, err)
	}
	want := []string{"/setup/1", "/setup/2", "/setup/1", "/setup/2", "/setup/3", "/state", "/purchase", "/purchase", "/state"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("request order = %v, want %v", events, want)
	}
}

func TestRunRejectsTooManySetupStepsBeforeSendingRequests(t *testing.T) {
	t.Parallel()
	scenario := orderedSetupScenario()
	scenario.Setup = make([]engine.SetupStep, engine.MaxSetupSteps+1)
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Error("request sent before validating setup count")
		return nil, errors.New("unexpected request")
	})}
	result, err := engine.Run(context.Background(), client, scenario)
	if err == nil || len(result.Setup) != 0 {
		t.Fatalf("Run() = %+v, error = %v", result, err)
	}
}
