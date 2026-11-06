package engine_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eumarumar/concurtest/internal/engine"
)

func TestEvidenceRetentionBelowExecutionBudget(t *testing.T) {
	t.Parallel()
	for _, trials := range []int{2, 4} {
		scenario := scenarioWithoutSetup()
		scenario.Attempts, scenario.Concurrency = 8, 2
		scenario.Observation = nil
		scenario.Invariant = engine.Invariant{MaximumSuccessfulAttempts: &engine.MaximumSuccessfulAttemptsInvariant{Name: "At most eight replies", Maximum: 8}}
		scenario.Operation.Request.Body = []byte(strings.Repeat("r", 32<<10))
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return testHTTPResponse(200, strings.Repeat("x", 128<<10)), nil
		})}
		result, err := engine.RunTrials(context.Background(), client, scenario, trials)
		if err != nil {
			t.Fatal(err)
		}
		responseBytes, requestBytes, attempts := 0, 0, 0
		for _, trial := range result.Trials {
			for _, attempt := range trial.Run.History.Attempts {
				attempts++
				responseBytes += len(attempt.Execution.Response.Body)
				requestBytes += len(attempt.Execution.Request.Body)
			}
		}
		t.Logf("trials=%d attempts=%d retained response bytes=%d retained request bytes=%d", trials, attempts, responseBytes, requestBytes)
		if responseBytes != trials*8*(128<<10) || requestBytes != trials*8*(32<<10) || result.Status != engine.TrialStatusPassed {
			t.Fatalf("unexpected bounded retention: %+v", result)
		}
	}
}

func TestExecutionOwnerSnapshotAndClose(t *testing.T) {
	t.Parallel()
	s := boundedHistoryScenario()
	owner, err := engine.NewExecution(context.Background(), s.Limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	snapshot := owner.Snapshot()
	snapshot.Limits.MaxExchanges = 1
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return testHTTPResponse(200, ""), nil })}
	result, err := owner.RunTrials(client, s, 1)
	if err != nil || result.Resources.Exchanges != 2 {
		t.Fatalf("snapshot mutated owner: %+v %v", result.Resources, err)
	}
	owner.Close()
	frozen := owner.Snapshot()
	owner.Close()
	if owner.Snapshot().Elapsed != frozen.Elapsed {
		t.Fatal("closed execution clock kept advancing")
	}
	if _, err := owner.RunTrials(client, s, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("closed owner started work: %v", err)
	}
}

func TestFinalObservationBudgetKeepsKnownHistoryViolation(t *testing.T) {
	t.Parallel()
	for _, group := range []bool{false, true} {
		t.Run(fmt.Sprint(group), func(t *testing.T) {
			s := boundedHistoryScenario()
			s.Invariant.MaximumSuccessfulAttempts.Maximum = 0
			if group {
				s.Invariant = engine.Invariant{All: []engine.Invariant{s.Invariant, integerCheck("State", "stock", 0)}}
			}
			s.Observation = &engine.HTTPRequest{Method: http.MethodGet, URL: "http://example.test/state"}
			s.Limits.MaxRetainedBytes = 500
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/state" {
					return testHTTPResponse(200, `{"stock":0}`+strings.Repeat(" ", 1000)), nil
				}
				return testHTTPResponse(201, ""), nil
			})}
			result, err := engine.RunTrials(context.Background(), client, s, 3)
			if err == nil || len(result.Trials) != 1 || result.Status != engine.TrialStatusViolated || result.Trials[0].Run.Outcome != engine.RunOutcomeViolated || result.Resources.Exhausted != "max_retained_bytes" {
				t.Fatalf("final observation hid known violation: %+v %v", result, err)
			}
		})
	}
}

func TestDirectRunCannotPassAfterItsDeadline(t *testing.T) {
	t.Parallel()
	s := scenarioWithoutSetup()
	s.Limits.MaxDuration = 10 * time.Millisecond
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/state" {
			<-r.Context().Done()
			return testHTTPResponse(200, `{"stock":0}`), nil
		}
		return testHTTPResponse(200, ""), nil
	})}
	result, err := engine.Run(context.Background(), client, s)
	if err == nil || result.Outcome == engine.RunOutcomePassed || result.Resources.Exhausted != "max_duration" {
		t.Fatalf("late direct result became a clean pass: %+v %v", result, err)
	}
}

