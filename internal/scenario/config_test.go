package scenario_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eumarumar/concurtest/internal/engine"
	"github.com/eumarumar/concurtest/internal/scenario"
)

func TestDecodeValidScenario(t *testing.T) {
	t.Parallel()

	file, err := os.Open("testdata/valid.yaml")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Errorf("close fixture: %v", err)
		}
	})

	definition, err := scenario.Decode(file)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	if definition.Name != "inventory oversell" {
		t.Errorf("name = %q, want %q", definition.Name, "inventory oversell")
	}
	if definition.Target != "http://127.0.0.1:8080" {
		t.Errorf("target = %q, want %q", definition.Target, "http://127.0.0.1:8080")
	}
	if definition.RequestTimeout != 2*time.Second {
		t.Errorf("request timeout = %v, want %v", definition.RequestTimeout, 2*time.Second)
	}
	if definition.Trials != 10 {
		t.Errorf("trials = %d, want 10", definition.Trials)
	}
	if !definition.Reduce {
		t.Error("reduce = false, want true")
	}
	if definition.Scenario.Setup == nil {
		t.Fatal("setup request is nil")
	}
	if definition.Scenario.Setup.URL != "http://127.0.0.1:8080/reset" {
		t.Errorf("setup URL = %q", definition.Scenario.Setup.URL)
	}
	if definition.Scenario.Setup.Header.Get("Content-Type") != "application/json" {
		t.Errorf("setup Content-Type = %q", definition.Scenario.Setup.Header.Get("Content-Type"))
	}
	if string(definition.Scenario.Setup.Body) != `{"stock":1}` {
		t.Errorf("setup body = %q", definition.Scenario.Setup.Body)
	}
	if definition.Scenario.Operation.Name != "purchase" {
		t.Errorf("operation name = %q", definition.Scenario.Operation.Name)
	}
	if definition.Scenario.Operation.Request.URL != "http://127.0.0.1:8080/purchase?source=config" {
		t.Errorf("operation URL = %q", definition.Scenario.Operation.Request.URL)
	}
	if definition.Scenario.Attempts != 2 || definition.Scenario.Concurrency != 2 {
		t.Errorf(
			"execution = attempts:%d concurrency:%d, want 2 and 2",
			definition.Scenario.Attempts,
			definition.Scenario.Concurrency,
		)
	}
	if definition.Scenario.Observation == nil {
		t.Fatal("observation request is nil")
	}
	if definition.Scenario.Observation.URL != "http://127.0.0.1:8080/state" {
		t.Errorf("observation URL = %q", definition.Scenario.Observation.URL)
	}
	jsonInvariant := definition.Scenario.Invariant.JSONIntegerMinimum
	if jsonInvariant == nil ||
		jsonInvariant.Name != "final stock must be non-negative" ||
		jsonInvariant.Field != "stock" ||
		jsonInvariant.Minimum != 0 {
		t.Errorf("invariant = %#v", definition.Scenario.Invariant)
	}
}

func TestDecodeAllowsOmittedSetup(t *testing.T) {
	t.Parallel()

	definition, err := scenario.Decode(strings.NewReader(validYAML("http://example.test")))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if definition.Scenario.Setup != nil {
		t.Errorf("setup = %#v, want nil", definition.Scenario.Setup)
	}
	if definition.Reduce {
		t.Error("reduce = true, want false when omitted")
	}
}

func TestDecodeProducesRunnableScenario(t *testing.T) {
	t.Parallel()

	var stock atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/purchase":
			stock.Add(-1)
			writer.WriteHeader(http.StatusCreated)
		case "/state":
			writer.Header().Set("Content-Type", "application/json")
			if _, err := fmt.Fprintf(writer, `{"stock":%d}`, stock.Load()); err != nil {
				t.Errorf("write state response: %v", err)
			}
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	stock.Store(1)

	definition, err := scenario.Decode(strings.NewReader(validYAML(server.URL)))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	client := server.Client()
	client.Timeout = definition.RequestTimeout
	result, err := engine.RunTrials(context.Background(), client, definition.Scenario, definition.Trials)
	if err != nil {
		t.Fatalf("RunTrials() error = %v", err)
	}
	if result.Status != engine.TrialStatusViolated {
		t.Errorf("status = %q, want %q", result.Status, engine.TrialStatusViolated)
	}
}

