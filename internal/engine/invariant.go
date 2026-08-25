package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Invariant contains exactly one concrete invariant definition.
type Invariant struct {
	JSONIntegerMinimum        *JSONIntegerMinimumInvariant
	MaximumSuccessfulAttempts *MaximumSuccessfulAttemptsInvariant
}

// JSONIntegerMinimumInvariant requires one top-level JSON integer field to be
// greater than or equal to Minimum.
type JSONIntegerMinimumInvariant struct {
	Name    string
	Field   string
	Minimum int64
}

// JSONIntegerMinimumEvaluation records the value observed for one JSON integer
// minimum invariant.
type JSONIntegerMinimumEvaluation struct {
	Invariant JSONIntegerMinimumInvariant
	Observed  int64
	Violated  bool
}

// MaximumSuccessfulAttemptsInvariant limits operation responses whose HTTP
// status qualifies as successful. A nil SuccessfulStatusCodes slice means any
// 2xx response; an explicit list matches only those exact status codes.
type MaximumSuccessfulAttemptsInvariant struct {
	Name                  string
	Maximum               int
	SuccessfulStatusCodes []int
}

// MaximumSuccessfulAttemptsEvaluation records the stable IDs of every
// qualifying attempt and the suffix beyond the configured maximum.
type MaximumSuccessfulAttemptsEvaluation struct {
	Invariant            MaximumSuccessfulAttemptsInvariant
	SuccessfulAttemptIDs []int
	OverLimitAttemptIDs  []int
	Violated             bool
}

// InvariantEvaluation contains exactly one concrete evaluation.
type InvariantEvaluation struct {
	JSONIntegerMinimum        *JSONIntegerMinimumEvaluation
	MaximumSuccessfulAttempts *MaximumSuccessfulAttemptsEvaluation
	Violated                  bool
}

// EvaluateJSONIntegerMinimum evaluates an invariant against one JSON document.
func EvaluateJSONIntegerMinimum(
	invariant JSONIntegerMinimumInvariant,
	document []byte,
) (JSONIntegerMinimumEvaluation, error) {
	if err := validateJSONIntegerMinimumInvariant(invariant); err != nil {
		return JSONIntegerMinimumEvaluation{}, err
	}

	decoder := json.NewDecoder(bytes.NewReader(document))
	var object map[string]json.RawMessage
	if err := decoder.Decode(&object); err != nil {
		return JSONIntegerMinimumEvaluation{}, fmt.Errorf("decode observation as a JSON object: %w", err)
	}
	if object == nil {
		return JSONIntegerMinimumEvaluation{}, errors.New("decode observation as a JSON object: got null")
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return JSONIntegerMinimumEvaluation{}, errors.New("decode observation as a JSON object: multiple JSON values")
		}
		return JSONIntegerMinimumEvaluation{}, fmt.Errorf("decode trailing observation data: %w", err)
	}

	rawValue, ok := object[invariant.Field]
	if !ok {
		return JSONIntegerMinimumEvaluation{}, fmt.Errorf("observation field %q is missing", invariant.Field)
	}

	var observed *int64
	if err := json.Unmarshal(rawValue, &observed); err != nil {
		return JSONIntegerMinimumEvaluation{}, fmt.Errorf(
			"observation field %q must be a JSON integer representable as int64: %w",
			invariant.Field,
			err,
		)
	}
	if observed == nil {
		return JSONIntegerMinimumEvaluation{}, fmt.Errorf("observation field %q must not be null", invariant.Field)
	}

	return JSONIntegerMinimumEvaluation{
		Invariant: invariant,
		Observed:  *observed,
		Violated:  *observed < invariant.Minimum,
	}, nil
}

// EvaluateMaximumSuccessfulAttempts evaluates a completed operation history.
func EvaluateMaximumSuccessfulAttempts(
	invariant MaximumSuccessfulAttemptsInvariant,
	history History,
) (MaximumSuccessfulAttemptsEvaluation, error) {
	if err := validateMaximumSuccessfulAttemptsInvariant(invariant); err != nil {
		return MaximumSuccessfulAttemptsEvaluation{}, err
	}

	successful := make([]int, 0, len(history.Attempts))
	for _, attempt := range history.Attempts {
		if attempt.Execution == nil || attempt.Execution.Err != nil || attempt.Execution.Response == nil {
			continue
		}
		if successfulStatusCode(invariant.SuccessfulStatusCodes, attempt.Execution.Response.StatusCode) {
			successful = append(successful, attempt.ID)
		}
	}

	overLimit := []int(nil)
	if len(successful) > invariant.Maximum {
		overLimit = append([]int(nil), successful[invariant.Maximum:]...)
	}
	return MaximumSuccessfulAttemptsEvaluation{
		Invariant:            cloneMaximumSuccessfulAttemptsInvariant(invariant),
		SuccessfulAttemptIDs: successful,
		OverLimitAttemptIDs:  overLimit,
		Violated:             len(successful) > invariant.Maximum,
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

func validateInvariant(invariant Invariant) error {
	definitions := 0
	if invariant.JSONIntegerMinimum != nil {
		definitions++
		if err := validateJSONIntegerMinimumInvariant(*invariant.JSONIntegerMinimum); err != nil {
			return err
		}
	}
	if invariant.MaximumSuccessfulAttempts != nil {
		definitions++
		if err := validateMaximumSuccessfulAttemptsInvariant(*invariant.MaximumSuccessfulAttempts); err != nil {
			return err
		}
	}
	if definitions != 1 {
		return fmt.Errorf("exactly one invariant definition is required, got %d", definitions)
	}
	return nil
}

func validateMaximumSuccessfulAttemptsInvariant(invariant MaximumSuccessfulAttemptsInvariant) error {
	if strings.TrimSpace(invariant.Name) == "" {
		return errors.New("evaluate maximum successful attempts invariant: empty name")
	}
	if invariant.Maximum < 0 {
		return fmt.Errorf(
			"evaluate maximum successful attempts invariant: maximum must not be negative: %d",
			invariant.Maximum,
		)
	}
	if invariant.SuccessfulStatusCodes != nil && len(invariant.SuccessfulStatusCodes) == 0 {
		return errors.New("evaluate maximum successful attempts invariant: successful status codes must not be empty")
	}
	seen := make(map[int]struct{}, len(invariant.SuccessfulStatusCodes))
	for _, status := range invariant.SuccessfulStatusCodes {
		if status < 100 || status > 599 {
			return fmt.Errorf(
				"evaluate maximum successful attempts invariant: HTTP status must be between 100 and 599: %d",
				status,
			)
		}
		if _, exists := seen[status]; exists {
			return fmt.Errorf(
				"evaluate maximum successful attempts invariant: HTTP status is repeated: %d",
				status,
			)
		}
		seen[status] = struct{}{}
	}
	return nil
}

func successfulStatusCode(configured []int, observed int) bool {
	if configured == nil {
		return observed >= http.StatusOK && observed < http.StatusMultipleChoices
	}
	for _, status := range configured {
		if observed == status {
			return true
		}
	}
	return false
}

func cloneMaximumSuccessfulAttemptsInvariant(
	invariant MaximumSuccessfulAttemptsInvariant,
) MaximumSuccessfulAttemptsInvariant {
	invariant.SuccessfulStatusCodes = append([]int(nil), invariant.SuccessfulStatusCodes...)
	return invariant
}
