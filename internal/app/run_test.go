package app_test

import (
	"bytes"
	"context"
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
			if !strings.Contains(stdout.String(), "concurtest run <scenario.yaml>") {
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
			targetShownBeforeRequests.Store(strings.Contains(output.String(), "Target: "+serverURL(request)))
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
		"Scenario: \"inventory oversell\"",
		"Target: "+server.URL,
		"Warning: this command sends concurrent requests and may modify target state.",
		"Result: VIOLATED",
		`Expected: "stock" >= 0`,
		`Observed: "stock" = -1`,
		`#1 "purchase"`,
		`#2 "purchase"`,
		"Status: HTTP 201 Created",
		`Response: "{\"stock\":-1}"`,
		"Reproduce: concurtest run "+fmt.Sprintf("%q", path),
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
	assertOutputContains(t, stdout.String(), "Result: PASSED", `Observed: "stock" = 0`)
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

	path := writeScenarioFile(t, scenarioYAML(server.URL, "20ms", 1, 1, false))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	if code := app.Run(ctx, []string{"run", path}, &stdout, &stderr); code != 2 {
		t.Fatalf("Run() exit code = %d, want 2\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	assertOutputContains(t, stdout.String(),
		"Result: INCONCLUSIVE",
		"1 of 1 operation attempts failed or did not start",
		"Client.Timeout exceeded",
	)
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
	assertOutputContains(t, stdout.String(), "Result: ERROR", "Problem: the run was canceled.")
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
		writer := &failOnWriteWriter{failAt: 2, err: wantErr}
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

observation:
  method: GET
  path: /state

invariant:
  name: final stock must be non-negative
  json_integer_field: stock
  minimum: 0
`, target, timeout, setup, attempts, concurrency)
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
