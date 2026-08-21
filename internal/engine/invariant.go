package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// JSONIntegerMinimumInvariant requires one top-level JSON integer field to be
// greater than or equal to Minimum.
type JSONIntegerMinimumInvariant struct {
	Name    string
	Field   string
	Minimum int64
}

// InvariantEvaluation records the value observed for one invariant.
type InvariantEvaluation struct {
	Invariant JSONIntegerMinimumInvariant
	Observed  int64
	Violated  bool
}

// EvaluateJSONIntegerMinimum evaluates an invariant against one JSON document.
func EvaluateJSONIntegerMinimum(
	invariant JSONIntegerMinimumInvariant,
	document []byte,
) (InvariantEvaluation, error) {
	if err := validateJSONIntegerMinimumInvariant(invariant); err != nil {
		return InvariantEvaluation{}, err
	}

	decoder := json.NewDecoder(bytes.NewReader(document))
	var object map[string]json.RawMessage
	if err := decoder.Decode(&object); err != nil {
		return InvariantEvaluation{}, fmt.Errorf("decode observation as a JSON object: %w", err)
	}
	if object == nil {
		return InvariantEvaluation{}, errors.New("decode observation as a JSON object: got null")
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return InvariantEvaluation{}, errors.New("decode observation as a JSON object: multiple JSON values")
		}
		return InvariantEvaluation{}, fmt.Errorf("decode trailing observation data: %w", err)
	}

	rawValue, ok := object[invariant.Field]
	if !ok {
		return InvariantEvaluation{}, fmt.Errorf("observation field %q is missing", invariant.Field)
	}

	var observed *int64
	if err := json.Unmarshal(rawValue, &observed); err != nil {
		return InvariantEvaluation{}, fmt.Errorf(
			"observation field %q must be a JSON integer representable as int64: %w",
			invariant.Field,
			err,
		)
	}
	if observed == nil {
		return InvariantEvaluation{}, fmt.Errorf("observation field %q must not be null", invariant.Field)
	}

	return InvariantEvaluation{
		Invariant: invariant,
		Observed:  *observed,
		Violated:  *observed < invariant.Minimum,
	}, nil
}

func validateJSONIntegerMinimumInvariant(invariant JSONIntegerMinimumInvariant) error {
	if strings.TrimSpace(invariant.Name) == "" {
		return errors.New("evaluate JSON integer minimum invariant: empty name")
	}
	if strings.TrimSpace(invariant.Field) == "" {
		return errors.New("evaluate JSON integer minimum invariant: empty field")
	}
	return nil
}
