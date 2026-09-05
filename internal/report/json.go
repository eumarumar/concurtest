package report

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/eumarumar/concurtest/internal/engine"
	"github.com/eumarumar/concurtest/internal/failure"
	"github.com/eumarumar/concurtest/internal/reduction"
)

// JSONSchemaVersion is the semantic version of the checked-in JSON report
// schema. Report objects are closed, so changing their emitted properties is a
// breaking schema change.
const JSONSchemaVersion = "1.0.0"

const maxSerializedErrorNodes = 32

// ErrorInput supplies the available context for a failure that happened before
// a run report could be constructed.
type ErrorInput struct {
	ScenarioPath string
	ScenarioName string
	Target       string
	Err          error
}

type timingJSON struct {
	StartedAt   string `json:"started_at"`
	CompletedAt string `json:"completed_at"`
	DurationNS  int64  `json:"duration_ns"`
}

type errorJSON struct {
	Code    failure.Code `json:"code"`
	Message string       `json:"message"`
	Causes  []errorJSON  `json:"causes"`
}

type requestJSON struct {
	Method string `json:"method"`
	Target string `json:"target"`
}

type operationJSON struct {
	Name    string      `json:"name"`
	Request requestJSON `json:"request"`
}

type executionConfigJSON struct {
	Attempts         int  `json:"attempts"`
	Concurrency      int  `json:"concurrency"`
	Trials           int  `json:"trials"`
	ReductionEnabled bool `json:"reduction_enabled"`
}

type scenarioJSON struct {
	Path             string              `json:"path"`
	Name             string              `json:"name"`
	Target           string              `json:"target"`
	RequestTimeoutNS int64               `json:"request_timeout_ns"`
	Execution        executionConfigJSON `json:"execution"`
	Setup            *requestJSON        `json:"setup"`
	Operation        operationJSON       `json:"operation"`
	Observation      *requestJSON        `json:"observation"`
	Invariant        any                 `json:"invariant"`
}

type jsonIntegerMinimumDefinitionJSON struct {
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	Path    []string `json:"path"`
	Minimum int64    `json:"minimum"`
}

type maximumSuccessfulAttemptsDefinitionJSON struct {
	Type                  string `json:"type"`
	Name                  string `json:"name"`
	Maximum               int    `json:"maximum"`
	SuccessfulStatusCodes []int  `json:"successful_status_codes"`
}

type summaryJSON struct {
	Requested           int         `json:"requested"`
	Completed           int         `json:"completed"`
	Passed              int         `json:"passed"`
	Violated            int         `json:"violated"`
	Inconclusive        int         `json:"inconclusive"`
	Errored             int         `json:"errored"`
	FirstViolationTrial *int        `json:"first_violation_trial"`
	Timing              *timingJSON `json:"timing"`
}

type responseExcerptJSON struct {
	Encoding      string `json:"encoding"`
	Data          string `json:"data"`
	RetainedBytes int    `json:"retained_bytes"`
	Truncated     bool   `json:"truncated"`
}

type responseJSON struct {
	StatusCode  int                  `json:"status_code"`
	BodyExcerpt *responseExcerptJSON `json:"body_excerpt"`
}

type executionJSON struct {
	Request  requestJSON   `json:"request"`
	Response *responseJSON `json:"response"`
	Timing   *timingJSON   `json:"timing"`
	Error    *errorJSON    `json:"error"`
}

type attemptJSON struct {
	ID            int            `json:"id"`
	OperationName string         `json:"operation_name"`
	StartOffsetNS *int64         `json:"start_offset_ns"`
	Execution     *executionJSON `json:"execution"`
}

type historyJSON struct {
	Timing   *timingJSON   `json:"timing"`
	Attempts []attemptJSON `json:"attempts"`
}

type jsonIntegerMinimumEvaluationJSON struct {
	Type     string `json:"type"`
	Observed int64  `json:"observed"`
	Violated bool   `json:"violated"`
}

