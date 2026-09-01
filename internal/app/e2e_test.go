package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	inventoryapp "github.com/eumarumar/concurtest/examples/vulnerable-inventory/app"
	"github.com/eumarumar/concurtest/internal/app"
)

func TestVulnerableInventoryEndToEnd(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(inventoryapp.NewHandler())
	t.Cleanup(server.Close)
	scenarioPath := scenarioForTarget(t, "scenario.yaml", server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var stdout, stderr bytes.Buffer
	exitCode := app.Run(ctx, []string{"run", scenarioPath}, &stdout, &stderr)
	if exitCode != 1 {
		t.Fatalf(
			"Run() exit code = %d, want 1\nstdout:\n%s\nstderr:\n%s",
			exitCode,
			stdout.String(),
			stderr.String(),
		)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}

	report := stdout.String()
	assertOutputContains(t, report,
		"ConcurTest · inventory oversell",
		"Target · "+server.URL,
		"VIOLATED",
		"Requested       10",
		"Completed       10",
		"10 of 10 completed trials demonstrated the violation",
		"First violation Trial 1",
		"Status          REDUCED",
		"Candidates      1 evaluated",
		"Smallest observed failure",
		"Attempts      2",
		"Concurrency   2",
		"Violations    10 of 10 trials",
		"not proof that no smaller failure exists",
		"Expected        $[\"stock\"] >= 0",
		"Observed        $[\"stock\"] = -1",
		"Attempt #1",
		"Attempt #2",
		"Trials 2–10 had the same violation evidence as Trial 1.",
		"concurtest run --attempts 2 --concurrency 2 --no-reduce "+scenarioPath,
	)
	if count := strings.Count(report, "HTTP 201 Created"); count < 4 || count >= 60 {
		t.Errorf("successful purchase statuses = %d, want compact representative evidence\n%s", count, report)
	}
	if count := strings.Count(report, `Response        "{\"accepted\":true}\n"`); count < 4 || count >= 60 {
		t.Errorf("successful purchase responses = %d, want compact representative evidence\n%s", count, report)
	}
	if count := strings.Count(report, "Violation · Trial"); count < 1 || count >= 10 {
		t.Errorf("baseline representative sections = %d, want compact distinct evidence\n%s", count, report)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/state", nil)
	if err != nil {
		t.Fatalf("create final state request: %v", err)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("observe final state: %v", err)
	}
	var state struct {
		Stock int `json:"stock"`
	}
	decodeErr := json.NewDecoder(response.Body).Decode(&state)
	closeErr := response.Body.Close()
	if decodeErr != nil {
		t.Fatalf("decode final state: %v", decodeErr)
	}
	if closeErr != nil {
		t.Fatalf("close final state response: %v", closeErr)
	}
	if response.StatusCode != http.StatusOK || state.Stock != -1 {
		t.Errorf("final state = status %d stock %d, want status 200 and stock -1", response.StatusCode, state.Stock)
	}
}

func TestVulnerableInventoryHistoryEndToEnd(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(inventoryapp.NewHandler())
	t.Cleanup(server.Close)
	scenarioPath := scenarioForTarget(t, "history-scenario.yaml", server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var stdout, stderr bytes.Buffer
	exitCode := app.Run(ctx, []string{"run", scenarioPath}, &stdout, &stderr)
	if exitCode != 1 {
		t.Fatalf(
			"Run() exit code = %d, want 1\nstdout:\n%s\nstderr:\n%s",
			exitCode,
			stdout.String(),
			stderr.String(),
		)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}

	report := stdout.String()
	assertOutputContains(t, report,
		"ConcurTest · inventory accepted purchases",
		"Target · "+server.URL,
		"VIOLATED",
		"Requested       10",
		"Completed       10",
		"10 of 10 completed trials demonstrated the violation",
		"First violation Trial 1",
		"Status          REDUCED",
		"Candidates      1 evaluated",
		"Smallest observed failure",
		"Attempts      2",
		"Concurrency   2",
		"Violations    10 of 10 trials",
		"Expected        At most 1 successful attempt",
		"Success statuses HTTP 201 Created",
		"Observed        2 successful attempts",
		"Successful       #1, #2",
		"Beyond maximum   #2",
		`Response        "{\"stock\":0}\n"`,
		"concurtest run --attempts 2 --concurrency 2 --no-reduce "+scenarioPath,
	)
	if count := strings.Count(report, "Beyond maximum   #2"); count < 1 || count >= 11 {
		t.Errorf("over-limit evidence fields = %d, want compact representative evidence\n%s", count, report)
	}
	if count := strings.Count(report, "HTTP 201 Created"); count < 4 {
		t.Errorf("successful purchase statuses = %d, want representative evidence\n%s", count, report)
	}
	if strings.Contains(report, "Trial 10 · VIOLATED") {
		t.Fatalf("default report expanded every equivalent trial:\n%s", report)
	}
}

func TestVulnerableInventoryVerboseEndToEnd(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(inventoryapp.NewHandler())
	t.Cleanup(server.Close)
	scenarioPath := scenarioForTarget(t, "scenario.yaml", server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var stdout, stderr bytes.Buffer
	if exitCode := app.Run(ctx, []string{"run", "--verbose", scenarioPath}, &stdout, &stderr); exitCode != 1 {
		t.Fatalf("Run() exit code = %d, want 1\nstdout:\n%s\nstderr:\n%s", exitCode, stdout.String(), stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	report := stdout.String()
	if count := strings.Count(report, "Violation · Trial"); count != 10 {
		t.Fatalf("verbose baseline evidence sections = %d, want 10\n%s", count, report)
	}
	if count := strings.Count(report, "· VIOLATED"); count != 10 {
		t.Fatalf("verbose selected evidence sections = %d, want 10\n%s", count, report)
	}
	assertOutputContains(t, report, "Candidate results", "Selected candidate evidence")
	if strings.Contains(report, "same violation evidence") || strings.Contains(report, "Use --verbose") {
		t.Fatalf("verbose report included compact evidence summaries:\n%s", report)
	}
}

func TestVulnerableInventoryJSONEndToEnd(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(inventoryapp.NewHandler())
	t.Cleanup(server.Close)
	scenarioPath := scenarioForTarget(t, "scenario.yaml", server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var stdout, stderr bytes.Buffer
	exitCode := app.Run(ctx, []string{"run", "--format", "json", scenarioPath}, &stdout, &stderr)
	if exitCode != 1 {
		t.Fatalf("Run() exit code = %d, want 1\nstdout:\n%s\nstderr:\n%s", exitCode, stdout.String(), stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	var document struct {
		SchemaVersion string `json:"schema_version"`
		ReportType    string `json:"report_type"`
		Status        string `json:"status"`
		Summary       struct {
			Requested int `json:"requested"`
			Completed int `json:"completed"`
			Violated  int `json:"violated"`
		} `json:"summary"`
		Trials    []json.RawMessage `json:"trials"`
		Reduction struct {
			Status     string `json:"status"`
			Candidates []struct {
				Accepted bool              `json:"accepted"`
				Trials   []json.RawMessage `json:"trials"`
			} `json:"candidates"`
		} `json:"reduction"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
		t.Fatalf("decode JSON report: %v\n%s", err, stdout.String())
	}
	if document.SchemaVersion != "1.0.0" || document.ReportType != "run" || document.Status != "violated" {
		t.Fatalf("report identity = %#v", document)
	}
	if document.Summary.Requested != 10 || document.Summary.Completed != 10 || document.Summary.Violated != 10 || len(document.Trials) != 10 {
		t.Fatalf("baseline summary = %#v, trials = %d", document.Summary, len(document.Trials))
	}
	if document.Reduction.Status != "reduced" || len(document.Reduction.Candidates) != 1 || !document.Reduction.Candidates[0].Accepted || len(document.Reduction.Candidates[0].Trials) != 10 {
		t.Fatalf("reduction = %#v", document.Reduction)
	}
	if strings.Contains(stdout.String(), "Authorization") || strings.Contains(stdout.String(), "request-secret") {
		t.Fatalf("JSON report exposed request headers:\n%s", stdout.String())
	}
}

func scenarioForTarget(t *testing.T, scenarioName, target string) string {
	t.Helper()
	path := filepath.Join("..", "..", "examples", "vulnerable-inventory", scenarioName)
	document, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read published scenario: %v", err)
	}
	const publishedTarget = "target: http://127.0.0.1:8080"
	if count := strings.Count(string(document), publishedTarget); count != 1 {
		t.Fatalf("published target occurrences = %d, want 1", count)
	}
	document = []byte(strings.Replace(string(document), publishedTarget, "target: "+target, 1))

	temporaryPath := filepath.Join(t.TempDir(), "scenario.yaml")
	if err := os.WriteFile(temporaryPath, document, 0o600); err != nil {
		t.Fatalf("write temporary scenario: %v", err)
	}
	return temporaryPath
}
