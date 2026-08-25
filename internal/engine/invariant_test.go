package engine_test

import (
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/eumarumar/concurtest/internal/engine"
)

func TestEvaluateJSONIntegerMinimum(t *testing.T) {
	t.Parallel()

	invariant := engine.JSONIntegerMinimumInvariant{
		Name:    "stock must be non-negative",
		Field:   "stock",
		Minimum: 0,
	}
	tests := []struct {
		name     string
		document string
		observed int64
		violated bool
		wantErr  bool
	}{
		{name: "above minimum", document: `{"stock":2}`, observed: 2},
		{name: "equal to minimum", document: `{"stock":0}`, observed: 0},
		{name: "below minimum", document: `{"stock":-1}`, observed: -1, violated: true},
		{name: "malformed JSON", document: `{"stock":`, wantErr: true},
		{name: "top-level array", document: `[0]`, wantErr: true},
		{name: "top-level null", document: `null`, wantErr: true},
		{name: "missing field", document: `{"remaining":0}`, wantErr: true},
		{name: "null field", document: `{"stock":null}`, wantErr: true},
		{name: "string field", document: `{"stock":"0"}`, wantErr: true},
		{name: "boolean field", document: `{"stock":true}`, wantErr: true},
		{name: "object field", document: `{"stock":{}}`, wantErr: true},
		{name: "array field", document: `{"stock":[0]}`, wantErr: true},
		{name: "decimal field", document: `{"stock":1.0}`, wantErr: true},
		{name: "overflowing field", document: `{"stock":9223372036854775808}`, wantErr: true},
		{name: "trailing JSON value", document: `{"stock":0} {"stock":1}`, wantErr: true},
		{name: "trailing invalid data", document: `{"stock":0} invalid`, wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evaluation, err := engine.EvaluateJSONIntegerMinimum(invariant, []byte(test.document))
			if test.wantErr {
				if err == nil {
					t.Fatal("EvaluateJSONIntegerMinimum() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("EvaluateJSONIntegerMinimum() error = %v", err)
			}
			if evaluation.Invariant != invariant {
				t.Errorf("evaluation invariant = %#v, want %#v", evaluation.Invariant, invariant)
			}
			if evaluation.Observed != test.observed {
				t.Errorf("observed = %d, want %d", evaluation.Observed, test.observed)
			}
			if evaluation.Violated != test.violated {
				t.Errorf("violated = %t, want %t", evaluation.Violated, test.violated)
			}
		})
	}
}

func TestEvaluateMaximumSuccessfulAttempts(t *testing.T) {
	t.Parallel()

	transportErr := errors.New("network unavailable")
	history := engine.History{Attempts: []engine.Attempt{
		{ID: 1, Execution: executionWithStatus(http.StatusCreated)},
		{ID: 2, Execution: executionWithStatus(http.StatusConflict)},
		{ID: 3, Execution: &engine.HTTPExecution{Err: transportErr}},
		{ID: 4},
		{ID: 5, Execution: executionWithStatus(http.StatusOK)},
	}}

	tests := []struct {
		name       string
		invariant  engine.MaximumSuccessfulAttemptsInvariant
		successful []int
		overLimit  []int
		violated   bool
	}{
		{
			name: "default 2xx statuses",
			invariant: engine.MaximumSuccessfulAttemptsInvariant{
				Name:    "at most one accepted purchase",
				Maximum: 1,
			},
			successful: []int{1, 5},
			overLimit:  []int{5},
			violated:   true,
		},
		{
			name: "explicit status can be outside 2xx",
			invariant: engine.MaximumSuccessfulAttemptsInvariant{
				Name:                  "at most one conflict",
				Maximum:               1,
				SuccessfulStatusCodes: []int{http.StatusConflict},
			},
			successful: []int{2},
		},
		{
			name: "zero maximum",
			invariant: engine.MaximumSuccessfulAttemptsInvariant{
				Name:                  "no accepted purchases",
				Maximum:               0,
				SuccessfulStatusCodes: []int{http.StatusCreated},
			},
			successful: []int{1},
			overLimit:  []int{1},
			violated:   true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evaluation, err := engine.EvaluateMaximumSuccessfulAttempts(test.invariant, history)
			if err != nil {
				t.Fatalf("EvaluateMaximumSuccessfulAttempts() error = %v", err)
			}
			if !reflect.DeepEqual(evaluation.SuccessfulAttemptIDs, test.successful) {
				t.Errorf("successful attempt IDs = %v, want %v", evaluation.SuccessfulAttemptIDs, test.successful)
			}
			if !reflect.DeepEqual(evaluation.OverLimitAttemptIDs, test.overLimit) {
				t.Errorf("over-limit attempt IDs = %v, want %v", evaluation.OverLimitAttemptIDs, test.overLimit)
			}
			if evaluation.Violated != test.violated {
				t.Errorf("violated = %t, want %t", evaluation.Violated, test.violated)
			}
		})
	}
}

func TestEvaluateMaximumSuccessfulAttemptsRejectsInvalidDefinition(t *testing.T) {
	t.Parallel()

	tests := []engine.MaximumSuccessfulAttemptsInvariant{
		{Maximum: 1},
		{Name: "empty explicit statuses", Maximum: 1, SuccessfulStatusCodes: []int{}},
		{Name: "negative maximum", Maximum: -1},
		{Name: "low status", Maximum: 1, SuccessfulStatusCodes: []int{99}},
		{Name: "high status", Maximum: 1, SuccessfulStatusCodes: []int{600}},
		{Name: "duplicate status", Maximum: 1, SuccessfulStatusCodes: []int{201, 201}},
	}

	for _, invariant := range tests {
		if _, err := engine.EvaluateMaximumSuccessfulAttempts(invariant, engine.History{}); err == nil {
			t.Errorf("EvaluateMaximumSuccessfulAttempts(%#v) error = nil, want validation error", invariant)
		}
	}
}

func executionWithStatus(status int) *engine.HTTPExecution {
	return &engine.HTTPExecution{Response: &engine.HTTPResponse{StatusCode: status}}
}

func TestEvaluateJSONIntegerMinimumRejectsInvalidDefinition(t *testing.T) {
	t.Parallel()

	tests := []engine.JSONIntegerMinimumInvariant{
		{Field: "stock"},
		{Name: "stock must be non-negative"},
		{Name: " ", Field: "stock"},
		{Name: "stock must be non-negative", Field: " "},
	}

	for _, invariant := range tests {
		if _, err := engine.EvaluateJSONIntegerMinimum(invariant, []byte(`{"stock":0}`)); err == nil {
			t.Errorf("EvaluateJSONIntegerMinimum(%#v) error = nil, want validation error", invariant)
		}
	}
}
