package scenario_test

import (
	"strings"
	"testing"

	"github.com/eumarumar/concurtest/internal/scenario"
)

func TestDecodeSetupAssertionsAndIndependentPrecondition(t *testing.T) {
	t.Parallel()
	document := validYAML("http://example.test") + `
setup:
  - name: Reset
    request: {method: POST, path: /reset}
    assert: {name: Reset declined, json_boolean_path: [data, 0, ok], equals: false}
precondition:
  request: {method: GET, path: /initial, headers: {X-Check: initial}}
  assert: {name: Initial counter, json_integer_path: [data, 0, count], equals: 0}
`
	definition, err := scenario.Decode(strings.NewReader(document))
	if err != nil {
		t.Fatal(err)
	}
	setup := definition.Scenario.Setup[0].Assertion.JSONBoolean
	precondition := definition.Scenario.Precondition
	if setup.Equals || strings.Join(setup.Path, "/") != "data/0/ok" || precondition.Request.URL != "http://example.test/initial" || precondition.Request.Header.Get("X-Check") != "initial" || *precondition.Assertion.JSONInteger.Equals != 0 {
		t.Fatalf("preparation definitions lost: %+v %+v", setup, precondition)
	}
}

func TestDecodeRejectsInvalidPreparationAssertions(t *testing.T) {
	t.Parallel()
	for _, assertion := range []string{
		"{}", "[]", "true", "{name: Check}", "{json_boolean_path: [ok], equals: true}",
		"{name: Check, json_boolean_path: [], equals: true}", "{name: Check, json_boolean_path: [ok], equals: 'true'}",
		"{name: Check, json_boolean_path: [ok], equals: 1}", "{name: Check, json_boolean_path: [ok], equals: null}",
		"{name: Check, json_boolean_path: [ok]}", "{name: Check, json_boolean_path: [ok], equals: true, minimum: 0}",
		"{name: Check, json_boolean_path: [ok], json_integer_path: [count], equals: true}",
		"{name: Check, json_integer_path: [count], equals: false}", "{name: Check, json_integer_path: [count], equals: null}",
		"{name: Check, json_integer_path: [count], equals: 0.0}", "{name: Check, json_integer_path: [count], equals: '0'}",
		"{name: Check, json_integer_path: [count], equals: 9223372036854775808}",
		"{name: Check, json_integer_path: [count], change: 0}", "{name: Check, json_integer_path: [count], minimum: 2, maximum: 1}",
		"{name: Check, json_integer_path: [count], equals: 0, minimum: 0}",
		"{name: Check, json_boolean_path: [ok], equals: true, unknown: true}",
		"{name: Check, json_boolean_path: [ok], equals: true, equals: false}",
	} {
		for _, location := range []string{"setup", "precondition"} {
			t.Run(location+"/"+assertion, func(t *testing.T) {
				declaration := "precondition: {request: {method: GET, path: /state}, assert: " + assertion + "}\n"
				if location == "setup" {
					declaration = "setup: [{name: Reset, request: {method: POST, path: /reset}, assert: " + assertion + "}]\n"
				}
				if _, err := scenario.Decode(strings.NewReader(validYAML("http://example.test") + declaration)); err == nil {
					t.Fatalf("accepted invalid %s assertion: %s", location, assertion)
				}
			})
		}
	}
}

func TestDecodeRejectsIncompleteOrUnknownPreconditionFields(t *testing.T) {
	t.Parallel()
	for _, value := range []string{
		"{}", "[]", "true", "{request: {method: GET, path: /state}}",
		"{assert: {name: Check, json_integer_path: [count], equals: 0}}",
		"{request: null, assert: {name: Check, json_integer_path: [count], equals: 0}}",
		"{request: {method: GET, path: /state}, assert: null}",
		"{request: {method: GET, path: /state}, assert: {name: Check, json_integer_path: [count], equals: 0}, unknown: true}",
		"{request: {method: GET, path: https://other.test/state}, assert: {name: Check, json_integer_path: [count], equals: 0}}",
	} {
		if _, err := scenario.Decode(strings.NewReader(validYAML("http://example.test") + "precondition: " + value + "\n")); err == nil {
			t.Errorf("accepted invalid precondition: %s", value)
		}
	}
}
