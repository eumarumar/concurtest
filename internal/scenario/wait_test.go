package scenario_test

import (
	"strings"
	"testing"
	"time"

	"github.com/eumarumar/concurtest/internal/engine"
	"github.com/eumarumar/concurtest/internal/scenario"
)

func TestDecodeReadinessAndCompletionConditions(t *testing.T) {
	t.Parallel()
	definition, err := scenario.Decode(strings.NewReader(validYAML("http://example.test") + `
readiness:
  request: {method: GET, path: /pending}
  assert: {name: No pending work, json_integer_path: [pending], equals: 0}
  timeout: 2s
  interval: 25ms
completion:
  request: {method: GET, path: /done}
  assert: {name: Complete, json_boolean_path: [done], equals: true}
  timeout: 3s
  interval: 10ms
  max_polls: 8
`))
	if err != nil {
		t.Fatal(err)
	}
	ready, complete := definition.Scenario.Readiness, definition.Scenario.Completion
	if ready == nil || ready.Timeout != 2*time.Second || ready.Interval != 25*time.Millisecond || ready.PollLimit() != engine.MaxWaitPolls || ready.Request.URL != "http://example.test/pending" {
		t.Fatalf("readiness not decoded: %+v", ready)
	}
	if complete == nil || complete.Timeout != 3*time.Second || complete.PollLimit() != 8 || !complete.Assertion.JSONBoolean.Equals {
		t.Fatalf("completion not decoded: %+v", complete)
	}
}

func TestDecodeRejectsInvalidWaitConditions(t *testing.T) {
	t.Parallel()
	base := `{request: {method: GET, path: /pending}, assert: {name: Idle, json_integer_path: [pending], equals: 0}, timeout: 2s, interval: 10ms}`
	invalid := []string{"{}", "[]", "true",
		strings.Replace(base, "timeout: 2s, ", "", 1), strings.Replace(base, ", interval: 10ms", "", 1),
		strings.Replace(base, "timeout: 2s", "timeout: 0s", 1), strings.Replace(base, "interval: 10ms", "interval: -1ms", 1),
		strings.Replace(base, "timeout: 2s", "timeout: 2", 1), strings.Replace(base, "interval: 10ms", "interval: fast", 1),
		strings.Replace(base, "request: {method: GET, path: /pending}, ", "", 1),
		strings.Replace(base, "assert: {name: Idle, json_integer_path: [pending], equals: 0}", "assert: null", 1),
		strings.Replace(base, "path: /pending", "path: https://other.test/pending", 1),
		strings.Replace(base, "equals: 0", "change: 0", 1),
		strings.TrimSuffix(base, "}") + ", max_polls: 0}", strings.TrimSuffix(base, "}") + ", max_polls: 101}",
		strings.TrimSuffix(base, "}") + ", max_polls: '2'}", strings.TrimSuffix(base, "}") + ", unknown: true}",
		strings.TrimSuffix(base, "}") + ", max_polls: 2, max_polls: 3}",
	}
	for _, stage := range []string{"readiness", "completion"} {
		for _, config := range invalid {
			if _, err := scenario.Decode(strings.NewReader(validYAML("http://example.test") + stage + ": " + config + "\n")); err == nil {
				t.Errorf("accepted invalid %s condition: %s", stage, config)
			}
		}
	}
}