type maximumSuccessfulAttemptsEvaluationJSON struct {
	Type                 string `json:"type"`
	SuccessfulAttemptIDs []int  `json:"successful_attempt_ids"`
	OverLimitAttemptIDs  []int  `json:"over_limit_attempt_ids"`
	Violated             bool   `json:"violated"`
}

type runEvidenceJSON struct {
	Outcome             *string        `json:"outcome"`
	Timing              *timingJSON    `json:"timing"`
	Setup               *executionJSON `json:"setup"`
	History             historyJSON    `json:"history"`
	Observation         *executionJSON `json:"observation"`
	InvariantEvaluation any            `json:"invariant_evaluation"`
}

type trialJSON struct {
	Number   int                `json:"number"`
	Status   engine.TrialStatus `json:"status"`
	Error    *errorJSON         `json:"error"`
	Evidence runEvidenceJSON    `json:"evidence"`
}

type baselineReductionJSON struct {
	Attempts    int         `json:"attempts"`
	Concurrency int         `json:"concurrency"`
	Summary     summaryJSON `json:"summary"`
}

type candidateReductionJSON struct {
	Number      int                  `json:"number"`
	Attempts    int                  `json:"attempts"`
	Concurrency int                  `json:"concurrency"`
	Accepted    bool                 `json:"accepted"`
	Summary     candidateSummaryJSON `json:"summary"`
	Error       *errorJSON           `json:"error"`
	Trials      []trialJSON          `json:"trials"`
}

type candidateSummaryJSON struct {
	Requested    int         `json:"requested"`
	Completed    int         `json:"completed"`
	Passed       int         `json:"passed"`
	Violated     int         `json:"violated"`
	Inconclusive int         `json:"inconclusive"`
	Errored      int         `json:"errored"`
	Timing       *timingJSON `json:"timing"`
}

type selectionJSON struct {
	Source          string `json:"source"`
	CandidateNumber *int   `json:"candidate_number"`
	Attempts        int    `json:"attempts"`
	Concurrency     int    `json:"concurrency"`
}

type reductionJSON struct {
	Status        string                   `json:"status"`
	Timing        *timingJSON              `json:"timing"`
	MaxCandidates int                      `json:"max_candidates"`
	Baseline      baselineReductionJSON    `json:"baseline"`
	Candidates    []candidateReductionJSON `json:"candidates"`
	Selection     *selectionJSON           `json:"selection"`
}

type reproductionJSON struct {
	Arguments []string `json:"arguments"`
}

type runReportJSON struct {
	SchemaVersion string           `json:"schema_version"`
	ReportType    string           `json:"report_type"`
	Status        string           `json:"status"`
	Scenario      scenarioJSON     `json:"scenario"`
	Summary       summaryJSON      `json:"summary"`
	Trials        []trialJSON      `json:"trials"`
	Reduction     *reductionJSON   `json:"reduction"`
	Error         *errorJSON       `json:"error"`
	Reproduction  reproductionJSON `json:"reproduction"`
}

type errorContextJSON struct {
	ScenarioPath *string `json:"scenario_path"`
	ScenarioName *string `json:"scenario_name"`
	Target       *string `json:"target"`
}

type errorReportJSON struct {
	SchemaVersion string           `json:"schema_version"`
	ReportType    string           `json:"report_type"`
	Status        string           `json:"status"`
	Context       errorContextJSON `json:"context"`
	Error         errorJSON        `json:"error"`
}

// WriteJSON writes one deterministic, indented run report followed by a
// newline. All recorded trials are included, including passing trials.
func WriteJSON(writer io.Writer, input Input) error {
	if writer == nil {
		return failure.New(failure.CodeReportInvalid, "write JSON report: nil writer")
	}
	if err := validateTextInput(input); err != nil {
		return failure.Wrap(failure.CodeReportInvalid, "write JSON report", err)
	}
	if err := validateJSONInput(input); err != nil {
		return failure.Wrap(failure.CodeReportInvalid, "write JSON report", err)
	}

	report, err := newRunReportJSON(input)
	if err != nil {
		return failure.Wrap(failure.CodeReportInvalid, "write JSON report", err)
	}
	return writeJSONValue(writer, report)
}

