package reduction

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/eumarumar/concurtest/internal/engine"
)

func TestReduceRequiresOutcomeCoverageEvenWithStateViolations(t *testing.T) {
	t.Parallel()
	for _, failBaseline := range []bool{true, false} {
		t.Run(map[bool]string{true: "baseline", false: "candidate"}[failBaseline], func(t *testing.T) {
			var trial atomic.Int32
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/setup":
					trial.Add(1)
					return response(http.StatusNoContent, ""), nil
				case "/operation":
					if failBaseline || trial.Load() > 3 {
						return response(http.StatusInternalServerError, "effects uncertain"), nil
					}
					return response(http.StatusCreated, "accepted"), nil
				case "/state":
					// The predicate is identical in every trial; this tests usable
					// outcomes, without introducing failure-identity matching.
					return response(http.StatusOK, `{"stock":-1}`), nil
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					return response(http.StatusNotFound, ""), nil
				}
			})}
			result, err := Reduce(context.Background(), client, scenarioWithSetup(3, 2), 3)
			if err != nil {
				t.Fatal(err)
			}
			if result.Baseline.Status != engine.TrialStatusViolated {
				t.Fatalf("lost baseline violation: %+v", result)
			}
			if failBaseline {
				if result.Status != StatusSkipped || len(result.Candidates) != 0 || trial.Load() != 3 {
					t.Fatalf("reduction ran without baseline coverage: %+v", result)
				}
				return
			}
			if result.Status != StatusUnchanged || len(result.Candidates) != 1 || trial.Load() != 6 {
				t.Fatalf("unexpected candidate search: %+v", result)
			}
			candidate := result.Candidates[0]
			if candidate.Accepted || candidate.OutcomeRequirementsMet || candidate.Summary.Violated != 3 || candidate.Trials != nil {
				t.Fatalf("candidate qualified despite unexpected outcomes: %+v", candidate)
			}
		})
	}
}

func TestReduceKeepsMinimumAcceptedRequirementForCandidates(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/state" {
			return response(http.StatusOK, `{"stock":-1}`), nil
		}
		return response(http.StatusOK, ""), nil
	})}
	scenario := scenarioWithSetup(3, 3)
	scenario.Outcomes.MinimumAcceptedAttempts = new(3)
	result, err := Reduce(context.Background(), client, scenario, 3)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusReduced || result.Selected != (Candidate{Attempts: 3, Concurrency: 2}) || len(result.Candidates) != 1 || !result.Candidates[0].OutcomeRequirementsMet {
		t.Fatalf("candidate violated the minimum requirement: %+v", result)
	}
}
