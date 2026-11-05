package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/eumarumar/concurtest/internal/failure"
)

// ResponseAssertion checks one scalar in a setup or precondition response.
// Exactly one definition is required. Integer Change is not supported because
// preparation checks assert an absolute precondition, not a later state change.
type ResponseAssertion struct {
	JSONInteger *JSONIntegerInvariant
	JSONBoolean *JSONBooleanAssertion
}

// JSONBooleanAssertion requires a literal Boolean at the selected JSON path.
type JSONBooleanAssertion struct {
	Name   string
	Path   []string
	Equals bool
}

// AssertionEvaluation retains a snapshot of the check and the observed scalar.
// Successful decoding populates exactly one observed pointer, including false
// and integer zero. Both are nil when the response cannot be verified.
type AssertionEvaluation struct {
	Assertion       ResponseAssertion
	ObservedInteger *int64
	ObservedBoolean *bool
	Satisfied       bool
}

// Name identifies the single declared check.
func (a ResponseAssertion) Name() string {
	if a.JSONInteger != nil {
		return a.JSONInteger.Name
	}
	if a.JSONBoolean != nil {
		return a.JSONBoolean.Name
	}
	return ""
}

// Validate rejects unsupported or ambiguous preparation checks before traffic.
func (a ResponseAssertion) Validate() error {
	if (a.JSONInteger == nil) == (a.JSONBoolean == nil) {
		return errors.New("assertion requires exactly one JSON integer or Boolean check")
	}
	if a.JSONInteger != nil {
		if err := validateJSONIntegerInvariant(*a.JSONInteger); err != nil {
			return err
		}
		if a.JSONInteger.Change != nil {
			return errors.New("assertion requires absolute bounds or equality, not change")
		}
		return nil
	}
	if strings.TrimSpace(a.JSONBoolean.Name) == "" || len(a.JSONBoolean.Path) == 0 {
		return errors.New("Boolean assertion requires a name and a non-empty JSON path")
	}
	for _, segment := range a.JSONBoolean.Path {
		if strings.TrimSpace(segment) == "" {
			return errors.New("Boolean assertion path must not contain empty segments")
		}
	}
	return nil
}

// EvaluateResponseAssertion evaluates a complete JSON document, using the same
// exact integer and object/array path semantics as scenario invariants.
func EvaluateResponseAssertion(assertion ResponseAssertion, document []byte) (AssertionEvaluation, error) {
	if err := assertion.Validate(); err != nil {
		return AssertionEvaluation{}, err
	}
	result := AssertionEvaluation{Assertion: cloneResponseAssertion(assertion)}
	if assertion.JSONInteger != nil {
		evaluation, err := EvaluateJSONInteger(*assertion.JSONInteger, document)
		if err != nil {
			return result, err
		}
		result.ObservedInteger = new(evaluation.Observed)
		result.Satisfied = !evaluation.Violated
		return result, nil
	}
	definition := result.Assertion.JSONBoolean
	raw, err := readJSONAtPath(definition.Path, document)
	if err != nil {
		return result, err
	}
	var observed *bool
	if err := json.Unmarshal(raw, &observed); err != nil {
		return result, fmt.Errorf("response path %s must contain a Boolean: %w", formatJSONPath(definition.Path), err)
	}
	if observed == nil {
		return result, fmt.Errorf("response path %s must contain a Boolean, not null", formatJSONPath(definition.Path))
	}
	result.ObservedBoolean = observed
	result.Satisfied = *observed == definition.Equals
	return result, nil
}

func cloneResponseAssertion(assertion ResponseAssertion) ResponseAssertion {
	if assertion.JSONInteger != nil {
		definition := cloneJSONIntegerInvariant(*assertion.JSONInteger)
		return ResponseAssertion{JSONInteger: &definition}
	}
	definition := *assertion.JSONBoolean
	definition.Path = slices.Clone(definition.Path)
	return ResponseAssertion{JSONBoolean: &definition}
}

// The caller has already required a successful exchange and status. Retain a
// mismatched evaluation, but do not evaluate incomplete response bodies.
func checkResponseAssertion(stage string, assertion ResponseAssertion, execution HTTPExecution) (*AssertionEvaluation, error) {
	if execution.Response.BodyTruncated {
		return &AssertionEvaluation{Assertion: cloneResponseAssertion(assertion)}, failure.Wrap(failure.CodePreconditionFailed, stage+": response assertion could not be checked",
			failure.New(failure.CodeResponseTruncated, "response body was truncated"))
	}
	evaluation, err := EvaluateResponseAssertion(assertion, execution.Response.Body)
	if err != nil {
		return &evaluation, failure.Wrap(failure.CodePreconditionFailed, stage+": response assertion could not be checked", err)
	}
	if !evaluation.Satisfied {
		return &evaluation, failure.New(failure.CodePreconditionFailed, fmt.Sprintf("%s: assertion %q did not match", stage, assertion.Name()))
	}
	return &evaluation, nil
}