func validateJSONInput(input Input) error {
	if strings.TrimSpace(input.ScenarioPath) == "" {
		return errors.New("scenario path is empty")
	}
	configuredTrials := input.ConfiguredTrials
	if configuredTrials == 0 {
		configuredTrials = input.Result.Requested
	}
	if configuredTrials < 1 || configuredTrials > engine.MaxTrials {
		return fmt.Errorf("configured trial count must be between 1 and %d: %d", engine.MaxTrials, configuredTrials)
	}
	if input.Scenario.Attempts < 1 || input.Scenario.Concurrency < 1 || input.Scenario.Concurrency > input.Scenario.Attempts {
		return errors.New("scenario execution settings are invalid")
	}
	if strings.TrimSpace(input.Scenario.Operation.Name) == "" {
		return errors.New("scenario operation name is empty")
	}
	if err := validateReportedRequest(input.Scenario.Operation.Request); err != nil {
		return fmt.Errorf("scenario operation request: %w", err)
	}
	if input.Scenario.Setup != nil {
		if err := validateReportedRequest(*input.Scenario.Setup); err != nil {
			return fmt.Errorf("scenario setup request: %w", err)
		}
	}
	if input.Scenario.Observation != nil {
		if err := validateReportedRequest(*input.Scenario.Observation); err != nil {
			return fmt.Errorf("scenario observation request: %w", err)
		}
	}
	if invariantDefinition(input.Scenario.Invariant) == nil {
		return errors.New("scenario invariant is missing")
	}
	return nil
}

func validateReportedRequest(request engine.HTTPRequest) error {
	if strings.TrimSpace(request.Method) == "" {
		return errors.New("method is empty")
	}
	if strings.TrimSpace(requestTarget(request.URL)) == "" {
		return errors.New("target is empty")
	}
	return nil
}

// WriteJSONError writes one structured report for a failure that occurred
// before trial execution could produce a run report.
func WriteJSONError(writer io.Writer, input ErrorInput) error {
	if writer == nil {
		return failure.New(failure.CodeReportInvalid, "write JSON error report: nil writer")
	}
	if input.Err == nil {
		return failure.New(failure.CodeReportInvalid, "write JSON error report: nil error")
	}
	report := errorReportJSON{
		SchemaVersion: JSONSchemaVersion,
		ReportType:    "error",
		Status:        "errored",
		Context: errorContextJSON{
			ScenarioPath: optionalString(input.ScenarioPath),
			ScenarioName: optionalString(input.ScenarioName),
			Target:       optionalString(input.Target),
		},
		Error: serializeError(input.Err),
	}
	return writeJSONValue(writer, report)
}

func writeJSONValue(writer io.Writer, value any) error {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return failure.Wrap(failure.CodeReportWriteFailed, "encode JSON report", err)
	}
	written, err := writer.Write(buffer.Bytes())
	if err == nil && written != buffer.Len() {
		err = io.ErrShortWrite
	}
	if err != nil {
		return failure.Wrap(failure.CodeReportWriteFailed, "write JSON report", err)
	}
	return nil
}