func TestOversizedRequestAndResponseHeadersAreNotCloned(t *testing.T) {
	t.Parallel()
	var calls, closed atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		r := testHTTPResponse(200, "")
		r.Header = http.Header{"X-Large": {strings.Repeat("h", engine.MaxHTTPHeaderBytes+1)}}
		r.Body = &closingBudgetBody{ReadCloser: r.Body, closed: &closed}
		return r, nil
	})}
	request := engine.HTTPRequest{Method: http.MethodPost, URL: "http://example.test/operation", Body: make([]byte, engine.MaxHTTPBodyBytes+1)}
	execution := engine.ExecuteHTTP(context.Background(), client, request)
	if execution.Err == nil || calls.Load() != 0 || !execution.RequestDataTruncated || len(execution.Request.Body) != 0 {
		t.Fatal("oversized request was copied or sent")
	}
	request.Body = nil
	execution = engine.ExecuteHTTP(context.Background(), client, request)
	if execution.Err == nil || calls.Load() != 1 || closed.Load() != 1 || !execution.Response.EvidenceTruncated || execution.Response.Header != nil {
		t.Fatal("oversized response headers were cloned or not closed")
	}
}

func boundedHistoryScenario() engine.Scenario {
	s := scenarioWithoutSetup()
	s.Attempts, s.Concurrency, s.Observation = 2, 1, nil
	s.Invariant = engine.Invariant{MaximumSuccessfulAttempts: &engine.MaximumSuccessfulAttemptsInvariant{Name: "Response limit", Maximum: 2}}
	return s
}

func TestCumulativeEvidenceBudgetStopsTrialsAndMarksPartialBody(t *testing.T) {
	t.Parallel()
	s := boundedHistoryScenario()
	s.Limits.MaxRetainedBytes = 5000
	var calls, closed atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		r := testHTTPResponse(200, strings.Repeat("x", 1000))
		r.Body = &closingBudgetBody{ReadCloser: r.Body, closed: &closed}
		return r, nil
	})}
	result, err := engine.RunTrials(context.Background(), client, s, 10)
	if err == nil || errors.Is(err, context.Canceled) || len(result.Trials) >= 10 || result.Resources.Exhausted != "max_retained_bytes" || result.Resources.RetainedBytes > 5000 || calls.Load() != closed.Load() {
		t.Fatalf("budget reset, leaked response, or wrong stop: %+v err=%v calls=%d closed=%d", result.Resources, err, calls.Load(), closed.Load())
	}
	found := false
	for _, trial := range result.Trials {
		for _, attempt := range trial.Run.History.Attempts {
			if attempt.Execution != nil && attempt.Execution.Response != nil && attempt.Execution.Response.EvidenceTruncated {
				found = true
				if !attempt.Execution.Response.BodyTruncated || attempt.Execution.Err == nil || cap(attempt.Execution.Response.Body) > 2048 {
					t.Fatal("partial evidence retained a large backing buffer or hid truncation")
				}
			}
		}
	}
	if !found || result.Status == engine.TrialStatusPassed {
		t.Fatal("exhaustion became a clean pass or lost evidence markers")
	}
}

type closingBudgetBody struct {
	io.ReadCloser
	closed *atomic.Int32
}

func (b *closingBudgetBody) Close() error { b.closed.Add(1); return b.ReadCloser.Close() }

func TestWorkBudgetBoundsCumulativeAttemptSlotsAndExchangeAdmissions(t *testing.T) {
	t.Parallel()
	for _, resource := range []string{"max_total_attempts", "max_exchanges"} {
		t.Run(resource, func(t *testing.T) {
			s := boundedHistoryScenario()
			if resource == "max_total_attempts" {
				s.Limits.MaxTotalAttempts = 5
			} else {
				s.Limits.MaxExchanges = 5
			}
			var calls atomic.Int32
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { calls.Add(1); return testHTTPResponse(200, ""), nil })}
			result, err := engine.RunTrials(context.Background(), client, s, 10)
			if err == nil || result.Resources.Exhausted != resource || calls.Load() > 5 || result.Resources.Exchanges > 5 || len(result.Trials) != 3 {
				t.Fatalf("cumulative work escaped budget: %+v err=%v calls=%d", result.Resources, err, calls.Load())
			}
			if resource == "max_total_attempts" && result.Resources.AttemptSlots != 4 {
				t.Fatal("attempt array allocated before checking remaining slots")
			}
		})
	}
}

