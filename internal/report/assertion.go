package report

import (
	"fmt"
	"io"

	"github.com/eumarumar/concurtest/internal/engine"
)

type setupDefinitionJSON struct {
	Name      string      `json:"name"`
	Request   requestJSON `json:"request"`
	Assertion any         `json:"assertion"`
}

type preconditionDefinitionJSON struct {
	Request   requestJSON `json:"request"`
	Assertion any         `json:"assertion"`
}

type preconditionExecutionJSON struct {
	Execution *executionJSON           `json:"execution"`
	Assertion *assertionEvaluationJSON `json:"assertion"`
}

type booleanAssertionJSON struct {
	Type   string   `json:"type"`
	Name   string   `json:"name"`
	Path   []string `json:"path"`
	Equals bool     `json:"equals"`
}

type assertionEvaluationJSON struct {
	Type      string `json:"type"`
	Observed  any    `json:"observed"`
	Satisfied bool   `json:"satisfied"`
}

func assertionDefinition(assertion *engine.ResponseAssertion) any {
	if assertion == nil {
		return nil
	}
	if assertion.JSONInteger != nil {
		return invariantDefinition(engine.Invariant{JSONInteger: assertion.JSONInteger})
	}
	if definition := assertion.JSONBoolean; definition != nil {
		return booleanAssertionJSON{Type: "json_boolean", Name: definition.Name, Path: definition.Path, Equals: definition.Equals}
	}
	return nil
}

func assertionResultJSON(evaluation *engine.AssertionEvaluation) *assertionEvaluationJSON {
	if evaluation == nil {
		return nil
	}
	result := &assertionEvaluationJSON{Satisfied: evaluation.Satisfied}
	if evaluation.Assertion.JSONInteger != nil {
		result.Type = "json_integer"
		if evaluation.ObservedInteger != nil {
			result.Observed = *evaluation.ObservedInteger
		}
	} else if evaluation.Assertion.JSONBoolean != nil {
		result.Type = "json_boolean"
		if evaluation.ObservedBoolean != nil {
			result.Observed = *evaluation.ObservedBoolean
		}
	}
	return result
}

func preconditionDefinition(definition *engine.Precondition) *preconditionDefinitionJSON {
	if definition == nil {
		return nil
	}
	return &preconditionDefinitionJSON{Request: requestFromEngine(definition.Request), Assertion: assertionDefinition(&definition.Assertion)}
}

func preconditionResult(execution *engine.PreconditionExecution) *preconditionExecutionJSON {
	if execution == nil {
		return nil
	}
	return &preconditionExecutionJSON{Execution: executionResultJSON(&execution.Execution), Assertion: assertionResultJSON(execution.Assertion)}
}

func writeAssertionEvidence(writer io.Writer, configured *engine.ResponseAssertion, evaluation *engine.AssertionEvaluation, indent string) {
	if configured == nil && evaluation != nil {
		configured = &evaluation.Assertion
	}
	if configured == nil {
		return
	}
	verified := false
	if definition := configured.JSONInteger; definition != nil {
		var observed *engine.InvariantEvaluation
		if evaluation != nil && evaluation.ObservedInteger != nil {
			observed = &engine.InvariantEvaluation{JSONInteger: &engine.JSONIntegerEvaluation{Observed: *evaluation.ObservedInteger}}
			verified = true
		}
		writeInvariantLines(writer, engine.Invariant{JSONInteger: definition}, observed, indent)
	} else if definition := configured.JSONBoolean; definition != nil {
		path := formatJSONPath(definition.Path)
		fmt.Fprintf(writer, "%s%s\n%sExpected        %s == %t\n", indent, definition.Name, indent, path, definition.Equals)
		if evaluation != nil && evaluation.ObservedBoolean != nil {
			fmt.Fprintf(writer, "%sObserved        %s = %t\n", indent, path, *evaluation.ObservedBoolean)
			verified = true
		} else {
			fmt.Fprintf(writer, "%sObserved        Not evaluated\n", indent)
		}
	}
	status := "NOT VERIFIED"
	if verified {
		status = "FAILED"
		if evaluation.Satisfied {
			status = "PASSED"
		}
	}
	fmt.Fprintf(writer, "%sCheck result    %s\n", indent, status)
}