func newRunReportJSON(input Input) (runReportJSON, error) {
	status, err := runStatus(input)
	if err != nil {
		return runReportJSON{}, err
	}
	configuredTrials := input.ConfiguredTrials
	if configuredTrials == 0 {
		configuredTrials = input.Result.Requested
	}
	reductionEnabled := input.ReductionEnabled || input.Reduction != nil
	report := runReportJSON{
		SchemaVersion: JSONSchemaVersion,
		ReportType:    "run",
		Status:        status,
		Scenario: scenarioJSON{
			Path:             input.ScenarioPath,
			Name:             input.ScenarioName,
			Target:           input.Target,
			RequestTimeoutNS: nonNegativeDuration(input.RequestTimeout),
			Execution: executionConfigJSON{
				Attempts:         input.Scenario.Attempts,
				Concurrency:      input.Scenario.Concurrency,
				Trials:           configuredTrials,
				ReductionEnabled: reductionEnabled,
			},
			Setup: configuredRequest(input.Scenario.Setup),
			Operation: operationJSON{
				Name:    input.Scenario.Operation.Name,
				Request: requestFromEngine(input.Scenario.Operation.Request),
			},
			Observation: configuredRequest(input.Scenario.Observation),
			Invariant:   invariantDefinition(input.Scenario.Invariant),
		},
		Summary:      resultSummary(input.Result),
		Trials:       trialsJSON(input.Result.Trials),
		Reduction:    reductionResultJSON(input),
		Error:        optionalError(input.RunError),
		Reproduction: reproductionJSON{Arguments: reproductionArguments(input)},
	}
	return report, nil
}

func runStatus(input Input) (string, error) {
	if input.RunError != nil {
		if errors.Is(input.RunError, context.Canceled) || errors.Is(input.RunError, context.DeadlineExceeded) {
			return "interrupted", nil
		}
		return "errored", nil
	}
	switch input.Result.Status {
	case engine.TrialStatusPassed, engine.TrialStatusViolated, engine.TrialStatusInconclusive, engine.TrialStatusErrored:
		return string(input.Result.Status), nil
	default:
		return "", fmt.Errorf("unknown trials status %q", input.Result.Status)
	}
}

func invariantDefinition(invariant engine.Invariant) any {
	if definition := invariant.JSONIntegerMinimum; definition != nil {
		return jsonIntegerMinimumDefinitionJSON{
			Type: "json_integer_minimum", Name: definition.Name,
			Path: append([]string{}, definition.Path...), Minimum: definition.Minimum,
		}
	}
	if definition := invariant.MaximumSuccessfulAttempts; definition != nil {
		return maximumSuccessfulAttemptsDefinitionJSON{
			Type: "maximum_successful_attempts", Name: definition.Name,
			Maximum:               definition.Maximum,
			SuccessfulStatusCodes: append([]int(nil), definition.SuccessfulStatusCodes...),
		}
	}
	return nil
}

func trialsJSON(trials []engine.TrialResult) []trialJSON {
	result := make([]trialJSON, len(trials))
	for index, trial := range trials {
		result[index] = trialJSON{
			Number:   trial.Number,
			Status:   trial.Status,
			Error:    optionalError(trial.Err),
			Evidence: runResultJSON(trial.Run),
		}
	}
	return result
}

func runResultJSON(result engine.RunResult) runEvidenceJSON {
	attempts := make([]attemptJSON, len(result.History.Attempts))
	for index, attempt := range result.History.Attempts {
		var offset *int64
		if attempt.Execution != nil && !result.History.StartedAt.IsZero() {
			value := nonNegativeDuration(attempt.Execution.StartedAt.Sub(result.History.StartedAt))
			offset = &value
		}
		attempts[index] = attemptJSON{
			ID: attempt.ID, OperationName: attempt.OperationName,
			StartOffsetNS: offset, Execution: executionResultJSON(attempt.Execution),
		}
	}
	var outcome *string
	if result.Outcome != "" {
		value := string(result.Outcome)
		outcome = &value
	}
	return runEvidenceJSON{
		Outcome:             outcome,
		Timing:              timing(result.StartedAt, result.CompletedAt),
		Setup:               executionResultJSON(result.Setup),
		History:             historyJSON{Timing: timing(result.History.StartedAt, result.History.CompletedAt), Attempts: attempts},
		Observation:         executionResultJSON(result.Observation),
		InvariantEvaluation: invariantEvaluation(result.Evaluation),
	}
}

