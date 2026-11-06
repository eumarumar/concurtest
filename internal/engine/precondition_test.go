package engine_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/eumarumar/concurtest/internal/engine"
	"github.com/eumarumar/concurtest/internal/failure"
)

func TestPreparationChecksStopEveryLaterStageOnFailure(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"setup", "precondition"} {
		for _, kind := range []string{"mismatch", "malformed", "missing", "null", "wrong type", "trailing", "truncated", "transport", "read", "close", "status", "cancellation", "cancel after close"} {
			t.Run(stage+"/"+kind, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				sentinel := errors.New("request failed")
				calls, closed := 0, 0
				client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					if r.URL.Path != "/setup" && r.URL.Path != "/precondition" && r.URL.Path != "/later" {
						t.Errorf("later stage ran: %s", r.URL.Path)
					}
					body := `{"ok":true}`
					if r.URL.Path != "/"+stage {
						return testHTTPResponse(http.StatusOK, body), nil
					}
					switch kind {
					case "mismatch":
						body = `{"ok":false}`
					case "malformed":
						body = `{`
					case "missing":
						body = `{}`
					case "null":
						body = `{"ok":null}`
					case "wrong type":
						body = `{"ok":"true"}`
					case "trailing":
						body += " {}"
					case "truncated":
						body += strings.Repeat(" ", engine.MaxHTTPBodyBytes)
					case "transport":
						return nil, sentinel
					case "cancellation":
						cancel()
						return nil, ctx.Err()
					}
					response := testHTTPResponse(http.StatusOK, "")
					response.Body = setupResponseBody{Reader: strings.NewReader(body), close: func() error {
						closed++
						if kind == "cancel after close" {
							cancel()
						}
						if kind == "close" {
							return sentinel
						}
						return nil
					}}
					if kind == "read" {
						response.Body = setupResponseBody{Reader: errorReader{sentinel}, close: func() error { closed++; return nil }}
					}
					if kind == "status" {
						response.StatusCode = http.StatusConflict
					}
					return response, nil
				})}
				scenario := scenarioWithoutSetup()
				scenario.Invariant.JSONInteger.Minimum = nil
				scenario.Invariant.JSONInteger.Change = new(int64(-1))
				scenario.Setup = []engine.SetupStep{
					{Name: "Reset", Request: engine.HTTPRequest{Method: http.MethodPost, URL: "http://example.test/setup"}, Assertion: booleanPreparationAssertion(true)},
					{Name: "Later setup", Request: engine.HTTPRequest{Method: http.MethodPost, URL: "http://example.test/later"}},
				}
				scenario.Precondition = &engine.Precondition{Request: engine.HTTPRequest{Method: http.MethodGet, URL: "http://example.test/precondition"}, Assertion: *booleanPreparationAssertion(true)}
				result, err := engine.Run(ctx, client, scenario)
				wantCalls := 1
				if stage == "precondition" {
					wantCalls = 3
				}
				if err == nil || calls != wantCalls || len(result.History.Attempts) != 0 || result.BaselineObservation != nil || result.Observation != nil || result.Evaluation != nil || result.Outcome != "" {
					t.Fatalf("failure did not stop later stages: calls=%d result=%+v error=%v", calls, result, err)
				}
				if stage == "setup" && result.Precondition != nil {
					t.Fatal("precondition ran after failed setup")
				}
				if stage == "precondition" || kind == "mismatch" || kind == "malformed" || kind == "missing" || kind == "null" || kind == "wrong type" || kind == "trailing" || kind == "truncated" {
					if failure.CodeOf(err) != failure.CodePreconditionFailed {
						t.Fatalf("error code=%s, want precondition_failed", failure.CodeOf(err))
					}
				}
				if (kind == "cancellation" || kind == "cancel after close") && !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost: %v", err)
				}
				if (kind == "transport" || kind == "read" || kind == "close") && !errors.Is(err, sentinel) {
					t.Fatalf("underlying error lost: %v", err)
				}
				if kind != "transport" && kind != "cancellation" && closed != 1 {
					t.Fatalf("response bodies closed=%d, want 1", closed)
				}
			})
		}
	}
}

func TestInvalidPreparationAssertionsSendNoRequests(t *testing.T) {
	t.Parallel()
	for _, invalid := range []engine.ResponseAssertion{
		{},
		{JSONBoolean: &engine.JSONBooleanAssertion{Name: "Missing path"}},
		{JSONBoolean: &engine.JSONBooleanAssertion{Path: []string{"ok"}}},
		{JSONBoolean: &engine.JSONBooleanAssertion{Name: "Empty segment", Path: []string{""}}},
		{JSONInteger: &engine.JSONIntegerInvariant{Name: "Change", Path: []string{"count"}, Change: new(int64(0))}},
		{JSONInteger: &engine.JSONIntegerInvariant{Name: "Invalid bounds", Path: []string{"count"}, Minimum: new(int64(2)), Maximum: new(int64(1))}},
		{JSONInteger: &engine.JSONIntegerInvariant{Name: "Integer", Path: []string{"count"}, Equals: new(int64(0))}, JSONBoolean: booleanPreparationAssertion(true).JSONBoolean},
	} {
		for _, stage := range []string{"setup", "precondition"} {
			calls := 0
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return testHTTPResponse(http.StatusOK, ""), nil })}
			scenario := scenarioWithoutSetup()
			scenario.Setup = []engine.SetupStep{{Request: scenario.Operation.Request}}
			if stage == "setup" {
				scenario.Setup = append(scenario.Setup, engine.SetupStep{Request: scenario.Operation.Request, Assertion: &invalid})
			} else {
				scenario.Precondition = &engine.Precondition{Request: scenario.Operation.Request, Assertion: invalid}
			}
			if _, err := engine.Run(context.Background(), client, scenario); err == nil || calls != 0 {
				t.Fatalf("invalid %s assertion: error=%v sent=%d", stage, err, calls)
			}
		}
	}
}

