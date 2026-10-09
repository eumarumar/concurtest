package report_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/eumarumar/concurtest/internal/engine"
	"github.com/eumarumar/concurtest/internal/failure"
	"github.com/eumarumar/concurtest/internal/report"
)

func TestSetupReportsRetainOrderedPartialEvidence(t *testing.T) {
	t.Parallel()
	input := completedTextInput(engine.RunOutcomePassed, 0)
	input.Scenario.Setup[0].Name = "Reset inventory"
	for _, name := range []string{"Clear basket", "Prepare basket"} {
		input.Scenario.Setup = append(input.Scenario.Setup, engine.SetupStep{Name: name,
			Request: engine.HTTPRequest{Method: http.MethodPost, URL: "http://example.test/" + strings.ReplaceAll(name, " ", "-"),
				Header: http.Header{"Authorization": {"setup-header-secret"}}, Body: []byte("setup-body-secret")}})
	}
	trial := &input.Result.Trials[0]
	trial.Status = engine.TrialStatusErrored
	trial.Err = failure.New(failure.CodeUnexpectedHTTPStatus, `setup step 2 ("Clear basket") scenario request: unexpected HTTP status 503`)
	trial.Run = engine.RunResult{StartedAt: trial.Run.StartedAt, CompletedAt: trial.Run.CompletedAt,
		Setup: []engine.SetupExecution{
			{Name: "Reset inventory", Execution: trial.Run.Setup[0].Execution},
			{Name: "Clear basket", Execution: engine.HTTPExecution{Request: input.Scenario.Setup[1].Request,
				Response: &engine.HTTPResponse{StatusCode: 503, Body: bytes.Repeat([]byte("x"), 800)}}},
		}}
	input.Result.Status = engine.TrialStatusErrored

	var output bytes.Buffer
	if err := report.WriteJSON(&output, input); err != nil {
		t.Fatal(err)
	}
	validateReportJSON(t, output.Bytes())
	var document struct {
		Scenario struct {
			Setup []struct {
				Name    string
				Request struct{ Method, Target string }
			}
		}
		Trials []struct {
			Evidence struct {
				Setup []struct {
					Name      string
					Execution struct {
						Response struct {
							StatusCode  int `json:"status_code"`
							BodyExcerpt struct {
								RetainedBytes int `json:"retained_bytes"`
								Truncated     bool
							} `json:"body_excerpt"`
						}
					}
				}
			}
		}
	}
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	configured, attempted := document.Scenario.Setup, document.Trials[0].Evidence.Setup
	if len(configured) != 3 || len(attempted) != 2 || configured[2].Name != "Prepare basket" || attempted[1].Name != "Clear basket" || attempted[1].Execution.Response.StatusCode != 503 {
		t.Fatalf("lost setup order or partial evidence: %s", &output)
	}
	if attempted[1].Execution.Response.BodyExcerpt.RetainedBytes != 512 || !attempted[1].Execution.Response.BodyExcerpt.Truncated {
		t.Fatal("setup response excerpt is not bounded")
	}
	for _, secret := range []string{"setup-header-secret", "setup-body-secret"} {
		if strings.Contains(output.String(), secret) {
			t.Fatalf("setup report exposed %s", secret)
		}
	}
	for _, verbose := range []bool{false, true} {
		output.Reset()
		if err := report.WriteTextWithOptions(&output, input, report.TextOptions{Verbose: verbose}); err != nil {
			t.Fatal(err)
		}
		assertContains(t, output.String(), `Setup #2 · "Clear basket"`, "HTTP 503")
		if verbose {
			assertContains(t, output.String(), `Setup #1 · "Reset inventory"`, `Setup #3 · "Prepare basket"`, "Not reached.")
		} else if strings.Contains(output.String(), "POST /reset") || strings.Contains(output.String(), "POST /Prepare-basket") {
			t.Fatal("compact report included successful or unattempted setup requests")
		}
	}
}

func TestJSONSetupUsesEmptyArraysWhenNoStepsWereAttempted(t *testing.T) {
	t.Parallel()
	input := completedTextInput(engine.RunOutcomePassed, 0)
	input.Scenario.Setup = nil
	input.Result.Trials[0].Run.Setup = nil
	var output bytes.Buffer
	if err := report.WriteJSON(&output, input); err != nil {
		t.Fatal(err)
	}
	validateReportJSON(t, output.Bytes())
	if strings.Count(output.String(), `"setup": []`) != 2 {
		t.Fatalf("setup is not consistently an array: %s", &output)
	}
}
