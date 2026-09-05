package report

import "strconv"

func reproductionExecutionArguments(input Input) []string {
	attempts := input.Scenario.Attempts
	concurrency := input.Scenario.Concurrency
	reduce := input.ReductionEnabled
	if input.Reduction != nil && input.Reduction.SelectedTrials != nil {
		attempts = input.Reduction.Selected.Attempts
		concurrency = input.Reduction.Selected.Concurrency
		reduce = false
	}
	arguments := []string{"--attempts", strconv.Itoa(attempts), "--concurrency", strconv.Itoa(concurrency)}
	if !reduce {
		arguments = append(arguments, "--no-reduce")
	}
	return arguments
}
