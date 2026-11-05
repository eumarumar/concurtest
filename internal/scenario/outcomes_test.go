package scenario_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/eumarumar/concurtest/internal/scenario"
)

func TestDecodeOutcomePolicy(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, config       string
		minimum            int
		accepted, rejected []int
	}{
		{"defaults", "", 1, nil, nil},
		{"explicit", "outcomes: {accepted_status_codes: [201], rejected_status_codes: [400, 409], minimum_accepted_attempts: 2}\n", 2, []int{201}, []int{400, 409}},
		{"rejection only", "outcomes: {rejected_status_codes: [409], minimum_accepted_attempts: 0}\n", 0, nil, []int{409}},
		{"empty rejection list", "outcomes: {rejected_status_codes: []}\n", 1, nil, []int{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			definition, err := scenario.Decode(strings.NewReader(validYAML("http://example.test") + test.config))
			if err != nil {
				t.Fatal(err)
			}
			policy := definition.Scenario.Outcomes
			if policy.MinimumAccepted() != test.minimum || !slices.Equal(policy.AcceptedStatusCodes, test.accepted) || !slices.Equal(policy.RejectedStatusCodes, test.rejected) {
				t.Fatalf("unexpected policy: %+v", policy)
			}
		})
	}
}

func TestDecodeRejectsInvalidOutcomePolicy(t *testing.T) {
	t.Parallel()
	for _, config := range []string{
		"accepted_status_codes: []", "accepted_status_codes: [201, 201]", "rejected_status_codes: [409, 409]",
		"accepted_status_codes: [99]", "rejected_status_codes: [600]", "rejected_status_codes: [204]",
		"accepted_status_codes: [409], rejected_status_codes: [409]", "accepted_status_codes: ['201']",
		"accepted_status_codes: 201", "minimum_accepted_attempts: -1", "minimum_accepted_attempts: 3",
		"minimum_accepted_attempts: '1'", "minimum_accepted_attempts: true", "unexpected_field: true",
		"minimum_accepted_attempts: 1, minimum_accepted_attempts: 0",
	} {
		t.Run(config, func(t *testing.T) {
			_, err := scenario.Decode(strings.NewReader(validYAML("http://example.test") + "outcomes: {" + config + "}\n"))
			if err == nil {
				t.Fatalf("accepted invalid outcomes: %s", config)
			}
		})
	}
}

func TestDecodeOutcomeAcceptanceUsesHistoryFilterUnlessOverridden(t *testing.T) {
	t.Parallel()
	for _, override := range []bool{false, true} {
		document := historyInvariantYAML("http://example.test", "  successful_status_codes: [201]\n")
		want := []int{201}
		if override {
			document += "outcomes: {accepted_status_codes: [202]}\n"
			want = []int{202}
		} else {
			// 204 can be a declared rejection once the effective acceptance
			// filter is 201 rather than all 2xx.
			document += "outcomes: {rejected_status_codes: [204]}\n"
		}
		definition, err := scenario.Decode(strings.NewReader(document))
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(definition.Scenario.EffectiveOutcomes().AcceptedStatusCodes, want) {
			t.Fatalf("acceptance did not use the effective filter: %+v", definition.Scenario.Outcomes)
		}
	}
}