func TestPreparationFailureDoesNotCarryIntoTheNextTrial(t *testing.T) {
	t.Parallel()
	for _, failedStage := range []string{"setup", "precondition"} {
		t.Run(failedStage, func(t *testing.T) {
			var trial, operations atomic.Int32
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/setup":
					trial.Add(1)
					return testHTTPResponse(http.StatusOK, fmt.Sprintf(`{"ok":%t}`, failedStage != "setup" || trial.Load() != 1)), nil
				case "/precondition":
					count := 0
					if failedStage == "precondition" && trial.Load() == 1 {
						count = 2
					}
					return testHTTPResponse(http.StatusOK, fmt.Sprintf(`{"count":%d}`, count)), nil
				case "/purchase":
					operations.Add(1)
					return testHTTPResponse(http.StatusCreated, ""), nil
				case "/state":
					return testHTTPResponse(http.StatusOK, `{"stock":0}`), nil
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					return nil, errors.New("unexpected request")
				}
			})}
			scenario := scenarioWithoutSetup()
			scenario.Setup = []engine.SetupStep{{Request: engine.HTTPRequest{Method: http.MethodPost, URL: "http://example.test/setup"}, Assertion: booleanPreparationAssertion(true)}}
			scenario.Precondition = &engine.Precondition{Request: engine.HTTPRequest{Method: http.MethodGet, URL: "http://example.test/precondition"}, Assertion: engine.ResponseAssertion{JSONInteger: &engine.JSONIntegerInvariant{Name: "Initial count", Path: []string{"count"}, Equals: new(int64(0))}}}
			result, err := engine.RunTrials(context.Background(), client, scenario, 2)
			if err != nil || len(result.Trials) != 2 || result.Trials[0].Status != engine.TrialStatusErrored || result.Trials[1].Status != engine.TrialStatusPassed || operations.Load() != 1 {
				t.Fatalf("subsequent trial did not restart checks: result=%+v operations=%d error=%v", result, operations.Load(), err)
			}
		})
	}
}

func TestPreparationChecksCompleteBeforeBaselineAndOperationsEveryTrial(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var events []string
	phase, stock, observations := 0, 0, 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, r.URL.Path)
		body := ""
		nextPhase := 0
		switch r.URL.Path {
		case "/setup":
			stock, phase, observations = 10, 0, 0
			body, nextPhase = `{"ok":true}`, 1
		case "/precondition":
			if phase != 1 {
				t.Errorf("precondition started before setup closed: phase=%d", phase)
			}
			body, nextPhase = `{"count":10}`, 2
		case "/state":
			observations++
			if observations == 1 && phase != 2 {
				t.Errorf("baseline started before precondition closed: phase=%d", phase)
			}
			body = fmt.Sprintf(`{"stock":%d}`, stock)
			if observations == 1 {
				nextPhase = 3
			}
		case "/purchase":
			if phase != 3 {
				t.Errorf("operation started before baseline closed: phase=%d", phase)
			}
			stock--
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		response := testHTTPResponse(http.StatusOK, "")
		response.Body = setupResponseBody{Reader: strings.NewReader(body), close: func() error {
			mu.Lock()
			defer mu.Unlock()
			if nextPhase != 0 {
				phase = nextPhase
			}
			return nil
		}}
		return response, nil
	})}
	scenario := orderedSetupScenario()
	scenario.Setup = []engine.SetupStep{{Name: "Reset", Request: engine.HTTPRequest{Method: http.MethodPost, URL: "http://example.test/setup"}, Assertion: booleanPreparationAssertion(true)}}
	scenario.Precondition = &engine.Precondition{
		Request:   engine.HTTPRequest{Method: http.MethodGet, URL: "http://example.test/precondition"},
		Assertion: engine.ResponseAssertion{JSONInteger: &engine.JSONIntegerInvariant{Name: "Initial count", Path: []string{"count"}, Equals: new(int64(10))}},
	}
	result, err := engine.RunTrials(context.Background(), client, scenario, 3)
	if err != nil || result.Status != engine.TrialStatusPassed {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	var want []string
	for _, trial := range result.Trials {
		want = append(want, "/setup", "/precondition", "/state", "/purchase", "/purchase", "/state")
		if trial.Run.Setup[0].Assertion == nil || !trial.Run.Setup[0].Assertion.Satisfied || trial.Run.Precondition == nil || !trial.Run.Precondition.Assertion.Satisfied {
			t.Fatalf("checks missing from trial evidence: %+v", trial.Run)
		}
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("request order=%v, want %v", events, want)
	}
}
