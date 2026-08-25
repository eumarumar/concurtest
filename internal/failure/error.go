// Package failure defines stable, structured errors shared across ConcurTest's
// execution and reporting boundaries.
package failure

import (
	"context"
	"errors"
)

// Code identifies a failure category without requiring consumers to parse a
// human-readable error message.
type Code string

const (
	CodeInvalidCommand            Code = "invalid_command"
	CodeScenarioFileFailed        Code = "scenario_file_failed"
	CodeScenarioInvalid           Code = "scenario_invalid"
	CodeInvalidExecution          Code = "invalid_execution"
	CodeRequestCreateFailed       Code = "request_create_failed"
	CodeRequestSendFailed         Code = "request_send_failed"
	CodeResponseFailed            Code = "response_failed"
	CodeResponseReadFailed        Code = "response_read_failed"
	CodeResponseCloseFailed       Code = "response_close_failed"
	CodeResponseTruncated         Code = "response_truncated"
	CodeMissingHTTPResponse       Code = "missing_http_response"
	CodeUnexpectedHTTPStatus      Code = "unexpected_http_status"
	CodeSetupFailed               Code = "setup_failed"
	CodeOperationBatchFailed      Code = "operation_batch_failed"
	CodeObservationFailed         Code = "observation_failed"
	CodeInvariantInvalid          Code = "invariant_invalid"
	CodeInvariantEvaluationFailed Code = "invariant_evaluation_failed"
	CodeTrialSequenceInterrupted  Code = "trial_sequence_interrupted"
	CodeReductionInvalid          Code = "reduction_invalid"
	CodeReductionBaselineFailed   Code = "reduction_baseline_failed"
	CodeReductionCandidateFailed  Code = "reduction_candidate_failed"
	CodeReportInvalid             Code = "report_invalid"
	CodeReportWriteFailed         Code = "report_write_failed"
	CodeCanceled                  Code = "canceled"
	CodeDeadlineExceeded          Code = "deadline_exceeded"
	CodeExternal                  Code = "external"
	CodeErrorTreeTruncated        Code = "error_tree_truncated"
	CodeInternal                  Code = "internal"
)

// Error is one structured failure node. Causes retain the original error tree
// so errors.Is and errors.As continue to work through package boundaries.
type Error struct {
	Code    Code
	Message string
	Causes  []error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	causes := nonNil(e.Causes)
	if len(causes) == 0 {
		return e.Message
	}
	if len(causes) == 1 {
		if e.Message == "" {
			return causes[0].Error()
		}
		return e.Message + ": " + causes[0].Error()
	}
	if e.Message == "" {
		return errors.Join(causes...).Error()
	}
	return e.Message + ": " + errors.Join(causes...).Error()
}

// Unwrap exposes all causes using Go's standard multi-error convention.
func (e *Error) Unwrap() []error {
	if e == nil {
		return nil
	}
	return e.Causes
}

// New creates a structured error without a cause.
func New(code Code, message string) error {
	return &Error{Code: code, Message: message}
}

// Wrap creates a structured error around one cause. A nil cause produces nil.
func Wrap(code Code, message string, cause error) error {
	if cause == nil {
		return nil
	}
	return &Error{Code: code, Message: message, Causes: []error{cause}}
}

// Join creates a structured error around all non-nil causes. No causes
// produces nil, matching errors.Join.
func Join(code Code, message string, causes ...error) error {
	causes = nonNil(causes)
	if len(causes) == 0 {
		return nil
	}
	return &Error{Code: code, Message: message, Causes: causes}
}

// CodeOf classifies an error node. Context errors are recognized even when
// they originate outside ConcurTest.
func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	if err == context.Canceled {
		return CodeCanceled
	}
	if err == context.DeadlineExceeded {
		return CodeDeadlineExceeded
	}
	if structured, ok := err.(*Error); ok && structured.Code != "" {
		return structured.Code
	}
	return CodeExternal
}

func nonNil(errorsList []error) []error {
	filtered := make([]error, 0, len(errorsList))
	for _, err := range errorsList {
		if err != nil {
			filtered = append(filtered, err)
		}
	}
	return filtered
}
