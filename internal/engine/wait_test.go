package engine_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eumarumar/concurtest/internal/engine"
	"github.com/eumarumar/concurtest/internal/failure"
)

func pendingCondition(path string) *engine.WaitCondition {
	return &engine.WaitCondition{
		Request:   engine.HTTPRequest{Method: http.MethodGet, URL: "http://example.test" + path},
		Assertion: engine.ResponseAssertion{JSONInteger: &engine.JSONIntegerInvariant{Name: "No pending work", Path: []string{"pending"}, Equals: new(int64(0))}},
		Timeout:   time.Second, Interval: time.Millisecond, MaxPolls: 3,
	}
}

func TestWaitEvidenceIsBoundedAfterFullResponseEvaluation(t *testing.T) {
	t.Parallel()
	body := `{"padding":"` + strings.Repeat("x", 800) + `","pending":0}`
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("X-Private") != "request-secret" {
			t.Error("request headers changed before sending")
		}
		return testHTTPResponse(http.StatusOK, body), nil
	})}
	scenario := scenarioWithoutSetup()
	scenario.Readiness = pendingCondition("/ready")
	scenario.Readiness.Request.Header = http.Header{"X-Private": {"request-secret"}}
	// Stop immediately after readiness so the fixture need not implement operations.
	scenario.Precondition = &engine.Precondition{Request: engine.HTTPRequest{Method: http.MethodGet, URL: "http://example.test/initial", Header: http.Header{"X-Private": {"request-secret"}}}, Assertion: *booleanPreparationAssertion(true)}
	result, err := engine.Run(context.Background(), client, scenario)
	if err == nil || result.Readiness == nil || !result.Readiness.Satisfied {
		t.Fatalf("full response was not evaluated: result=%+v error=%v", result, err)
	}
	poll := result.Readiness.Polls[0]
	if len(poll.Execution.Response.Body) != engine.MaxWaitBodyBytes || !poll.Execution.Response.BodyTruncated || poll.Assertion == nil || !poll.Assertion.Satisfied || *poll.Assertion.ObservedInteger != 0 {
		t.Fatalf("retained evidence is incorrect: %+v", poll)
	}
	if poll.Execution.Request.Header != nil || poll.Execution.Request.Body != nil || poll.Execution.Response.Header != nil || result.Readiness.Condition.Request.Header != nil {
		t.Fatal("private request/response headers were duplicated in wait evidence")
	}
	*scenario.Readiness.Assertion.JSONInteger.Equals = 1
	if *result.Readiness.Condition.Assertion.JSONInteger.Equals != 0 {
		t.Fatal("retained condition shares caller constraints")
	}
}

func TestInvalidWaitConditionsStartNoTraffic(t *testing.T) {
	t.Parallel()
	for _, change := range []func(*engine.WaitCondition){
		func(c *engine.WaitCondition) { c.Timeout = 0 }, func(c *engine.WaitCondition) { c.Interval = 0 },
		func(c *engine.WaitCondition) { c.MaxPolls = engine.MaxWaitPolls + 1 }, func(c *engine.WaitCondition) { c.MaxPolls = -1 },
		func(c *engine.WaitCondition) { c.Assertion = engine.ResponseAssertion{} },
	} {
		calls := 0
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return testHTTPResponse(http.StatusOK, ""), nil })}
		for _, stage := range []string{"readiness", "completion"} {
			scenario := scenarioWithoutSetup()
			condition := pendingCondition("/wait")
			change(condition)
			if stage == "readiness" {
				scenario.Readiness = condition
			} else {
				scenario.Completion = condition
			}
			if _, err := engine.Run(context.Background(), client, scenario); err == nil || calls != 0 {
				t.Fatalf("invalid %s condition: error=%v calls=%d", stage, err, calls)
			}
		}
	}
}

