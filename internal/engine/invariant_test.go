package engine_test

import (
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/eumarumar/concurtest/internal/engine"
)

func TestEvaluateJSONIntegerMinimum(t *testing.T) {
	t.Parallel()

	invariant := engine.JSONIntegerMinimumInvariant{
		Name:    "stock must be non-negative",
		Path:    []string{"data", "quantity"},
		Minimum: 0,
	}
	tests := []struct {
		name          string
		path          []string
		document      string
		observed      int64
		violated      bool
		wantErr       bool
		wantErrorText string
	}{
		{name: "above minimum", document: `{"data":{"quantity":2}}`, observed: 2},
		{name: "equal to minimum", document: `{"data":{"quantity":0}}`, observed: 0},
		{name: "below minimum", document: `{"data":{"quantity":-1}}`, observed: -1, violated: true},
		{name: "top-level path", path: []string{"stock"}, document: `{"stock":1}`, observed: 1},
		{name: "literal dot in segment", path: []string{"data.quantity"}, document: `{"data.quantity":3}`, observed: 3},
		{name: "checkout quantity", path: []string{"data", "Products", "0", "BasketItem", "quantity"}, document: `{"data":{"Products":[{"BasketItem":{"quantity":-1}}]}}`, observed: -1, violated: true},
		{name: "second array element", path: []string{"data", "1", "quantity"}, document: `{"data":[{"quantity":-1},{"quantity":2}]}`, observed: 2},
		{name: "array integer leaf", path: []string{"data", "0"}, document: `{"data":[0]}`, observed: 0},
		{name: "nested arrays", path: []string{"data", "0", "1"}, document: `{"data":[[1,2]]}`, observed: 2},
		{name: "root array", path: []string{"0", "quantity"}, document: `[{"quantity":2}]`, observed: 2},
		{name: "numeric object key", path: []string{"data", "0", "quantity"}, document: `{"data":{"0":{"quantity":3}}}`, observed: 3},
		{name: "literal noncanonical numeric key", path: []string{"data", "01"}, document: `{"data":{"01":3}}`, observed: 3},
		{name: "large exact array integer", path: []string{"data", "0"}, document: `{"data":[9223372036854775807]}`, observed: 9223372036854775807},
		{name: "empty array", path: []string{"data", "0"}, document: `{"data":[]}`, wantErr: true, wantErrorText: `observation path $["data"]["0"] is out of range: array has 0 elements`},
		{name: "array index out of range", path: []string{"data", "1"}, document: `{"data":[0]}`, wantErr: true, wantErrorText: "out of range"},
		{name: "negative array index", path: []string{"data", "-1"}, document: `{"data":[0]}`, wantErr: true, wantErrorText: "needs a non-negative base-10 array index"},
		{name: "overflowing array index", path: []string{"data", "99999999999999999999999"}, document: `{"data":[0]}`, wantErr: true, wantErrorText: "needs a non-negative base-10 array index"},
		{name: "fractional array index", path: []string{"data", "0.5"}, document: `{"data":[0]}`, wantErr: true, wantErrorText: "needs a non-negative base-10 array index"},
		{name: "leading zero array index", path: []string{"data", "01"}, document: `{"data":[0,1]}`, wantErr: true, wantErrorText: "needs a non-negative base-10 array index"},
		{name: "null array element", path: []string{"data", "0", "quantity"}, document: `{"data":[null]}`, wantErr: true, wantErrorText: `observation path $["data"]["0"] must contain a JSON object or array, not null`},
		{name: "scalar array element", path: []string{"data", "0", "quantity"}, document: `{"data":[2]}`, wantErr: true, wantErrorText: `observation path $["data"]["0"] must contain a JSON object or array`},
		{name: "missing key in array element", path: []string{"data", "0", "quantity"}, document: `{"data":[{}]}`, wantErr: true, wantErrorText: `observation path $["data"]["0"]["quantity"] is missing`},
		{name: "non-integer array leaf", path: []string{"data", "0"}, document: `{"data":[1.5]}`, wantErr: true, wantErrorText: "must contain a JSON integer"},
		{name: "malformed JSON", document: `{"data":`, wantErr: true},
		{name: "top-level array", document: `[0]`, wantErr: true},
		{name: "top-level null", document: `null`, wantErr: true},
		{name: "missing first segment", document: `{"status":"success"}`, wantErr: true},
		{name: "missing final segment", document: `{"data":{"remaining":0}}`, wantErr: true, wantErrorText: `observation path $["data"]["quantity"] is missing`},
		{name: "null intermediate segment", document: `{"data":null}`, wantErr: true, wantErrorText: `observation path $["data"] must contain a JSON object or array, not null`},
		{name: "string intermediate segment", document: `{"data":"value"}`, wantErr: true},
		{name: "array needs explicit index", document: `{"data":[{"quantity":0}]}`, wantErr: true, wantErrorText: "needs a non-negative base-10 array index"},
		{name: "null value", document: `{"data":{"quantity":null}}`, wantErr: true, wantErrorText: `observation path $["data"]["quantity"] must contain an integer, not null`},
		{name: "string value", document: `{"data":{"quantity":"0"}}`, wantErr: true, wantErrorText: `observation path $["data"]["quantity"] must contain a JSON integer`},
		{name: "boolean value", document: `{"data":{"quantity":true}}`, wantErr: true},
		{name: "object value", document: `{"data":{"quantity":{}}}`, wantErr: true},
		{name: "array value", document: `{"data":{"quantity":[0]}}`, wantErr: true},
		{name: "decimal value", document: `{"data":{"quantity":1.0}}`, wantErr: true},
		{name: "overflowing value", document: `{"data":{"quantity":9223372036854775808}}`, wantErr: true},
		{name: "trailing JSON value", document: `{"data":{"quantity":0}} {"data":{"quantity":1}}`, wantErr: true},
		{name: "trailing invalid data", document: `{"data":{"quantity":0}} invalid`, wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			definition := invariant
			if test.path != nil {
				definition.Path = test.path
			}
			evaluation, err := engine.EvaluateJSONIntegerMinimum(definition, []byte(test.document))
			if test.wantErr {
				if err == nil {
					t.Fatal("EvaluateJSONIntegerMinimum() error = nil, want error")
				}
				if test.wantErrorText != "" && !strings.Contains(err.Error(), test.wantErrorText) {
					t.Errorf("EvaluateJSONIntegerMinimum() error = %q, want text %q", err, test.wantErrorText)
				}
				return
			}
			if err != nil {
				t.Fatalf("EvaluateJSONIntegerMinimum() error = %v", err)
			}
			if !reflect.DeepEqual(evaluation.Invariant, definition) {
				t.Errorf("evaluation invariant = %#v, want %#v", evaluation.Invariant, definition)
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

func TestEvaluateJSONIntegerMinimumCopiesInvariantPath(t *testing.T) {
	t.Parallel()

	invariant := engine.JSONIntegerMinimumInvariant{
		Name: "quantity must be non-negative", Path: []string{"data", "quantity"}, Minimum: 0,
	}
	evaluation, err := engine.EvaluateJSONIntegerMinimum(invariant, []byte(`{"data":{"quantity":1}}`))
	if err != nil {
		t.Fatalf("EvaluateJSONIntegerMinimum() error = %v", err)
	}
	invariant.Path[0] = "changed"
	if evaluation.Invariant.Path[0] != "data" {
		t.Errorf("evaluation path changed to %q", evaluation.Invariant.Path[0])
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
		{Path: []string{"stock"}},
		{Name: "stock must be non-negative"},
		{Name: " ", Path: []string{"stock"}},
		{Name: "stock must be non-negative", Path: []string{}},
		{Name: "stock must be non-negative", Path: []string{" "}},
		{Name: "stock must be non-negative", Path: []string{"data", ""}},
	}

	for _, invariant := range tests {
		if _, err := engine.EvaluateJSONIntegerMinimum(invariant, []byte(`{"stock":0}`)); err == nil {
			t.Errorf("EvaluateJSONIntegerMinimum(%#v) error = nil, want validation error", invariant)
		}
	}
}
