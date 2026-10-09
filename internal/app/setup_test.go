package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/eumarumar/concurtest/internal/app"
)

func TestCLIOrderedSetupsAcrossTrialsAndReduction(t *testing.T) {
	t.Parallel()
	for _, failSetup := range []bool{false, true} {
		for _, format := range []string{"text", "json"} {
			t.Run(fmt.Sprintf("fail=%t/%s", failSetup, format), func(t *testing.T) {
				var mu sync.Mutex
				trials, stage, stock, operations, observations := 0, 0, 0, 0, 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					defer mu.Unlock()
					switch r.URL.Path {
					case "/reset":
						if trials > 0 && stage != 3 && !(failSetup && trials == 1 && stage == 1) {
							t.Errorf("reset before prior trial ended: stage %d", stage)
						}
						trials++
						stage, stock, operations = 1, 10, 0
					case "/prepare":
						if stage != 1 {
							t.Errorf("prepare before reset: stage %d", stage)
						}
						if failSetup && trials == 1 {
							http.Error(w, "prepare failed", http.StatusConflict)
							return
						}
						stage = 2
					case "/purchase":
						if stage != 2 {
							t.Errorf("purchase before preparation: stage %d", stage)
						}
						operations++
						stock--
					case "/state":
						if stage != 2 {
							t.Errorf("observation before preparation: stage %d", stage)
						}
						observations++
						if operations > 0 {
							stage = 3
						}
						if err := json.NewEncoder(w).Encode(map[string]int{"stock": stock}); err != nil {
							t.Errorf("write observation: %v", err)
						}
						return
					default:
						t.Errorf("unexpected request %s", r.URL.Path)
					}
					w.WriteHeader(http.StatusNoContent)
				}))
				t.Cleanup(server.Close)
				document := reductionScenarioYAML(server.URL, 4, 4, 3)
				document = strings.Replace(document, `setup:
  method: POST
  path: /reset
  headers:
    Authorization: request-secret`, `setup:
  - name: Reset inventory
    request: {method: POST, path: /reset}
  - name: Prepare basket
    request: {method: POST, path: /prepare}`, 1)
				document = strings.Replace(document, "minimum: 0", "change: -1", 1)
				path := writeScenarioFile(t, document)
				var stdout, stderr bytes.Buffer
				arguments := []string{"run", "--format", format}
				if format == "text" {
					arguments = append(arguments, "--verbose")
				}
				arguments = append(arguments, path)
				if code := app.Run(context.Background(), arguments, &stdout, &stderr); code != 1 || stderr.Len() != 0 {
					t.Fatalf("exit=%d, stdout=%s, stderr=%s", code, &stdout, &stderr)
				}
				mu.Lock()
				defer mu.Unlock()
				wantTrials, wantObservations := 6, 12
				if failSetup {
					wantTrials, wantObservations = 3, 4
				}
				if trials != wantTrials || observations != wantObservations {
					t.Fatalf("trials=%d observations=%d, want %d/%d", trials, observations, wantTrials, wantObservations)
				}
				assertOutputContains(t, stdout.String(), "Reset inventory", "Prepare basket")
				if failSetup {
					assertOutputContains(t, stdout.String(), "prepare failed")
				}
			})
		}
	}
}
