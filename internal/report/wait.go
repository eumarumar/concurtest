package report

import (
	"fmt"
	"io"

	"github.com/eumarumar/concurtest/internal/engine"
)

type waitDefinitionJSON struct {
	Request    requestJSON `json:"request"`
	Assertion  any         `json:"assertion"`
	TimeoutNS  int64       `json:"timeout_ns"`
	IntervalNS int64       `json:"interval_ns"`
	MaxPolls   int         `json:"max_polls"`
}

type waitPollJSON struct {
	Number    int                      `json:"number"`
	Execution *executionJSON           `json:"execution"`
	Assertion *assertionEvaluationJSON `json:"assertion"`
}

type waitExecutionJSON struct {
	Timing    *timingJSON    `json:"timing"`
	Polls     []waitPollJSON `json:"polls"`
	Satisfied bool           `json:"satisfied"`
	Error     *errorJSON     `json:"error"`
}

func waitDefinition(condition *engine.WaitCondition) *waitDefinitionJSON {
	if condition == nil {
		return nil
	}
	return &waitDefinitionJSON{Request: requestFromEngine(condition.Request), Assertion: assertionDefinition(&condition.Assertion), TimeoutNS: int64(condition.Timeout), IntervalNS: int64(condition.Interval), MaxPolls: condition.PollLimit()}
}

func waitExecution(result *engine.WaitResult) *waitExecutionJSON {
	if result == nil {
		return nil
	}
	polls := make([]waitPollJSON, len(result.Polls))
	for index, poll := range result.Polls {
		polls[index] = waitPollJSON{Number: index + 1, Execution: executionResultJSON(&poll.Execution), Assertion: assertionResultJSON(poll.Assertion)}
	}
	return &waitExecutionJSON{Timing: timing(result.StartedAt, result.CompletedAt), Polls: polls, Satisfied: result.Satisfied, Error: optionalError(result.Err)}
}

func writeWaitDefinition(writer io.Writer, label string, condition *engine.WaitCondition) {
	if condition != nil {
		fmt.Fprintf(writer, "  %-15s %s · timeout %s · interval %s · at most %d polls\n", label, condition.Assertion.Name(), condition.Timeout, condition.Interval, condition.PollLimit())
	}
}

func writeWaitEvidence(writer io.Writer, label string, result *engine.WaitResult, verbose bool, indent string) {
	if result == nil {
		return
	}
	status := "NOT VERIFIED"
	if result.Satisfied {
		status = "VERIFIED"
	}
	fmt.Fprintf(writer, "%s%s · %s · %d polls\n", indent, label, status, len(result.Polls))
	fmt.Fprintf(writer, "%s  Condition       %s\n", indent, result.Condition.Assertion.Name())
	if result.Err != nil {
		fmt.Fprintf(writer, "%s  Error           %s\n", indent, quoted(result.Err.Error()))
	}
	first := 0
	if !verbose && len(result.Polls) > 1 {
		first = len(result.Polls) - 1
		fmt.Fprintf(writer, "%s  %d earlier polls not shown.\n", indent, first)
	}
	for index := first; index < len(result.Polls); index++ {
		poll := result.Polls[index]
		fmt.Fprintf(writer, "%s  Poll #%d         %s %s · %s\n", indent, index+1, poll.Execution.Request.Method, requestTarget(poll.Execution.Request.URL), executionStatus(&poll.Execution))
		if verbose {
			writeExecutionDetails(writer, &poll.Execution, indent+"    ")
		} else {
			writeCompactExecutionDetails(writer, &poll.Execution, indent+"    ")
		}
		value := "Not verified"
		if poll.Assertion != nil {
			if poll.Assertion.ObservedInteger != nil {
				value = fmt.Sprint(*poll.Assertion.ObservedInteger)
			} else if poll.Assertion.ObservedBoolean != nil {
				value = fmt.Sprint(*poll.Assertion.ObservedBoolean)
			}
		}
		fmt.Fprintf(writer, "%s    Observed        %s\n", indent, value)
	}
}