func TestWaitConditionsDrainWorkBeforeResetAndObservation(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	pending, count, readinessPolls, completionPolls := 1, 0, 0, 0
	var events []string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, r.URL.Path)
		switch r.URL.Path {
		case "/ready":
			readinessPolls++
			if readinessPolls == 2 {
				pending = 0
			}
			return testHTTPResponse(http.StatusOK, fmt.Sprintf(`{"pending":%d}`, pending)), nil
		case "/reset":
			if pending != 0 {
				t.Error("reset overlapped unfinished work")
			}
			count, completionPolls = 0, 0
			return testHTTPResponse(http.StatusNoContent, ""), nil
		case "/purchase":
			pending++
			return testHTTPResponse(http.StatusAccepted, ""), nil
		case "/complete":
			completionPolls++
			if completionPolls == 2 {
				count += pending
				pending = 0
			}
			return testHTTPResponse(http.StatusOK, fmt.Sprintf(`{"pending":%d}`, pending)), nil
		case "/state":
			if pending != 0 {
				t.Error("state observed before deferred work completed")
			}
			return testHTTPResponse(http.StatusOK, fmt.Sprintf(`{"stock":%d}`, count)), nil
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			return nil, errors.New("unexpected request")
		}
	})}
	scenario := scenarioWithoutSetup()
	scenario.Attempts, scenario.Concurrency = 2, 2
	scenario.Setup = []engine.SetupStep{{Request: engine.HTTPRequest{Method: http.MethodPost, URL: "http://example.test/reset"}}}
	scenario.Readiness, scenario.Completion = pendingCondition("/ready"), pendingCondition("/complete")
	scenario.Invariant.JSONInteger.Minimum = nil
	scenario.Invariant.JSONInteger.Maximum = new(int64(1))
	result, err := engine.RunTrials(context.Background(), client, scenario, 3)
	if err != nil || result.Status != engine.TrialStatusViolated || len(result.Trials) != 3 {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	var want []string
	for i, trial := range result.Trials {
		want = append(want, "/ready")
		if i == 0 {
			want = append(want, "/ready")
		}
		want = append(want, "/reset", "/purchase", "/purchase", "/complete", "/complete", "/state")
		if !trial.Run.Readiness.Satisfied || !trial.Run.Completion.Satisfied || len(trial.Run.Completion.Polls) != 2 || trial.Run.Evaluation.JSONInteger.Observed != 2 {
			t.Fatalf("completion evidence missing: %+v", trial.Run)
		}
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v, want %v", events, want)
	}
}

func TestUnverifiedWaitStopsLaterTrialsAndPreservesHistoryViolations(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"readiness", "completion"} {
		for _, history := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/history=%t", stage, history), func(t *testing.T) {
				var mu sync.Mutex
				resets, operations, polls, observations := 0, 0, 0, 0
				client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					mu.Lock()
					defer mu.Unlock()
					switch r.URL.Path {
					case "/wait":
						polls++
						return testHTTPResponse(http.StatusOK, `{"pending":1}`), nil
					case "/reset":
						resets++
						return testHTTPResponse(http.StatusNoContent, ""), nil
					case "/purchase":
						operations++
						return testHTTPResponse(http.StatusCreated, ""), nil
					case "/state":
						observations++
						return testHTTPResponse(http.StatusOK, `{"stock":0}`), nil
					default:
						return nil, errors.New("unexpected request")
					}
				})}
				scenario := scenarioWithoutSetup()
				scenario.Setup = []engine.SetupStep{{Request: engine.HTTPRequest{Method: http.MethodPost, URL: "http://example.test/reset"}}}
				if history {
					scenario.Invariant = maximumSuccessfulAttemptsScenario(0).Invariant
				}
				if stage == "readiness" {
					scenario.Readiness = pendingCondition("/wait")
				} else {
					scenario.Completion = pendingCondition("/wait")
				}
				result, err := engine.RunTrials(context.Background(), client, scenario, 3)
				if err != nil || len(result.Trials) != 1 || !result.WaitFailed() || polls != 3 || observations != 0 {
					t.Fatalf("unsafe continuation: result=%+v polls=%d observation=%d error=%v", result, polls, observations, err)
				}
				wantStatus := engine.TrialStatusErrored
				if history && stage == "completion" {
					wantStatus = engine.TrialStatusViolated
				}
				if result.Status != wantStatus || result.Trials[0].Err == nil {
					t.Fatalf("result status=%s want %s", result.Status, wantStatus)
				}
				if stage == "readiness" && (resets != 0 || operations != 0) {
					t.Fatal("readiness failure allowed reset or operations")
				}
				if stage == "completion" && (resets != 1 || operations != 1) {
					t.Fatal("completion failure allowed a later reset")
				}
			})
		}
	}
}

