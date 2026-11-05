package reduction

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
)

func TestReductionSharesWorkBudgetWithBaselineAndRetainsActiveCandidate(t *testing.T) {
	t.Parallel()
	var calls, setups atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.Path == "/setup" {
			setups.Add(1)
		}
		if r.URL.Path == "/state" {
			return response(200, `{"stock":-1}`), nil
		}
		return response(200, ""), nil
	})}
	scenario := scenarioWithSetup(4, 4)
	scenario.Limits.MaxExchanges = 20
	result, err := Reduce(context.Background(), client, scenario, 3)
	if err == nil || errors.Is(err, context.Canceled) || result.Resources.Exhausted != "max_exchanges" || result.Resources.Exchanges != 20 || result.Baseline.Resources.Exchanges != 18 || calls.Load() > 20 || setups.Load() != 4 {
		t.Fatalf("candidate reset baseline budget: resources=%+v err=%v calls=%d setups=%d", result.Resources, err, calls.Load(), setups.Load())
	}
	if len(result.Candidates) != 1 || result.SelectedTrials != nil || result.Candidates[0].Trials == nil || result.Candidates[0].ChecksComplete || result.Candidates[0].Accepted {
		t.Fatalf("partial candidate selected or evidence lost: %+v", result.Candidates)
	}
	if result.Baseline.Status != "violated" {
		t.Fatal("earlier baseline violation hidden")
	}
}
