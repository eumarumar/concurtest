package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// Invariant contains exactly one concrete invariant definition.
type Invariant struct {
	JSONInteger               *JSONIntegerInvariant
	MaximumSuccessfulAttempts *MaximumSuccessfulAttemptsInvariant
}

// JSONIntegerInvariant constrains the JSON integer at one path. Bounds are
// inclusive; Equals and Change are each exclusive with other constraints.
type JSONIntegerInvariant struct {
	Name string
	// Path entries select literal object keys or zero-based array indexes.
	// Indexes use canonical decimal strings, keeping numeric object keys valid.
	Path []string
	// Pointers distinguish an omitted constraint from a constraint of zero.
	Minimum *int64
	Maximum *int64
	Equals  *int64
	// Change requires final minus baseline to equal this value.
	Change *int64
}

// JSONIntegerEvaluation records the value observed for one JSON integer invariant.
type JSONIntegerEvaluation struct {
	Invariant JSONIntegerInvariant
	Observed  int64
	// Baseline and Change are populated only for change invariants.
	Baseline *int64
	Change   *int64
	Violated bool
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
	JSONInteger               *JSONIntegerEvaluation
	MaximumSuccessfulAttempts *MaximumSuccessfulAttemptsEvaluation
	Violated                  bool
}

// EvaluateJSONInteger evaluates an invariant against one JSON document.
func EvaluateJSONInteger(
	invariant JSONIntegerInvariant,
	document []byte,
) (JSONIntegerEvaluation, error) {
	return evaluateJSONInteger(invariant, document, nil)
}

// EvaluateJSONIntegerChange compares the final integer with a previously read
// baseline. The invariant must declare Change; subtraction is checked for overflow.
func EvaluateJSONIntegerChange(invariant JSONIntegerInvariant, baseline int64, document []byte) (JSONIntegerEvaluation, error) {
	if invariant.Change == nil {
		return JSONIntegerEvaluation{}, errors.New("JSON integer change invariant requires change")
	}
	return evaluateJSONInteger(invariant, document, &baseline)
}

func evaluateJSONInteger(invariant JSONIntegerInvariant, document []byte, baseline *int64) (JSONIntegerEvaluation, error) {
	if err := validateJSONIntegerInvariant(invariant); err != nil {
		return JSONIntegerEvaluation{}, err
	}

	if invariant.Change != nil && baseline == nil {
		return JSONIntegerEvaluation{}, errors.New("JSON integer change requires a baseline observation")
	}
	observed, err := readJSONInteger(invariant.Path, document)
	if err != nil {
		return JSONIntegerEvaluation{}, err
	}
	result := JSONIntegerEvaluation{
		Invariant: cloneJSONIntegerInvariant(invariant),
		Observed:  observed,
		Violated: (invariant.Minimum != nil && observed < *invariant.Minimum) ||
			(invariant.Maximum != nil && observed > *invariant.Maximum) ||
			(invariant.Equals != nil && observed != *invariant.Equals),
	}
	if invariant.Change != nil {
		// Check before subtracting: wrapping int64 arithmetic could falsely pass.
		if (*baseline > 0 && observed < math.MinInt64+*baseline) ||
			(*baseline < 0 && observed > math.MaxInt64+*baseline) {
			return JSONIntegerEvaluation{}, errors.New("observed JSON integer change is not representable as int64")
		}
		result.Baseline = new(*baseline)
		result.Change = new(observed - *baseline)
		result.Violated = *result.Change != *invariant.Change
	}
	return result, nil
}

// readJSONInteger preserves exact values and is shared by baseline capture and
// final evaluation. A malformed baseline must stop the run before operations.
func readJSONInteger(segments []string, document []byte) (int64, error) {
	decoder := json.NewDecoder(bytes.NewReader(document))
	var rawValue json.RawMessage
	if err := decoder.Decode(&rawValue); err != nil {
		return 0, fmt.Errorf("decode observation as JSON: %w", err)
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return 0, errors.New("decode observation as JSON: multiple JSON values")
		}
		return 0, fmt.Errorf("decode trailing observation data: %w", err)
	}

	path := formatJSONPath(segments)
	for index, segment := range segments {
		var err error
		rawValue, err = jsonPathChild(rawValue, segment, formatJSONPath(segments[:index]))
		if err != nil {
			return 0, err
		}
	}

	var observed *int64
	if err := json.Unmarshal(rawValue, &observed); err != nil {
		return 0, fmt.Errorf(
			"observation path %s must contain a JSON integer representable as int64: %w",
			path,
			err,
		)
	}
	if observed == nil {
		return 0, fmt.Errorf("observation path %s must contain an integer, not null", path)
	}

	return *observed, nil
}