func TestDecodeMaximumSuccessfulAttemptsInvariant(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		document     string
		wantStatuses []int
		observation  bool
	}{
		{
			name:        "default 2xx without observation",
			document:    historyInvariantYAML("http://example.test", ""),
			observation: false,
		},
		{
			name: "explicit statuses with observation",
			document: strings.Replace(
				historyInvariantYAML("http://example.test", "  successful_status_codes: [201, 202]\n"),
				"invariant:\n",
				"observation:\n  method: GET\n  path: /state\n\ninvariant:\n",
				1,
			),
			wantStatuses: []int{201, 202},
			observation:  true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			definition, err := scenario.Decode(strings.NewReader(test.document))
			if err != nil {
				t.Fatalf("Decode() error = %v", err)
			}
			invariant := definition.Scenario.Invariant.MaximumSuccessfulAttempts
			if invariant == nil {
				t.Fatalf("invariant = %#v, want maximum successful attempts", definition.Scenario.Invariant)
			}
			if invariant.Name != "accepted purchases must not exceed stock" || invariant.Maximum != 1 {
				t.Errorf("invariant = %#v", invariant)
			}
			if fmt.Sprint(invariant.SuccessfulStatusCodes) != fmt.Sprint(test.wantStatuses) {
				t.Errorf("successful statuses = %v, want %v", invariant.SuccessfulStatusCodes, test.wantStatuses)
			}
			if (definition.Scenario.Observation != nil) != test.observation {
				t.Errorf("observation configured = %t, want %t", definition.Scenario.Observation != nil, test.observation)
			}
		})
	}
}

func TestDecodeRejectsInvalidMaximumSuccessfulAttemptsInvariant(t *testing.T) {
	t.Parallel()

	valid := historyInvariantYAML("http://example.test", "")
	tests := []struct {
		name     string
		document string
	}{
		{name: "negative maximum", document: strings.Replace(valid, "maximum_successful_attempts: 1", "maximum_successful_attempts: -1", 1)},
		{name: "fractional maximum", document: strings.Replace(valid, "maximum_successful_attempts: 1", "maximum_successful_attempts: 1.5", 1)},
		{name: "empty statuses", document: strings.Replace(valid, "  maximum_successful_attempts: 1\n", "  maximum_successful_attempts: 1\n  successful_status_codes: []\n", 1)},
		{name: "null statuses", document: strings.Replace(valid, "  maximum_successful_attempts: 1\n", "  maximum_successful_attempts: 1\n  successful_status_codes: null\n", 1)},
		{name: "low status", document: strings.Replace(valid, "  maximum_successful_attempts: 1\n", "  maximum_successful_attempts: 1\n  successful_status_codes: [99]\n", 1)},
		{name: "high status", document: strings.Replace(valid, "  maximum_successful_attempts: 1\n", "  maximum_successful_attempts: 1\n  successful_status_codes: [600]\n", 1)},
		{name: "duplicate status", document: strings.Replace(valid, "  maximum_successful_attempts: 1\n", "  maximum_successful_attempts: 1\n  successful_status_codes: [201, 201]\n", 1)},
		{name: "fractional status", document: strings.Replace(valid, "  maximum_successful_attempts: 1\n", "  maximum_successful_attempts: 1\n  successful_status_codes: [201.0]\n", 1)},
		{name: "string status", document: strings.Replace(valid, "  maximum_successful_attempts: 1\n", "  maximum_successful_attempts: 1\n  successful_status_codes: ['201']\n", 1)},
		{name: "status without maximum", document: strings.Replace(valid, "  maximum_successful_attempts: 1\n", "  successful_status_codes: [201]\n", 1)},
		{name: "mixed invariant definitions", document: strings.Replace(valid, "  maximum_successful_attempts: 1\n", "  maximum_successful_attempts: 1\n  json_integer_field: stock\n  minimum: 0\n", 1)},
		{name: "missing invariant definition", document: strings.Replace(valid, "  maximum_successful_attempts: 1\n", "", 1)},
		{name: "state invariant without observation", document: strings.Replace(validYAML("http://example.test"), "observation:\n  method: GET\n  path: /state\n\n", "", 1)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := scenario.Decode(strings.NewReader(test.document)); err == nil {
				t.Fatal("Decode() error = nil, want validation error")
			}
		})
	}
}