func executionResultJSON(execution *engine.HTTPExecution) *executionJSON {
	if execution == nil {
		return nil
	}
	var response *responseJSON
	if execution.Response != nil {
		response = &responseJSON{StatusCode: execution.Response.StatusCode}
		if len(execution.Response.Body) > 0 || execution.Response.BodyTruncated {
			excerpt := responseExcerptJSONValue(execution.Response)
			response.BodyExcerpt = &excerpt
		}
	}
	return &executionJSON{
		Request:  requestFromEngine(execution.Request),
		Response: response,
		Timing:   timing(execution.StartedAt, execution.CompletedAt),
		Error:    optionalError(execution.Err),
	}
}

func responseExcerptJSONValue(response *engine.HTTPResponse) responseExcerptJSON {
	body := response.Body
	truncated := response.BodyTruncated
	if len(body) > maxResponseExcerptBytes {
		body = body[:maxResponseExcerptBytes]
		truncated = true
	}
	value := responseExcerptJSON{RetainedBytes: len(body), Truncated: truncated}
	if utf8.Valid(body) {
		value.Encoding = "utf-8"
		value.Data = string(body)
	} else {
		value.Encoding = "base64"
		value.Data = base64.StdEncoding.EncodeToString(body)
	}
	return value
}

func invariantEvaluation(evaluation *engine.InvariantEvaluation) any {
	if evaluation == nil {
		return nil
	}
	if concrete := evaluation.JSONIntegerMinimum; concrete != nil {
		return jsonIntegerMinimumEvaluationJSON{
			Type: "json_integer_minimum", Observed: concrete.Observed, Violated: concrete.Violated,
		}
	}
	if concrete := evaluation.MaximumSuccessfulAttempts; concrete != nil {
		return maximumSuccessfulAttemptsEvaluationJSON{
			Type:                 "maximum_successful_attempts",
			SuccessfulAttemptIDs: append([]int{}, concrete.SuccessfulAttemptIDs...),
			OverLimitAttemptIDs:  append([]int{}, concrete.OverLimitAttemptIDs...),
			Violated:             concrete.Violated,
		}
	}
	return nil
}

func reductionResultJSON(input Input) *reductionJSON {
	if input.Reduction == nil {
		return nil
	}
	result := input.Reduction
	status := string(result.Status)
	if input.RunError != nil {
		if errors.Is(input.RunError, context.Canceled) || errors.Is(input.RunError, context.DeadlineExceeded) {
			status = "interrupted"
		} else {
			status = "errored"
		}
	}
	candidates := make([]candidateReductionJSON, len(result.Candidates))
	for index, candidate := range result.Candidates {
		var retained []trialJSON
		if candidate.Trials != nil {
			retained = trialsJSON(candidate.Trials.Trials)
		}
		candidates[index] = candidateReductionJSON{
			Number:      index + 1,
			Attempts:    candidate.Candidate.Attempts,
			Concurrency: candidate.Candidate.Concurrency,
			Accepted:    candidate.Accepted,
			Summary:     reductionSummaryJSON(candidate.Summary),
			Error:       optionalError(candidate.Err),
			Trials:      retained,
		}
	}
	return &reductionJSON{
		Status:        status,
		Timing:        timing(result.StartedAt, result.CompletedAt),
		MaxCandidates: reduction.MaxCandidates,
		Baseline: baselineReductionJSON{
			Attempts:    input.Scenario.Attempts,
			Concurrency: input.Scenario.Concurrency,
			Summary:     resultSummary(result.Baseline),
		},
		Candidates: candidates,
		Selection:  reductionSelection(*result),
	}
}

func reductionSelection(result reduction.Result) *selectionJSON {
	if result.SelectedTrials == nil {
		return nil
	}
	selection := &selectionJSON{
		Source: "baseline", Attempts: result.Selected.Attempts, Concurrency: result.Selected.Concurrency,
	}
	for index, candidate := range result.Candidates {
		if candidate.Accepted {
			number := index + 1
			selection.Source = "candidate"
			selection.CandidateNumber = &number
			break
		}
	}
	return selection
}

