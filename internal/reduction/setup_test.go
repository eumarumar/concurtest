package reduction

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/eumarumar/concurtest/internal/engine"
)

func TestReductionRunsAllSetupsInOrderForEveryTrial(t *testing.T) {
	t.Parallel()
	for _, failCandidate := range []bool{false, true} {
		name := "successful setup"
		if failCandidate {
			name = "candidate setup fails"
		}
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			trial, stage, operations := 0, 0, 0
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				mu.Lock()
				defer mu.Unlock()
				switch r.URL.Path {
				case "/setup":
					if trial > 0 && stage != 4 && !(failCandidate && trial >= 4 && trial <= 6 && stage == 2) {
						t.Errorf("trial %d began before previous trial finished, stage=%d", trial+1, stage)
					}
					trial++
					stage, operations = 1, 0
				case "/clear":
					if stage != 1 {
						t.Errorf("clear at stage %d", stage)
					}
					stage = 2
					if failCandidate && trial >= 4 && trial <= 6 {
						return response(http.StatusServiceUnavailable, "clear failed"), nil
					}
				case "/prepare":
					if stage != 2 {
						t.Errorf("prepare at stage %d", stage)
					}
					stage = 3
				case "/operation":
					if stage != 3 {
						t.Errorf("operation at stage %d", stage)
					}
					operations++
				case "/state":
					if stage != 3 || operations < 2 {
						t.Errorf("observation at stage %d with %d operations", stage, operations)
					}
					stage = 4
					return response(http.StatusOK, `{"stock":-1}`), nil
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
				}
				return response(http.StatusNoContent, ""), nil
			})}
			scenario := scenarioWithSetup(4, 4)
			scenario.Setup[0].Name = "Reset"
			for _, path := range []string{"clear", "prepare"} {
				scenario.Setup = append(scenario.Setup, engine.SetupStep{Name: path,
					Request: engine.HTTPRequest{Method: http.MethodPost, URL: "http://example.test/" + path}})
			}
			result, err := Reduce(context.Background(), client, scenario, 3)
			wantTrials, wantAttempts := 6, 2
			if failCandidate {
				wantTrials, wantAttempts = 9, 3
			}
			if err != nil || result.Status != StatusReduced || trial != wantTrials || result.Selected.Attempts != wantAttempts {
				t.Fatalf("reduction: status=%s, trials=%d, selected=%+v, error=%v", result.Status, trial, result.Selected, err)
			}
			if failCandidate && result.Candidates[0].Summary.Errored != 3 {
				t.Fatalf("candidate setup errors lost: %+v", result.Candidates[0])
			}
			for _, trials := range []engine.TrialsResult{result.Baseline, *result.SelectedTrials} {
				for _, trial := range trials.Trials {
					if len(trial.Run.Setup) != 3 || trial.Run.Setup[2].Name != "prepare" {
						t.Fatalf("incomplete setup evidence: %+v", trial.Run.Setup)
					}
				}
			}
		})
	}
}