func TestWaitFailuresKeepPartialEvidenceAndCancellation(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"transport", "read", "close", "status", "malformed", "truncated", "deadline", "interval deadline", "late match", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sentinel := errors.New("poll failed")
			calls := 0
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if kind == "transport" {
					return nil, sentinel
				}
				if kind == "deadline" {
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
				if kind == "cancel" {
					cancel()
					return nil, ctx.Err()
				}
				response := testHTTPResponse(http.StatusOK, `{"pending":1}`)
				switch kind {
				case "read":
					response.Body = setupResponseBody{Reader: errorReader{sentinel}, close: func() error { return nil }}
				case "close":
					response.Body = setupResponseBody{Reader: strings.NewReader(`{"pending":0}`), close: func() error { return sentinel }}
				case "status":
					response.StatusCode = 500
				case "malformed":
					response = testHTTPResponse(http.StatusOK, `{`)
				case "truncated":
					response = testHTTPResponse(http.StatusOK, `{"pending":0}`+strings.Repeat(" ", engine.MaxHTTPBodyBytes))
				case "late match":
					response.Body = setupResponseBody{Reader: strings.NewReader(`{"pending":0}`), close: func() error { <-r.Context().Done(); return nil }}
				}
				return response, nil
			})}
			scenario := scenarioWithoutSetup()
			scenario.Readiness = pendingCondition("/ready")
			if kind == "deadline" || kind == "interval deadline" || kind == "late match" {
				scenario.Readiness.Timeout = 100 * time.Millisecond
				scenario.Readiness.Interval = time.Second
			}
			result, err := engine.RunTrials(ctx, client, scenario, 3)
			if len(result.Trials) != 1 || !result.WaitFailed() || calls != 1 {
				t.Fatalf("result=%+v calls=%d error=%v", result, calls, err)
			}
			wait := result.Trials[0].Run.Readiness
			if wait.Satisfied || len(wait.Polls) != 1 || failure.CodeOf(wait.Err) != failure.CodeReadinessFailed {
				t.Fatalf("wait=%+v", wait)
			}
			if kind == "cancel" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost: %v", err)
				}
			} else if err != nil {
				t.Fatalf("trial error became an orchestration error: %v", err)
			}
			if (kind == "deadline" || kind == "interval deadline" || kind == "late match") && !errors.Is(wait.Err, context.DeadlineExceeded) {
				t.Fatalf("deadline identity lost: %v", wait.Err)
			}
			if kind == "late match" && (wait.Polls[0].Assertion == nil || !wait.Polls[0].Assertion.Satisfied) {
				t.Fatal("late matching response fixture did not exercise the deadline check")
			}
			if (kind == "transport" || kind == "read" || kind == "close") && !errors.Is(wait.Err, sentinel) {
				t.Fatalf("poll failure identity lost: %v", wait.Err)
			}
		})
	}
}

func TestCompletionWaitFollowsChangeBaselineAndClosesEveryPoll(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var events []string
	count, pending, polls, active := 5, 0, 0, false
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if active {
			t.Error("next request started before the previous response closed")
		}
		active = true
		events = append(events, r.URL.Path)
		body := `{"pending":0}`
		switch r.URL.Path {
		case "/ready":
		case "/state":
			body = fmt.Sprintf(`{"stock":%d}`, count)
		case "/purchase":
			pending++
		case "/complete":
			polls++
			if polls == 2 {
				count -= pending
				pending = 0
			}
			body = fmt.Sprintf(`{"pending":%d}`, pending)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		response := testHTTPResponse(http.StatusOK, "")
		response.Body = setupResponseBody{Reader: strings.NewReader(body), close: func() error { mu.Lock(); active = false; mu.Unlock(); return nil }}
		return response, nil
	})}
	scenario := scenarioWithoutSetup()
	scenario.Invariant.JSONInteger.Minimum = nil
	scenario.Invariant.JSONInteger.Change = new(int64(-1))
	scenario.Readiness, scenario.Completion = pendingCondition("/ready"), pendingCondition("/complete")
	result, err := engine.Run(context.Background(), client, scenario)
	if err != nil || result.Outcome != engine.RunOutcomePassed || *result.Evaluation.JSONInteger.Baseline != 5 || result.Evaluation.JSONInteger.Observed != 4 {
		t.Fatalf("change invariant did not use settled final state: result=%+v error=%v", result, err)
	}
	want := []string{"/ready", "/state", "/purchase", "/complete", "/complete", "/state"}
	if !reflect.DeepEqual(events, want) || active {
		t.Fatalf("request lifecycle=%v active=%t, want %v", events, active, want)
	}
}

func TestWaitIntervalStopsPromptlyOnParentCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	closed := make(chan struct{})
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		response := testHTTPResponse(http.StatusOK, "")
		response.Body = setupResponseBody{Reader: strings.NewReader(`{"pending":1}`), close: func() error { close(closed); return nil }}
		return response, nil
	})}
	scenario := scenarioWithoutSetup()
	scenario.Readiness = pendingCondition("/ready")
	scenario.Readiness.Timeout, scenario.Readiness.Interval = 10*time.Second, 5*time.Second
	type completion struct {
		result engine.TrialsResult
		err    error
	}
	finished := make(chan completion, 1)
	go func() { result, err := engine.RunTrials(ctx, client, scenario, 3); finished <- completion{result, err} }()
	select {
	case <-closed:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("first poll did not close")
	}
	select {
	case result := <-finished:
		if !errors.Is(result.err, context.Canceled) || len(result.result.Trials) != 1 || len(result.result.Trials[0].Run.Readiness.Polls) != 1 {
			t.Fatalf("cancellation did not stop interval: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("wait did not stop promptly after cancellation")
	}
}