// jsonPathChild interprets each segment using the observed container type.
// RawMessage preserves exact integer values without a float64 conversion.
func jsonPathChild(raw json.RawMessage, segment, parentPath string) (json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	path := fmt.Sprintf("%s[%q]", parentPath, segment)
	switch {
	case len(raw) > 0 && raw[0] == '{':
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return nil, fmt.Errorf("decode observation path %s as a JSON object: %w", parentPath, err)
		}
		value, ok := object[segment]
		if !ok {
			return nil, fmt.Errorf("observation path %s is missing", path)
		}
		return value, nil
	case len(raw) > 0 && raw[0] == '[':
		index, err := strconv.Atoi(segment)
		if err != nil || index < 0 || strconv.Itoa(index) != segment {
			return nil, fmt.Errorf("observation path %s needs a non-negative base-10 array index, got %q", parentPath, segment)
		}
		var array []json.RawMessage
		if err := json.Unmarshal(raw, &array); err != nil {
			return nil, fmt.Errorf("decode observation path %s as a JSON array: %w", parentPath, err)
		}
		if index >= len(array) {
			return nil, fmt.Errorf("observation path %s is out of range: array has %d elements", path, len(array))
		}
		return array[index], nil
	case bytes.Equal(raw, []byte("null")):
		return nil, fmt.Errorf("observation path %s must contain a JSON object or array, not null", parentPath)
	default:
		return nil, fmt.Errorf("observation path %s must contain a JSON object or array", parentPath)
	}
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

func validateJSONIntegerInvariant(invariant JSONIntegerInvariant) error {
	if strings.TrimSpace(invariant.Name) == "" {
		return errors.New("evaluate JSON integer invariant: empty name")
	}
	if len(invariant.Path) == 0 {
		return errors.New("evaluate JSON integer invariant: empty path")
	}
	for index, segment := range invariant.Path {
		if strings.TrimSpace(segment) == "" {
			return fmt.Errorf("evaluate JSON integer invariant: empty path segment %d", index+1)
		}
	}
	if invariant.Minimum == nil && invariant.Maximum == nil && invariant.Equals == nil && invariant.Change == nil {
		return errors.New("JSON integer invariant requires minimum, maximum, equals, or change")
	}
	if invariant.Change != nil && (invariant.Minimum != nil || invariant.Maximum != nil || invariant.Equals != nil) {
		return errors.New("JSON integer invariant: change cannot be combined with minimum, maximum, or equals")
	}
	if invariant.Equals != nil && (invariant.Minimum != nil || invariant.Maximum != nil) {
		return errors.New("JSON integer invariant: equals cannot be combined with minimum or maximum")
	}
	if invariant.Minimum != nil && invariant.Maximum != nil && *invariant.Minimum > *invariant.Maximum {
		return errors.New("JSON integer invariant: minimum must not exceed maximum")
	}
	return nil
}

func validateInvariant(invariant Invariant) error {
	definitions := 0
	if invariant.JSONInteger != nil {
		definitions++
		if err := validateJSONIntegerInvariant(*invariant.JSONInteger); err != nil {
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

func cloneJSONIntegerInvariant(invariant JSONIntegerInvariant) JSONIntegerInvariant {
	invariant.Path = append([]string(nil), invariant.Path...)
	if invariant.Minimum != nil {
		invariant.Minimum = new(*invariant.Minimum)
	}
	if invariant.Maximum != nil {
		invariant.Maximum = new(*invariant.Maximum)
	}
	if invariant.Equals != nil {
		invariant.Equals = new(*invariant.Equals)
	}
	if invariant.Change != nil {
		invariant.Change = new(*invariant.Change)
	}
	return invariant
}

func formatJSONPath(path []string) string {
	var formatted strings.Builder
	formatted.WriteByte('$')
	for _, segment := range path {
		fmt.Fprintf(&formatted, "[%q]", segment)
	}
	return formatted.String()
}