func resultSummary(result engine.TrialsResult) summaryJSON {
	counts := trialCounts(result.Trials)
	var first *int
	if counts.firstViolation != 0 {
		value := counts.firstViolation
		first = &value
	}
	return summaryJSON{
		Requested: result.Requested, Completed: len(result.Trials),
		Passed: counts.passed, Violated: counts.violated,
		Inconclusive: counts.inconclusive, Errored: counts.errored,
		FirstViolationTrial: first,
		Timing:              timing(result.StartedAt, result.CompletedAt),
	}
}

func reductionSummaryJSON(summary reduction.TrialSummary) candidateSummaryJSON {
	return candidateSummaryJSON{
		Requested: summary.Requested, Completed: summary.Completed,
		Passed: summary.Passed, Violated: summary.Violated,
		Inconclusive: summary.Inconclusive, Errored: summary.Errored,
		Timing: timing(summary.StartedAt, summary.CompletedAt),
	}
}

func timing(startedAt, completedAt time.Time) *timingJSON {
	if startedAt.IsZero() && completedAt.IsZero() {
		return nil
	}
	return &timingJSON{
		StartedAt:   startedAt.UTC().Format(time.RFC3339Nano),
		CompletedAt: completedAt.UTC().Format(time.RFC3339Nano),
		DurationNS:  nonNegativeDuration(completedAt.Sub(startedAt)),
	}
}

func nonNegativeDuration(duration time.Duration) int64 {
	if duration < 0 {
		return 0
	}
	return duration.Nanoseconds()
}

func configuredRequest(request *engine.HTTPRequest) *requestJSON {
	if request == nil {
		return nil
	}
	value := requestFromEngine(*request)
	return &value
}

func requestFromEngine(request engine.HTTPRequest) requestJSON {
	return requestJSON{Method: request.Method, Target: requestTarget(request.URL)}
}

func reproductionArguments(input Input) []string {
	arguments := []string{"concurtest", "run", "--format", "json"}
	arguments = append(arguments, reproductionExecutionArguments(input)...)
	return append(arguments, input.ScenarioPath)
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func optionalError(err error) *errorJSON {
	if err == nil {
		return nil
	}
	value := serializeError(err)
	return &value
}

func serializeError(err error) errorJSON {
	remaining := maxSerializedErrorNodes
	return serializeErrorNode(err, &remaining)
}

func serializeErrorNode(err error, remaining *int) errorJSON {
	if *remaining <= 0 {
		return errorJSON{Code: failure.CodeErrorTreeTruncated, Message: "additional error causes were omitted", Causes: []errorJSON{}}
	}
	*remaining = *remaining - 1
	if err == nil {
		return errorJSON{Code: failure.CodeInternal, Message: "missing error", Causes: []errorJSON{}}
	}

	var code failure.Code
	var message string
	var causes []error
	if structured, ok := err.(*failure.Error); ok {
		code = structured.Code
		message = structured.Message
		causes = structured.Causes
	} else {
		code = failure.CodeOf(err)
		message = err.Error()
		if multi, ok := err.(interface{ Unwrap() []error }); ok {
			causes = multi.Unwrap()
		} else if single, ok := err.(interface{ Unwrap() error }); ok {
			if cause := single.Unwrap(); cause != nil {
				causes = []error{cause}
				message = strings.TrimSuffix(message, ": "+cause.Error())
			}
		}
	}
	if code == "" {
		code = failure.CodeInternal
	}
	serialized := make([]errorJSON, 0, len(causes))
	for _, cause := range causes {
		if cause == nil {
			continue
		}
		if *remaining <= 1 {
			serialized = append(serialized, errorJSON{
				Code:    failure.CodeErrorTreeTruncated,
				Message: "additional error causes were omitted",
				Causes:  []errorJSON{},
			})
			*remaining = *remaining - 1
			break
		}
		serialized = append(serialized, serializeErrorNode(cause, remaining))
	}
	if serialized == nil {
		serialized = []errorJSON{}
	}
	return errorJSON{Code: code, Message: message, Causes: serialized}
}
