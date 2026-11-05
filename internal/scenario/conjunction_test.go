package scenario_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/eumarumar/concurtest/internal/engine"
	"github.com/eumarumar/concurtest/internal/scenario"
)

func conjunctionYAML(invariant string) string {
	prefix, _, _ := strings.Cut(validYAML("http://example.test"), "invariant:\n")
	return prefix + "invariant:\n" + invariant + "\n"
}

func TestDecodeConjunctionOfExistingChecks(t *testing.T) {
	t.Parallel()
	document := conjunctionYAML(`  all:
    - {name: Balance, json_integer_path: [balances, 0, cents], minimum: 0}
    - {name: Ledger, json_integer_path: [mismatch], equals: 0}
    - {name: Debit, json_integer_path: [balance], change: -7000}
    - {name: Replies, maximum_successful_attempts: 2, successful_status_codes: [201]}`)
	definition, err := scenario.Decode(strings.NewReader(document))
	if err != nil {
		t.Fatal(err)
	}
	group := definition.Scenario.Invariant
	if len(group.All) != 4 || !group.RequiresObservation() || !group.HasChange() || group.All[0].JSONInteger.Path[1] != "0" || *group.All[2].JSONInteger.Change != -7000 || definition.Scenario.Outcomes.AcceptedStatusCodes != nil {
		t.Fatalf("incorrect group: %+v", definition)
	}
	for _, check := range group.All {
		if check.Name() == "" {
			t.Fatal("member identity missing")
		}
	}
	var maximum strings.Builder
	maximum.WriteString("  all:\n")
	for index := 0; index < engine.MaxInvariantChecks; index++ {
		fmt.Fprintf(&maximum, "    - {name: Check %d, json_integer_path: [count], equals: 0}\n", index)
	}
	definition, err = scenario.Decode(strings.NewReader(conjunctionYAML(maximum.String())))
	if err != nil || len(definition.Scenario.Invariant.All) != engine.MaxInvariantChecks {
		t.Fatalf("maximum group rejected: %v", err)
	}
}

func TestDecodeRejectsInvalidConjunctions(t *testing.T) {
	t.Parallel()
	for _, invariant := range []string{
		"  all: null", "  all: []", "  all: {}", "  all: [1, 2]",
		"  all: [{name: A, json_integer_path: [a], equals: 0}]",
		"  all:\n    - {name: A, json_integer_path: [a], equals: 0}\n    - {name: A, json_integer_path: [b], equals: 0}",
		"  all:\n    - {name: ' A ', json_integer_path: [a], equals: 0}\n    - {name: A, json_integer_path: [b], equals: 0}",
		"  all:\n    - {json_integer_path: [a], equals: 0}\n    - {name: B, json_integer_path: [b], equals: 0}",
		"  all:\n    - {name: A, json_integer_path: [a], equals: null}\n    - {name: B, json_integer_path: [b], equals: 0}",
		"  all:\n    - {name: A, typo: 0}\n    - {name: B, json_integer_path: [b], equals: 0}",
		"  all:\n    - {name: A, name: B, json_integer_path: [a], equals: 0}\n    - {name: B, json_integer_path: [b], equals: 0}",
		"  all:\n    - {all: [{name: A}, {name: B}]}\n    - {name: C, json_integer_path: [c], equals: 0}",
		"  name: ''\n  all: [{name: A}, {name: B}]",
		"  equals: 0\n  all: [{name: A}, {name: B}]",
		"  all: [{name: A}, {name: B}]\n  all: [{name: C}, {name: D}]",
	} {
		if _, err := scenario.Decode(strings.NewReader(conjunctionYAML(invariant))); err == nil {
			t.Fatalf("accepted invalid conjunction:\n%s", invariant)
		}
	}
	var oversized strings.Builder
	oversized.WriteString("  all:\n")
	for index := 0; index <= engine.MaxInvariantChecks; index++ {
		fmt.Fprintf(&oversized, "    - {name: C%d, json_integer_path: [c], equals: 0}\n", index)
	}
	if _, err := scenario.Decode(strings.NewReader(conjunctionYAML(oversized.String()))); err == nil {
		t.Fatal("oversized conjunction accepted")
	}
	state := conjunctionYAML("  all:\n    - {name: A, json_integer_path: [a], equals: 0}\n    - {name: B, json_integer_path: [b], equals: 0}")
	state = strings.Replace(state, "observation:\n  method: GET\n  path: /state\n", "", 1)
	// The shared observer is required for any state member.
	if strings.Contains(state, "observation:") {
		t.Fatal("test did not remove the observer")
	}
	if _, err := scenario.Decode(strings.NewReader(state)); err == nil {
		t.Fatal("missing observer accepted")
	}
}
