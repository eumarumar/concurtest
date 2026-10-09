package scenario_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/eumarumar/concurtest/internal/engine"
	"github.com/eumarumar/concurtest/internal/scenario"
)

func TestDecodeOrderedSetupRequests(t *testing.T) {
	t.Parallel()
	definition, err := scenario.Decode(strings.NewReader(strings.Replace(validYAML("http://example.test"), "operation:\n", `setup:
  - name: Reset inventory
    request:
      method: POST
      path: /testing/reset-inventory
      headers:
        Content-Type: application/json
      body: '{"stock":1000}'
  - name: Clear basket
    request:
      method: DELETE
      path: /testing/basket/2/items
  - name: Prepare basket
    request:
      method: POST
      path: /api/BasketItems
      body: '{"BasketId":2,"ProductId":6,"quantity":1}'

operation:
`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	steps := definition.Scenario.Setup
	if len(steps) != 3 {
		t.Fatalf("setup count = %d, want 3", len(steps))
	}
	for index, want := range []struct{ name, method, path string }{
		{"Reset inventory", "POST", "/testing/reset-inventory"},
		{"Clear basket", "DELETE", "/testing/basket/2/items"},
		{"Prepare basket", "POST", "/api/BasketItems"},
	} {
		if steps[index].Name != want.name || steps[index].Request.Method != want.method || steps[index].Request.URL != "http://example.test"+want.path {
			t.Errorf("step %d = %#v, want %+v", index+1, steps[index], want)
		}
	}
	if steps[0].Request.Header.Get("Content-Type") != "application/json" || string(steps[0].Request.Body) != `{"stock":1000}` {
		t.Fatalf("first request lost headers/body: %#v", steps[0].Request)
	}
}

func TestDecodeRejectsInvalidSetupRequests(t *testing.T) {
	t.Parallel()
	for _, setup := range []string{
		"[]", "42", "true", "somewhere",
		"[null]", "[[]]", "[{}]",
		"[{name: ' ', request: {method: POST, path: /reset}}]",
		"[{name: 42, request: {method: POST, path: /reset}}]",
		"[{name: Reset}]", "[{name: Reset, request: null}]",
		"[{name: Reset, request: []}]", "[{name: Reset, request: {}}]",
		"[{name: Reset, request: {method: POST, path: /reset}, unknown: true}]",
		"[{name: Reset, name: Again, request: {method: POST, path: /reset}}]",
		"[{name: Reset, request: {method: POST, path: /reset, unknown: true}}]",
		"[{name: Reset, request: {method: POST, path: /reset, path: /again}}]",
		"[{name: Reset, request: {method: POST, path: https://other.test/reset}}]",
		"[{name: Reset, request: {method: POST, path: /reset, body: 42}}]",
		"[{name: Reset, request: {method: POST, path: /reset, headers: {X-Mode: 42}}}]",
		"{method: POST, path: /reset, unknown: true}",
		"{method: POST, path: /reset, path: /again}",
	} {
		t.Run(setup, func(t *testing.T) {
			document := strings.Replace(validYAML("http://example.test"), "operation:\n", "setup: "+setup+"\noperation:\n", 1)
			if _, err := scenario.Decode(strings.NewReader(document)); err == nil {
				t.Fatalf("accepted invalid setup: %s", setup)
			}
		})
	}
}

func TestDecodeSetupLimitAndReduction(t *testing.T) {
	t.Parallel()
	for _, count := range []int{engine.MaxSetupSteps, engine.MaxSetupSteps + 1} {
		var setup strings.Builder
		setup.WriteString("setup:\n")
		for index := 0; index < count; index++ {
			fmt.Fprintf(&setup, "  - name: Step %d\n    request: {method: POST, path: /reset}\n", index+1)
		}
		document := strings.Replace(validYAML("http://example.test"), "operation:\n", setup.String()+"operation:\n", 1)
		document = strings.Replace(document, "trials: 1", "trials: 3\n  reduce: true", 1)
		definition, err := scenario.Decode(strings.NewReader(document))
		if count > engine.MaxSetupSteps {
			if err == nil {
				t.Fatal("accepted too many setup requests")
			}
		} else if err != nil || len(definition.Scenario.Setup) != count || !definition.Reduce {
			t.Fatalf("setup list with reduction: definition = %+v, error = %v", definition, err)
		}
	}
}
