package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eumarumar/concurtest/internal/app"
)

func TestRunShowsHelp(t *testing.T) {
	t.Parallel()

	tests := [][]string{
		{"help"},
		{"-h"},
		{"--help"},
		{"run", "-h"},
		{"run", "--help"},
	}
	for _, args := range tests {
		args := args
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			if code := app.Run(context.Background(), args, &stdout, &stderr); code != 0 {
				t.Errorf("Run() exit code = %d, want 0", code)
			}
			if !strings.Contains(stdout.String(), "concurtest run [--attempts N] [--concurrency N] [--no-reduce] [--format text|json] [--verbose] [--color auto|always|never] <scenario.yaml>") {
				t.Errorf("stdout does not contain usage:\n%s", stdout.String())
			}
			if stderr.Len() != 0 {
				t.Errorf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}

func TestRunRejectsInvalidArguments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "no command", want: "expected the run command and a scenario file"},
		{name: "unknown command", args: []string{"check"}, want: `unknown command "check"`},
		{name: "missing scenario", args: []string{"run"}, want: "run needs a scenario file"},
		{name: "extra argument", args: []string{"run", "scenario.yaml", "extra"}, want: "run accepts exactly one scenario file"},
		{name: "unknown option", args: []string{"run", "--unknown", "scenario.yaml"}, want: `unknown run option "--unknown"`},
		{name: "missing option value", args: []string{"run", "--attempts"}, want: "--attempts needs a positive integer"},
		{name: "invalid option value", args: []string{"run", "--concurrency=zero", "scenario.yaml"}, want: "--concurrency must be a positive integer"},
		{name: "duplicate option", args: []string{"run", "--attempts=2", "--attempts=3", "scenario.yaml"}, want: "run accepts --attempts only once"},
		{name: "invalid format", args: []string{"run", "--format=xml", "scenario.yaml"}, want: `--format must be text or json, got "xml"`},
		{name: "missing format", args: []string{"run", "--format"}, want: "--format needs text or json"},
		{name: "invalid color", args: []string{"run", "--color=sometimes", "scenario.yaml"}, want: `--color must be auto, always, or never, got "sometimes"`},
		{name: "missing color", args: []string{"run", "--color"}, want: "--color needs auto, always, or never"},
		{name: "duplicate color", args: []string{"run", "--color=auto", "--color=never", "scenario.yaml"}, want: "run accepts --color only once"},
		{name: "duplicate verbose", args: []string{"run", "--verbose", "--verbose", "scenario.yaml"}, want: "run accepts --verbose only once"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			if code := app.Run(context.Background(), test.args, &stdout, &stderr); code != 2 {
				t.Errorf("Run() exit code = %d, want 2", code)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty", stdout.String())
			}
			if !strings.Contains(stderr.String(), test.want) || !strings.Contains(stderr.String(), "Usage:") {
				t.Errorf("stderr does not explain the command error:\n%s", stderr.String())
			}
		})
	}
}

func TestRunWritesJSONForEarlyFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		args     []string
		wantCode string
		wantPath string
	}{
		{
			name:     "command failure",
			args:     []string{"run", "--format", "json", "--unknown"},
			wantCode: "invalid_command",
		},
		{
			name:     "scenario file failure",
			args:     []string{"run", filepath.Join(t.TempDir(), "missing.yaml"), "--format=json"},
			wantCode: "scenario_file_failed",
			wantPath: "missing.yaml",
		},
		{
			name:     "duplicate format after JSON selection",
			args:     []string{"run", "--format=json", "--format=text", "scenario.yaml"},
			wantCode: "invalid_command",
		},
		{
			name:     "verbose conflicts with JSON",
			args:     []string{"run", "--format=json", "--verbose", "scenario.yaml"},
			wantCode: "invalid_command",
		},
		{
			name:     "color conflicts with JSON",
			args:     []string{"run", "--color=never", "--format", "json", "scenario.yaml"},
			wantCode: "invalid_command",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := app.Run(context.Background(), test.args, &stdout, &stderr); code != 2 {
				t.Fatalf("Run() exit code = %d, want 2", code)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
			var document struct {
				SchemaVersion string `json:"schema_version"`
				ReportType    string `json:"report_type"`
				Error         struct {
					Code string `json:"code"`
				} `json:"error"`
				Context struct {
					ScenarioPath *string `json:"scenario_path"`
				} `json:"context"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
				t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
			}
			if document.SchemaVersion != "1.0.0" || document.ReportType != "error" || document.Error.Code != test.wantCode {
				t.Fatalf("error report = %#v\n%s", document, stdout.String())
			}
			if test.wantPath != "" && (document.Context.ScenarioPath == nil || !strings.Contains(*document.Context.ScenarioPath, test.wantPath)) {
				t.Fatalf("scenario path = %#v", document.Context.ScenarioPath)
			}
		})
	}
}

func TestRunWritesJSONRunReportAndPreservesExitCode(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/purchase":
			writer.Header().Set("X-Secret", "response-secret")
			writer.WriteHeader(http.StatusCreated)
			_, _ = writer.Write([]byte(`{"accepted":true}`))
		case "/state":
			_, _ = writer.Write([]byte(`{"stock":-1}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	path := writeScenarioFile(t, scenarioYAML(server.URL, "1s", 1, 1, false))

	var stdout, stderr bytes.Buffer
	if code := app.Run(context.Background(), []string{"run", path, "--format=json"}, &stdout, &stderr); code != 1 {
		t.Fatalf("Run() exit code = %d, want 1\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "Scenario:") || strings.Contains(stdout.String(), "Warning:") || strings.Contains(stdout.String(), "request-secret") || strings.Contains(stdout.String(), "response-secret") {
		t.Fatalf("JSON output contains text preamble or secrets:\n%s", stdout.String())
	}
	var document struct {
		ReportType string `json:"report_type"`
		Status     string `json:"status"`
		Summary    struct {
			Violated int `json:"violated"`
		} `json:"summary"`
		Trials []json.RawMessage `json:"trials"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if document.ReportType != "run" || document.Status != "violated" || document.Summary.Violated != 1 || len(document.Trials) != 1 {
		t.Fatalf("run report = %#v", document)
	}
}

func TestRunReportsScenarioFileErrors(t *testing.T) {
	t.Parallel()

	malformedPath := writeScenarioFile(t, "version: [")
	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "missing", path: filepath.Join(t.TempDir(), "missing.yaml"), want: "missing.yaml"},
		{name: "malformed", path: malformedPath, want: "read scenario YAML"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			if code := app.Run(context.Background(), []string{"run", test.path}, &stdout, &stderr); code != 2 {
				t.Errorf("Run() exit code = %d, want 2", code)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty", stdout.String())
			}
			if !strings.Contains(stderr.String(), "Could not load scenario") || !strings.Contains(stderr.String(), test.want) {
				t.Errorf("stderr does not explain the file error:\n%s", stderr.String())
			}
		})
	}
}

func TestRunReportsViolationAndPrintsTargetBeforeRequests(t *testing.T) {
	t.Parallel()

	var stock atomic.Int64
	var output synchronizedBuffer
	var targetShownBeforeRequests atomic.Bool
	var requestNumber atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if requestNumber.Add(1) == 1 {
			targetShownBeforeRequests.Store(strings.Contains(output.String(), "Target · "+serverURL(request)))
		}
		switch request.URL.Path {
		case "/reset":
			stock.Store(1)
			writer.WriteHeader(http.StatusNoContent)
		case "/purchase":
			stock.Add(-1)
			writer.Header().Set("X-Secret", "response-secret")
			writer.WriteHeader(http.StatusCreated)
			_, _ = writer.Write([]byte(`{"accepted":true}`))
		case "/state":
			_, _ = fmt.Fprintf(writer, `{"stock":%d}`, stock.Load())
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)

	path := writeScenarioFile(t, scenarioYAML(server.URL, "1s", 2, 2, true))
	var stderr bytes.Buffer
	if code := app.Run(context.Background(), []string{"run", path}, &output, &stderr); code != 1 {
		t.Fatalf("Run() exit code = %d, want 1\nstdout:\n%s\nstderr:\n%s", code, output.String(), stderr.String())
	}
	if !targetShownBeforeRequests.Load() {
		t.Error("target was not printed before the first request")
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
	assertOutputContains(t, output.String(),
		"ConcurTest · inventory oversell",
		"Target · "+server.URL,
		"Warning · This run sends concurrent requests and may change target data.",
		"VIOLATED",
		"Requested       1",
		"Completed       1",
		"1/1 trials demonstrated the violation",
		"First violation Trial 1",
		"Expected        $[\"stock\"] >= 0",
		"Observed        $[\"stock\"] = -1",
		"Attempt #1",
		"Attempt #2",
		"HTTP 201 Created",
		`Response        "{\"stock\":-1}"`,
		"concurtest run --attempts 2 --concurrency 2 --no-reduce "+path,
	)
	if strings.Contains(output.String(), "request-secret") || strings.Contains(output.String(), "response-secret") {
		t.Fatalf("output exposed a header value:\n%s", output.String())
	}
}

func serverURL(request *http.Request) string {
	return "http://" + request.Host
}

func TestRunReturnsSuccessForTrustworthyPass(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/purchase":
			writer.WriteHeader(http.StatusCreated)
		case "/state":
			_, _ = writer.Write([]byte(`{"stock":0}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)

	path := writeScenarioFile(t, scenarioYAML(server.URL, "1s", 1, 1, false))
	var stdout, stderr bytes.Buffer
	if code := app.Run(context.Background(), []string{"run", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("Run() exit code = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	assertOutputContains(t, stdout.String(), "PASSED", "Requested       1", "1/1 trials passed.", "Run with --verbose for all trial evidence.")
	if strings.Contains(stdout.String(), "Trial 1 · PASSED") {
		t.Fatalf("passing trial evidence was expanded:\n%s", stdout.String())
	}
}

func TestRunAppliesTextPresentationOptionsWithoutChangingExecution(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		switch request.URL.Path {
		case "/purchase":
			writer.WriteHeader(http.StatusCreated)
		case "/state":
			_, _ = writer.Write([]byte(`{"stock":0}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	path := writeScenarioFile(t, scenarioYAML(server.URL, "1s", 1, 1, false))
	t.Setenv("NO_COLOR", "1")

	tests := []struct {
		name       string
		args       []string
		wantANSI   bool
		wantDetail bool
	}{
		{name: "auto respects NO_COLOR", args: []string{"run", path}},
		{name: "never stays plain", args: []string{"run", "--color=never", path}},
		{name: "always overrides NO_COLOR", args: []string{"run", "--color", "always", path}, wantANSI: true},
		{name: "verbose expands passing evidence", args: []string{"run", "--verbose", path}, wantDetail: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := app.Run(context.Background(), test.args, &stdout, &stderr); code != 0 {
				t.Fatalf("Run() exit code = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
			}
			if got := strings.Contains(stdout.String(), "\x1b["); got != test.wantANSI {
				t.Fatalf("ANSI presence = %t, want %t\n%q", got, test.wantANSI, stdout.String())
			}
			if got := strings.Contains(stdout.String(), "Trial 1 · PASSED"); got != test.wantDetail {
				t.Fatalf("passing detail presence = %t, want %t\n%s", got, test.wantDetail, stdout.String())
			}
		})
	}
	if requests.Load() != 8 {
		t.Fatalf("requests = %d, want 8 across four identical two-request runs", requests.Load())
	}
}

func TestRunUsesRequestTimeoutAndReturnsInconclusive(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/purchase":
			<-request.Context().Done()
		case "/state":
			_, _ = writer.Write([]byte(`{"stock":0}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)

	path := writeScenarioFile(t, scenarioYAML(server.URL, "200ms", 1, 1, false))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	if code := app.Run(ctx, []string{"run", path}, &stdout, &stderr); code != 2 {
		t.Fatalf("Run() exit code = %d, want 2\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	assertOutputContains(t, stdout.String(),
		"INCONCLUSIVE",
		"1/1 attempts failed or did not start",
		"Client.Timeout exceeded",
	)
}

func TestRunContinuesAfterTrialErrorAndViolationControlsExitCode(t *testing.T) {
	t.Parallel()

	var setups atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/reset":
			if setups.Add(1) == 1 {
				http.Error(writer, "setup unavailable", http.StatusServiceUnavailable)
				return
			}
			writer.WriteHeader(http.StatusNoContent)
		case "/purchase":
			writer.WriteHeader(http.StatusCreated)
		case "/state":
			_, _ = writer.Write([]byte(`{"stock":-1}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)

	document := strings.Replace(scenarioYAML(server.URL, "1s", 1, 1, true), "trials: 1", "trials: 2", 1)
	path := writeScenarioFile(t, document)
	var stdout, stderr bytes.Buffer
	if code := app.Run(context.Background(), []string{"run", path}, &stdout, &stderr); code != 1 {
		t.Fatalf("Run() exit code = %d, want 1\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
	assertOutputContains(t, stdout.String(),
		"VIOLATED",
		"Requested       2",
		"Completed       2",
		"Violated        1",
		"Errored         1",
		"First violation Trial 2",
		"Problem · Baseline · Trial 1 · ERRORED",
		"Baseline failure · Trial 2",
	)
}

func TestRunReturnsErrorForErroredTrial(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/reset" {
			http.Error(writer, "setup unavailable", http.StatusServiceUnavailable)
			return
		}
		http.NotFound(writer, request)
	}))
	t.Cleanup(server.Close)

	path := writeScenarioFile(t, scenarioYAML(server.URL, "1s", 1, 1, true))
	var stdout, stderr bytes.Buffer
	if code := app.Run(context.Background(), []string{"run", path}, &stdout, &stderr); code != 2 {
		t.Fatalf("Run() exit code = %d, want 2\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	assertOutputContains(t, stdout.String(), "ERROR", "Errored         1", "Trial 1 · ERRORED")
}

func TestRunReducesToSmallestObservedCandidate(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	operations := 0
	var setups atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch request.URL.Path {
		case "/reset":
			operations = 0
			setups.Add(1)
			writer.WriteHeader(http.StatusNoContent)
		case "/purchase":
			operations++
			writer.WriteHeader(http.StatusCreated)
		case "/state":
			stock := 0
			if operations >= 2 {
				stock = -1
			}
			_, _ = fmt.Fprintf(writer, `{"stock":%d}`, stock)
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)

	path := writeScenarioFile(t, reductionScenarioYAML(server.URL, 4, 4, 3))
	var stdout, stderr bytes.Buffer
	if code := app.Run(context.Background(), []string{"run", path}, &stdout, &stderr); code != 1 {
		t.Fatalf("Run() exit code = %d, want 1\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
	assertOutputContains(t, stdout.String(),
		"Reduction · Up to 100 smaller configurations may also run.",
		"Status          REDUCED",
		"Attempts        2",
		"Concurrency     2",
		"Violations      3/3 trials",
		"Smallest observed failure; a smaller one may still exist.",
		"Evidence\n  Smallest observed failure · Trial 1",
		"Run with --verbose for all trial evidence.",
		"concurtest run --attempts 2 --concurrency 2 --no-reduce "+path,
	)
	if strings.Contains(stdout.String(), "Candidates      1 evaluated") {
		t.Fatalf("compact output included reduction candidate details:\n%s", stdout.String())
	}
	if setups.Load() != 6 {
		t.Errorf("setup calls = %d, want 6 for baseline and selected candidate", setups.Load())
	}
}

func TestRunAppliesExecutionOverridesAndDisablesReduction(t *testing.T) {
	t.Parallel()

	for _, format := range []string{"text", "json"} {
		for _, concurrency := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s/concurrency=%d", format, concurrency), func(t *testing.T) {
				t.Parallel()
				var requests atomic.Int32
				var warningBeforeRequests atomic.Bool
				var stdout synchronizedBuffer
				warning := "Warning · This run sends requests and may change target data."
				if concurrency > 1 {
					warning = "Warning · This run sends concurrent requests and may change target data."
				}
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if requests.Add(1) == 1 {
						warningBeforeRequests.Store(strings.Contains(stdout.String(), warning))
					}
					switch request.URL.Path {
					case "/reset":
						writer.WriteHeader(http.StatusNoContent)
					case "/purchase":
						writer.WriteHeader(http.StatusCreated)
					case "/state":
						_, _ = writer.Write([]byte(`{"stock":-1}`))
					default:
						http.NotFound(writer, request)
					}
				}))
				t.Cleanup(server.Close)
				path := writeScenarioFile(t, reductionScenarioYAML(server.URL, 4, 4, 3))

				var stderr bytes.Buffer
				args := []string{"run", path, "--attempts=2", "--concurrency", fmt.Sprint(concurrency), "--no-reduce", "--format", format}
				if code := app.Run(context.Background(), args, &stdout, &stderr); code != 1 {
					t.Fatalf("Run() exit code = %d, want 1\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
				}
				if stderr.Len() != 0 {
					t.Fatalf("stderr = %q, want empty", stderr.String())
				}
				flags := fmt.Sprintf("--attempts 2 --concurrency %d --no-reduce ", concurrency)
				if format == "text" {
					assertOutputContains(t, stdout.String(), "Attempts        2", fmt.Sprintf("Concurrency     %d", concurrency), "concurtest run "+flags+path)
					if !warningBeforeRequests.Load() {
						t.Error("effective concurrency warning was not printed before the first request")
					}
					if concurrency == 1 && strings.Contains(stdout.String(), "concurrent requests") {
						t.Error("sequential run warning claims concurrency")
					}
					if strings.Contains(stdout.String(), "Up to 100 smaller configurations") || strings.Contains(stdout.String(), "Status          REDUCED") {
						t.Fatalf("reduction ran despite --no-reduce:\n%s", stdout.String())
					}
				} else {
					var document struct {
						Reproduction struct {
							Arguments []string `json:"arguments"`
						} `json:"reproduction"`
					}
					if err := json.Unmarshal([]byte(stdout.String()), &document); err != nil {
						t.Fatal(err)
					}
					want := "concurtest run --format json " + flags + path
					if got := strings.Join(document.Reproduction.Arguments, " "); got != want {
						t.Fatalf("reproduction command = %q, want %q", got, want)
					}
				}
				if requests.Load() != 12 {
					t.Errorf("requests = %d, want 12 for three direct trials", requests.Load())
				}
			})
		}
	}
}

func TestRunRejectsInvalidExecutionOverridesBeforeRequests(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	t.Cleanup(server.Close)
	path := writeScenarioFile(t, reductionScenarioYAML(server.URL, 4, 4, 3))
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "concurrency exceeds attempts", args: []string{"run", "--attempts=2", "--concurrency=3", path}, want: "must not exceed attempts"},
		{name: "reduction attempt minimum", args: []string{"run", "--attempts=1", "--concurrency=1", path}, want: "reduction needs at least 2 attempts"},
		{name: "reduction concurrency minimum", args: []string{"run", "--concurrency=1", path}, want: "reduction needs concurrency of at least 2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := app.Run(context.Background(), test.args, &stdout, &stderr); code != 2 {
				t.Errorf("Run() exit code = %d, want 2", code)
			}
			if stdout.Len() != 0 || !strings.Contains(stderr.String(), test.want) {
				t.Errorf("unexpected output:\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
			}
		})
	}
	if requests.Load() != 0 {
		t.Errorf("requests = %d, want 0", requests.Load())
	}
}

func TestRunPropagatesCanceledContextWithoutRequests(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	t.Cleanup(server.Close)

	path := writeScenarioFile(t, scenarioYAML(server.URL, "1s", 1, 1, false))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	if code := app.Run(ctx, []string{"run", path}, &stdout, &stderr); code != 2 {
		t.Fatalf("Run() exit code = %d, want 2", code)
	}
	if requests.Load() != 0 {
		t.Errorf("requests = %d, want 0", requests.Load())
	}
	assertOutputContains(t, stdout.String(), "ERROR", "trial sequence was canceled", "Requested       1", "Completed       0")
}

func TestRunHandlesOutputFailures(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.URL.Path == "/state" {
			_, _ = writer.Write([]byte(`{"stock":0}`))
			return
		}
		writer.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(server.Close)
	path := writeScenarioFile(t, scenarioYAML(server.URL, "1s", 1, 1, false))
	wantErr := errors.New("output unavailable")

	t.Run("before execution", func(t *testing.T) {
		var stderr bytes.Buffer
		if code := app.Run(context.Background(), []string{"run", path}, errorWriter{err: wantErr}, &stderr); code != 2 {
			t.Errorf("Run() exit code = %d, want 2", code)
		}
		if requests.Load() != 0 {
			t.Errorf("requests = %d, want 0", requests.Load())
		}
		if !strings.Contains(stderr.String(), "Could not write the run details") {
			t.Errorf("stderr does not explain output failure: %s", stderr.String())
		}
	})

	t.Run("while reporting", func(t *testing.T) {
		requests.Store(0)
		writer := &failOnWriteWriter{failAt: 3, err: wantErr}
		var stderr bytes.Buffer
		if code := app.Run(context.Background(), []string{"run", path}, writer, &stderr); code != 2 {
			t.Errorf("Run() exit code = %d, want 2", code)
		}
		if requests.Load() != 2 {
			t.Errorf("requests = %d, want operation and observation", requests.Load())
		}
		if !strings.Contains(stderr.String(), "Could not write the run report") {
			t.Errorf("stderr does not explain report failure: %s", stderr.String())
		}
	})
}

func writeScenarioFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scenario.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write scenario: %v", err)
	}
	return path
}

func scenarioYAML(target, timeout string, attempts, concurrency int, includeSetup bool) string {
	setup := ""
	if includeSetup {
		setup = `setup:
  method: POST
  path: /reset
  headers:
    Authorization: request-secret

`
	}
	return fmt.Sprintf(`version: 1
name: inventory oversell
target: %s
request_timeout: %s

%soperation:
  name: purchase
  method: POST
  path: /purchase
  headers:
    Authorization: request-secret

execution:
  attempts: %d
  concurrency: %d
  trials: 1

observation:
  method: GET
  path: /state

invariant:
  name: final stock must be non-negative
  json_integer_path: [stock]
  minimum: 0
`, target, timeout, setup, attempts, concurrency)
}

func reductionScenarioYAML(target string, attempts, concurrency, trials int) string {
	document := scenarioYAML(target, "1s", attempts, concurrency, true)
	return strings.Replace(
		document,
		"  trials: 1\n",
		fmt.Sprintf("  trials: %d\n  reduce: true\n", trials),
		1,
	)
}

func assertOutputContains(t *testing.T, output string, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if !strings.Contains(output, fragment) {
			t.Errorf("output does not contain %q:\n%s", fragment, output)
		}
	}
}

type synchronizedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *synchronizedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(data)
}

func (buffer *synchronizedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}

type errorWriter struct {
	err error
}

func (writer errorWriter) Write([]byte) (int, error) {
	return 0, writer.err
}

type failOnWriteWriter struct {
	writes int
	failAt int
	err    error
}

func (writer *failOnWriteWriter) Write(data []byte) (int, error) {
	writer.writes++
	if writer.writes == writer.failAt {
		return 0, writer.err
	}
	return len(data), nil
}
