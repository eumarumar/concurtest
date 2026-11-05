package scenario_test

import (
	"strings"
	"testing"
	"time"

	"github.com/eumarumar/concurtest/internal/scenario"
)

func TestDecodeRejectsOversizedWorkBeforeExecution(t *testing.T) {
	t.Parallel()
	for _, document := range []string{
		strings.Replace(validYAML("http://example.test"), "attempts: 2", "attempts: 2000000", 1),
		strings.NewReplacer("attempts: 2", "attempts: 2000000", "concurrency: 2", "concurrency: 2000000").Replace(validYAML("http://example.test")),
	} {
		if _, err := scenario.Decode(strings.NewReader(document)); err == nil {
			t.Fatal("oversized work accepted; no requests were executed")
		}
	}
}

func TestDecodeExecutionLimitsAndRejectInvalidValues(t *testing.T) {
	t.Parallel()
	document := validYAML("http://example.test") + `limits:
  max_attempts: 8
  max_concurrency: 2
  max_total_attempts: 12
  max_exchanges: 20
  max_retained_bytes: 4096
  max_duration: 250ms
  max_report_bytes: 65536
`
	definition, err := scenario.Decode(strings.NewReader(document))
	if err != nil {
		t.Fatal(err)
	}
	l := definition.Scenario.Limits.Effective()
	if l.MaxAttempts != 8 || l.MaxConcurrency != 2 || l.MaxTotalAttempts != 12 || l.MaxExchanges != 20 || l.MaxRetainedBytes != 4096 || l.MaxDuration != 250*time.Millisecond || l.MaxReportBytes != 65536 {
		t.Fatalf("limits not preserved: %+v", l)
	}
	for _, field := range []string{"max_attempts", "max_concurrency", "max_total_attempts", "max_exchanges", "max_retained_bytes", "max_report_bytes"} {
		for _, value := range []string{"0", "-1", "null", "true", "1.5", "'2'", "9223372036854775808"} {
			bad := validYAML("http://example.test") + "limits: {" + field + ": " + value + "}\n"
			if _, err := scenario.Decode(strings.NewReader(bad)); err == nil {
				t.Fatalf("invalid %s accepted: %s", field, value)
			}
		}
	}
	for _, limits := range []string{"{typo: 1}", "[]", "{max_duration: 0s}", "{max_duration: -1s}", "{max_duration: 31m}", "{max_duration: null}", "{max_duration: 1}", "{max_attempts: 10001}", "{max_concurrency: 257}", "{max_total_attempts: 100001}", "{max_exchanges: 100001}", "{max_retained_bytes: 67108865}", "{max_report_bytes: 67108865}", "{max_exchanges: 2, max_exchanges: 3}"} {
		if _, err := scenario.Decode(strings.NewReader(validYAML("http://example.test") + "limits: " + limits)); err == nil {
			t.Fatalf("invalid limits accepted: %s", limits)
		}
	}
}
