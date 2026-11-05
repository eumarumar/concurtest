package engine_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/eumarumar/concurtest/internal/engine"
)

func TestEvaluateOutcomesPartitionsEvidence(t *testing.T) {
	t.Parallel()
	history := engine.History{Attempts: []engine.Attempt{
		{ID: 1, Execution: executionWithStatus(201)},
		{ID: 2, Execution: executionWithStatus(409)},
		{ID: 3, Execution: executionWithStatus(500)},
		{ID: 4, Execution: nil},
		{ID: 5, Execution: &engine.HTTPExecution{Err: errors.New("connection failed")}},
		{ID: 6, Execution: &engine.HTTPExecution{}},
		{ID: 7, Execution: &engine.HTTPExecution{Response: &engine.HTTPResponse{StatusCode: 201}, Err: errors.New("response read failed")}},
	}}
	policy := engine.OutcomePolicy{AcceptedStatusCodes: []int{201}, RejectedStatusCodes: []int{409}, MinimumAcceptedAttempts: new(1)}
	evaluation, err := engine.EvaluateOutcomes(policy, history)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(evaluation.AcceptedAttemptIDs, []int{1}) || !slices.Equal(evaluation.RejectedAttemptIDs, []int{2}) ||
		!slices.Equal(evaluation.UnexpectedAttemptIDs, []int{3}) || !slices.Equal(evaluation.UnknownAttemptIDs, []int{4, 5, 6, 7}) || evaluation.Trustworthy() {
		t.Fatalf("unexpected outcome classification: %+v", evaluation)
	}
	policy.AcceptedStatusCodes[0], policy.RejectedStatusCodes[0], *policy.MinimumAcceptedAttempts = 202, 400, 7
	if evaluation.Policy.AcceptedStatusCodes[0] != 201 || evaluation.Policy.RejectedStatusCodes[0] != 409 || evaluation.Policy.MinimumAccepted() != 1 {
		t.Fatal("evaluation shares mutable policy with caller")
	}
}

func TestRunTrialsRequiresCoverageWithinEachTrial(t *testing.T) {
	t.Parallel()
	var trial atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/setup" {
			trial.Add(1)
			return testHTTPResponse(http.StatusNoContent, ""), nil
		}
		if trial.Load() == 1 {
			return testHTTPResponse(http.StatusCreated, "accepted"), nil
		}
		return testHTTPResponse(http.StatusConflict, "expected rejection"), nil
	})}
	scenario := maximumSuccessfulAttemptsScenario(2)
	scenario.Attempts, scenario.Concurrency = 2, 2
	scenario.Setup = []engine.SetupStep{{Request: engine.HTTPRequest{Method: http.MethodPost, URL: "http://example.test/setup"}}}
	scenario.Outcomes.RejectedStatusCodes = []int{409}
	result, err := engine.RunTrials(context.Background(), client, scenario, 2)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != engine.TrialStatusInconclusive || len(result.Trials) != 2 ||
		result.Trials[0].Status != engine.TrialStatusPassed || result.Trials[1].Status != engine.TrialStatusInconclusive ||
		result.Trials[1].Run.Outcomes.Trustworthy() {
		t.Fatalf("coverage carried over between trials: %+v", result)
	}
}

func TestRunRejectsInvalidOutcomePolicyBeforeSendingRequests(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return testHTTPResponse(http.StatusOK, ""), nil
	})}
	scenario := maximumSuccessfulAttemptsScenario(1)
	scenario.Setup = []engine.SetupStep{{Request: scenario.Operation.Request}}
	// Acceptance inherits 201 from the history filter, so this overlap must be
	// rejected by the programmatic runner as well as the YAML decoder.
	scenario.Outcomes.RejectedStatusCodes = []int{201}
	if _, err := engine.Run(context.Background(), client, scenario); err == nil || calls.Load() != 0 {
		t.Fatalf("invalid policy error=%v, sent requests=%d", err, calls.Load())
	}
}

func TestOutcomeCoverageAndExpectedRejections(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		policy engine.OutcomePolicy
		codes  []int
		met    bool
	}{
		{"default success", engine.OutcomePolicy{}, []int{201, 204}, true},
		{"default unexpected unauthorized", engine.OutcomePolicy{}, []int{201, 401}, false},
		{"default unexpected redirect", engine.OutcomePolicy{}, []int{302}, false},
		{"expected conflict", engine.OutcomePolicy{RejectedStatusCodes: []int{409}}, []int{201, 409}, true},
		{"expected bad request", engine.OutcomePolicy{RejectedStatusCodes: []int{400}}, []int{201, 400}, true},
		{"all expected conflicts still lack coverage", engine.OutcomePolicy{RejectedStatusCodes: []int{409}}, []int{409, 409}, false},
		{"explicit rejection-only test", engine.OutcomePolicy{RejectedStatusCodes: []int{409}, MinimumAcceptedAttempts: new(0)}, []int{409, 409}, true},
		{"minimum cannot excuse unexpected status", engine.OutcomePolicy{MinimumAcceptedAttempts: new(0)}, []int{500}, false},
		{"explicit accepted contract", engine.OutcomePolicy{AcceptedStatusCodes: []int{409}}, []int{409}, true},
		{"explicit accepted statuses exclude empty response", engine.OutcomePolicy{AcceptedStatusCodes: []int{201}, RejectedStatusCodes: []int{204}}, []int{204, 204}, false},
		{"minimum across all attempts", engine.OutcomePolicy{MinimumAcceptedAttempts: new(2), RejectedStatusCodes: []int{409}}, []int{201, 409}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			history := engine.History{}
			for i, code := range test.codes {
				history.Attempts = append(history.Attempts, engine.Attempt{ID: i + 1, Execution: executionWithStatus(code)})
			}
			evaluation, err := engine.EvaluateOutcomes(test.policy, history)
			if err != nil {
				t.Fatal(err)
			}
			if evaluation.Trustworthy() != test.met {
				t.Fatalf("coverage=%t, want %t: %+v", evaluation.Trustworthy(), test.met, evaluation)
			}
		})
	}
}

func TestOutcomePolicyValidation(t *testing.T) {
	t.Parallel()
	for _, policy := range []engine.OutcomePolicy{
		{AcceptedStatusCodes: []int{}},
		{AcceptedStatusCodes: []int{201, 201}},
		{RejectedStatusCodes: []int{409, 409}},
		{AcceptedStatusCodes: []int{99}},
		{RejectedStatusCodes: []int{600}},
		{RejectedStatusCodes: []int{http.StatusNoContent}},
		{AcceptedStatusCodes: []int{409}, RejectedStatusCodes: []int{409}},
		{MinimumAcceptedAttempts: new(-1)},
		{MinimumAcceptedAttempts: new(3)},
	} {
		if err := policy.Validate(2); err == nil {
			t.Fatalf("accepted invalid policy: %+v", policy)
		}
	}
}
