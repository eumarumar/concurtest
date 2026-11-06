package engine_test

import (
	"math"
	"testing"

	"github.com/eumarumar/concurtest/internal/engine"
)

func booleanPreparationAssertion(want bool) *engine.ResponseAssertion {
	return &engine.ResponseAssertion{JSONBoolean: &engine.JSONBooleanAssertion{Name: "Reset acknowledged", Path: []string{"ok"}, Equals: want}}
}

func TestResponseAssertionsPreserveScalarTypesAndExactIntegers(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, body string
		assertion  engine.ResponseAssertion
		satisfied  bool
	}{
		{"true", `{"ok":true}`, *booleanPreparationAssertion(true), true},
		{"false is a value", `{"ok":false}`, *booleanPreparationAssertion(false), true},
		{"false mismatch", `{"ok":false}`, *booleanPreparationAssertion(true), false},
		{"array path", `{"items":[{"ready":true}]}`, engine.ResponseAssertion{JSONBoolean: &engine.JSONBooleanAssertion{Name: "Ready", Path: []string{"items", "0", "ready"}, Equals: true}}, true},
		{"exact int64", `{"count":9223372036854775807}`, engine.ResponseAssertion{JSONInteger: &engine.JSONIntegerInvariant{Name: "Count", Path: []string{"count"}, Equals: new(int64(math.MaxInt64))}}, true},
		{"zero", `{"count":0}`, engine.ResponseAssertion{JSONInteger: &engine.JSONIntegerInvariant{Name: "Count", Path: []string{"count"}, Equals: new(int64(0))}}, true},
		{"bounds mismatch", `{"count":3}`, engine.ResponseAssertion{JSONInteger: &engine.JSONIntegerInvariant{Name: "Count", Path: []string{"count"}, Minimum: new(int64(-2)), Maximum: new(int64(2))}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			evaluation, err := engine.EvaluateResponseAssertion(test.assertion, []byte(test.body))
			if err != nil || evaluation.Satisfied != test.satisfied {
				t.Fatalf("evaluation=%+v error=%v", evaluation, err)
			}
			if test.assertion.JSONBoolean != nil {
				test.assertion.JSONBoolean.Path[0] = "changed"
				if evaluation.Assertion.JSONBoolean.Path[0] == "changed" || evaluation.ObservedBoolean == nil {
					t.Fatal("Boolean check was not snapshotted or its value is missing")
				}
			} else {
				test.assertion.JSONInteger.Path[0] = "changed"
				if evaluation.Assertion.JSONInteger.Path[0] == "changed" || evaluation.ObservedInteger == nil {
					t.Fatal("integer check was not snapshotted or its value is missing")
				}
			}
		})
	}
}

func TestResponseAssertionsRejectUnreadableValues(t *testing.T) {
	t.Parallel()
	for _, body := range []string{`{`, `{}`, `{"ok":null}`, `{"ok":"true"}`, `{"ok":1}`, `{"ok":true} {}`, `{"ok":true} trailing`} {
		if evaluation, err := engine.EvaluateResponseAssertion(*booleanPreparationAssertion(true), []byte(body)); err == nil || evaluation.Satisfied || evaluation.ObservedBoolean != nil {
			t.Errorf("accepted unreadable Boolean %q: %+v error=%v", body, evaluation, err)
		}
	}
	assertion := engine.ResponseAssertion{JSONInteger: &engine.JSONIntegerInvariant{Name: "Count", Path: []string{"count"}, Equals: new(int64(0))}}
	for _, body := range []string{`{"count":0.0}`, `{"count":"0"}`, `{"count":null}`, `{"count":9223372036854775808}`} {
		if evaluation, err := engine.EvaluateResponseAssertion(assertion, []byte(body)); err == nil || evaluation.Satisfied || evaluation.ObservedInteger != nil {
			t.Errorf("accepted unreadable integer %q: %+v error=%v", body, evaluation, err)
		}
	}
}
