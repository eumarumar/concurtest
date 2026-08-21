package engine_test

import (
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