func TestDecodeRejectsInvalidScenario(t *testing.T) {
	t.Parallel()

	valid := validYAML("http://example.test")
	reducible := strings.Replace(valid, "operation:\n", "setup:\n  method: POST\n  path: /reset\n\noperation:\n", 1)
	reducible = strings.Replace(reducible, "  trials: 1\n", "  trials: 3\n  reduce: true\n", 1)
	tests := []struct {
		name     string
		document string
	}{
		{name: "empty file", document: ""},
		{name: "malformed YAML", document: "version: ["},
		{name: "unknown top-level field", document: valid + "unknown: true\n"},
		{name: "unknown nested field", document: strings.Replace(valid, "  path: /purchase", "  path: /purchase\n  unknown: true", 1)},
		{name: "duplicate key", document: valid + "name: duplicate\n"},
		{name: "multiple documents", document: valid + "---\nversion: 1\n"},
		{name: "missing version", document: strings.Replace(valid, "version: 1\n", "", 1)},
		{name: "unsupported version", document: strings.Replace(valid, "version: 1", "version: 2", 1)},
		{name: "fractional version", document: strings.Replace(valid, "version: 1", "version: 1.5", 1)},
		{name: "empty name", document: strings.Replace(valid, "name: inventory oversell", "name: ' '", 1)},
		{name: "non-string target", document: strings.Replace(valid, "target: http://example.test", "target: 123", 1)},
		{name: "invalid target scheme", document: strings.Replace(valid, "http://example.test", "ftp://example.test", 1)},
		{name: "target without hostname", document: strings.Replace(valid, "http://example.test", "http://:8080", 1)},
		{name: "target credentials", document: strings.Replace(valid, "http://example.test", "http://user:password@example.test", 1)},
		{name: "target path", document: strings.Replace(valid, "http://example.test", "http://example.test/api", 1)},
		{name: "target query", document: strings.Replace(valid, "http://example.test", "http://example.test?mode=test", 1)},
		{name: "empty target query", document: strings.Replace(valid, "http://example.test", "http://example.test?", 1)},
		{name: "target fragment", document: strings.Replace(valid, "http://example.test", "http://example.test#state", 1)},
		{name: "invalid timeout", document: strings.Replace(valid, "request_timeout: 2s", "request_timeout: soon", 1)},
		{name: "zero timeout", document: strings.Replace(valid, "request_timeout: 2s", "request_timeout: 0s", 1)},
		{name: "invalid method", document: strings.Replace(valid, "method: POST", "method: 'bad method'", 1)},
		{name: "method whitespace", document: strings.Replace(valid, "method: POST", "method: ' POST '", 1)},
		{name: "absolute request URL", document: strings.Replace(valid, "path: /purchase", "path: https://other.test/purchase", 1)},
		{name: "path whitespace", document: strings.Replace(valid, "path: /purchase", "path: '/purchase '", 1)},
		{name: "request fragment", document: strings.Replace(valid, "path: /purchase", "path: /purchase#fragment", 1)},
		{name: "duplicate header names", document: strings.Replace(valid, "    Content-Type: application/json", "    Content-Type: application/json\n    content-type: text/plain", 1)},
		{name: "invalid header name", document: strings.Replace(valid, "Content-Type", "Bad Header", 1)},
		{name: "invalid header value", document: strings.Replace(valid, "application/json", `"first\nsecond"`, 1)},
		{name: "non-string header value", document: strings.Replace(valid, "application/json", "123", 1)},
		{name: "non-string body", document: strings.Replace(valid, `body: '{"quantity":1}'`, "body: 1", 1)},
		{name: "zero attempts", document: strings.Replace(valid, "attempts: 2", "attempts: 0", 1)},
		{name: "fractional attempts", document: strings.Replace(valid, "attempts: 2", "attempts: 2.5", 1)},
		{name: "zero concurrency", document: strings.Replace(valid, "concurrency: 2", "concurrency: 0", 1)},
		{name: "concurrency exceeds attempts", document: strings.Replace(valid, "concurrency: 2", "concurrency: 3", 1)},
		{name: "missing trials", document: strings.Replace(valid, "  trials: 1\n", "", 1)},
		{name: "zero trials", document: strings.Replace(valid, "trials: 1", "trials: 0", 1)},
		{name: "negative trials", document: strings.Replace(valid, "trials: 1", "trials: -1", 1)},
		{name: "fractional trials", document: strings.Replace(valid, "trials: 1", "trials: 1.5", 1)},
		{name: "too many trials", document: strings.Replace(valid, "trials: 1", "trials: 101", 1)},
		{name: "non-boolean reduce", document: strings.Replace(valid, "  trials: 1\n", "  trials: 3\n  reduce: yes\n", 1)},
		{name: "reduce requires setup", document: strings.Replace(valid, "  trials: 1\n", "  trials: 3\n  reduce: true\n", 1)},
		{name: "reduce requires three trials", document: strings.Replace(reducible, "trials: 3", "trials: 2", 1)},
		{name: "reduce requires two attempts", document: strings.NewReplacer("attempts: 2", "attempts: 1", "concurrency: 2", "concurrency: 1").Replace(reducible)},
		{name: "reduce requires concurrency two", document: strings.Replace(reducible, "concurrency: 2", "concurrency: 1", 1)},
		{name: "empty invariant name", document: strings.Replace(valid, "name: final stock must be non-negative", "name: ' '", 1)},
		{name: "empty invariant field", document: strings.Replace(valid, "json_integer_field: stock", "json_integer_field: ' '", 1)},
		{name: "missing minimum", document: strings.Replace(valid, "  minimum: 0\n", "", 1)},
		{name: "fractional minimum", document: strings.Replace(valid, "minimum: 0", "minimum: 0.5", 1)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := scenario.Decode(strings.NewReader(test.document)); err == nil {
				t.Fatal("Decode() error = nil, want validation error")
			}
		})
	}
}

func TestDecodeRejectsOversizedScenario(t *testing.T) {
	t.Parallel()

	oversized := strings.Repeat(" ", (1<<20)+1)
	if _, err := scenario.Decode(strings.NewReader(oversized)); err == nil {
		t.Fatal("Decode() error = nil, want size error")
	}
}

func validYAML(target string) string {
	return fmt.Sprintf(`version: 1
name: inventory oversell
target: %s
request_timeout: 2s

operation:
  name: purchase
  method: POST
  path: /purchase
  headers:
    Content-Type: application/json
  body: '{"quantity":1}'

execution:
  attempts: 2
  concurrency: 2
  trials: 1

observation:
  method: GET
  path: /state

invariant:
  name: final stock must be non-negative
  json_integer_field: stock
  minimum: 0
`, target)
}

func historyInvariantYAML(target, statusLine string) string {
	return fmt.Sprintf(`version: 1
name: inventory accepted purchases
target: %s
request_timeout: 2s

operation:
  name: purchase
  method: POST
  path: /purchase

execution:
  attempts: 2
  concurrency: 2
  trials: 1

invariant:
  name: accepted purchases must not exceed stock
  maximum_successful_attempts: 1
%s`, target, statusLine)
}