func TestResourceExhaustionRetainsAlreadyDemonstratedHistoryViolation(t *testing.T) {
	t.Parallel()
	s := boundedHistoryScenario()
	s.Limits.MaxExchanges = 1
	s.Invariant.MaximumSuccessfulAttempts.Maximum = 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return testHTTPResponse(201, ""), nil })}
	result, err := engine.RunTrials(context.Background(), client, s, 3)
	if err == nil || len(result.Trials) != 1 || result.Status != engine.TrialStatusViolated || result.Trials[0].Run.Evaluation == nil || !result.Trials[0].Run.Evaluation.Violated || result.Resources.Exhausted != "max_exchanges" {
		t.Fatalf("budget hid a known failure: %+v error=%v", result, err)
	}
}

func TestBudgetsCoverSetupAndReadinessPolls(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"setup", "readiness"} {
		t.Run(stage, func(t *testing.T) {
			s := boundedHistoryScenario()
			s.Limits.MaxExchanges = 2
			if stage == "readiness" {
				s.Readiness = pendingCondition("/ready")
				s.Readiness.Interval = time.Nanosecond
			} else {
				for range 3 {
					s.Setup = append(s.Setup, engine.SetupStep{Request: engine.HTTPRequest{Method: http.MethodPost, URL: "http://example.test/setup"}})
				}
			}
			var calls atomic.Int32
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				if r.URL.Path == "/purchase" {
					t.Error("operations ran after preparation exhausted budget")
				}
				return testHTTPResponse(200, `{"pending":1}`), nil
			})}
			result, err := engine.RunTrials(context.Background(), client, s, 3)
			if err == nil || calls.Load() != 2 || len(result.Trials) != 1 || result.Resources.Exhausted != "max_exchanges" {
				t.Fatalf("preparation bypassed budget: %+v error=%v", result, err)
			}
		})
	}
}

func TestDurationBudgetPreservesParentCancellationIdentity(t *testing.T) {
	t.Parallel()
	for _, parent := range []bool{false, true} {
		t.Run(fmt.Sprint(parent), func(t *testing.T) {
			s := boundedHistoryScenario()
			s.Limits.MaxDuration = 10 * time.Millisecond
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if parent {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 10*time.Millisecond)
				s.Limits.MaxDuration = time.Second
			}
			defer cancel()
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })}
			result, err := engine.RunTrials(ctx, client, s, 3)
			if err == nil || len(result.Trials) != 1 {
				t.Fatalf("duration did not stop work: %+v %v", result, err)
			}
			if parent {
				if !errors.Is(err, context.DeadlineExceeded) || result.Resources.Exhausted != "" {
					t.Fatalf("parent deadline mislabeled: %+v %v", result.Resources, err)
				}
			} else if errors.Is(err, context.DeadlineExceeded) || result.Resources.Exhausted != "max_duration" {
				t.Fatalf("execution deadline mislabeled: %+v %v", result.Resources, err)
			}
		})
	}
}

func TestInvalidWorkAndLimitsAllocateNoHistoryAndSendNoTraffic(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { calls.Add(1); return testHTTPResponse(200, ""), nil })}
	for _, counts := range [][2]int{{10001, 1}, {1000, 257}} {
		result, err := engine.ExecuteConcurrent(context.Background(), client, engine.Operation{Name: "operation", Request: engine.HTTPRequest{Method: "POST", URL: "http://example.test/operation"}}, counts[0], counts[1])
		if err == nil || len(result.Attempts) != 0 || calls.Load() != 0 {
			t.Fatal("oversized work allocated history or sent traffic")
		}
	}
	for _, limits := range []engine.Limits{{MaxAttempts: -1}, {MaxConcurrency: 257}, {MaxTotalAttempts: 100001}, {MaxExchanges: 100001}, {MaxRetainedBytes: 65 << 20}, {MaxDuration: 31 * time.Minute}, {MaxReportBytes: 65 << 20}} {
		s := boundedHistoryScenario()
		s.Limits = limits
		if _, err := engine.Run(context.Background(), client, s); err == nil || calls.Load() != 0 {
			t.Fatal("invalid limits allowed traffic")
		}
	}
}
