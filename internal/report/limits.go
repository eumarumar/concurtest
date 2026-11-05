package report

import (
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"

	"github.com/eumarumar/concurtest/internal/engine"
	"github.com/eumarumar/concurtest/internal/failure"
)

type limitsJSON struct {
	MaxAttempts      int   `json:"max_attempts"`
	MaxConcurrency   int   `json:"max_concurrency"`
	MaxTotalAttempts int64 `json:"max_total_attempts"`
	MaxExchanges     int64 `json:"max_exchanges"`
	MaxRetainedBytes int64 `json:"max_retained_bytes"`
	MaxDurationNS    int64 `json:"max_duration_ns"`
	MaxReportBytes   int64 `json:"max_report_bytes"`
}

type resourcesJSON struct {
	AttemptSlots  int64   `json:"attempt_slots"`
	Exchanges     int64   `json:"exchanges"`
	RetainedBytes int64   `json:"retained_bytes"`
	ElapsedNS     int64   `json:"elapsed_ns"`
	Exhausted     *string `json:"exhausted"`
}

func configuredLimits(l engine.Limits) limitsJSON {
	l = l.Effective()
	return limitsJSON{l.MaxAttempts, l.MaxConcurrency, l.MaxTotalAttempts, l.MaxExchanges, l.MaxRetainedBytes, int64(l.MaxDuration), l.MaxReportBytes}
}

func executionResources(input Input) *engine.ResourceUsage {
	if input.Reduction != nil && input.Reduction.Resources != nil {
		return input.Reduction.Resources
	}
	return input.Result.Resources
}

func resourceUsageJSON(input Input) *resourcesJSON {
	u := executionResources(input)
	if u == nil {
		return nil
	}
	return &resourcesJSON{u.AttemptSlots, u.Exchanges, u.RetainedBytes, int64(u.Elapsed), optionalString(u.Exhausted)}
}

func writeResources(writer io.Writer, input Input) {
	l := input.Scenario.Limits.Effective()
	fmt.Fprintf(writer, "\nResources\n  Limits          %d attempts/trial · %d workers · %d total attempt slots · %d exchanges\n", l.MaxAttempts, l.MaxConcurrency, l.MaxTotalAttempts, l.MaxExchanges)
	fmt.Fprintf(writer, "  Evidence        %d bytes · execution time %s · report budget %d bytes\n", l.MaxRetainedBytes, l.MaxDuration, l.MaxReportBytes)
	if u := executionResources(input); u != nil {
		fmt.Fprintf(writer, "  Used            %d attempt slots · %d exchanges · %d retained bytes\n", u.AttemptSlots, u.Exchanges, u.RetainedBytes)
		if u.Exhausted != "" {
			fmt.Fprintf(writer, "  Limit reached   %s; execution is incomplete. Earlier violations remain recorded.\n", u.Exhausted)
		}
	}
}

// encoding/json buffers a complete value even when its writer is bounded.
// Preflight the closed report structs before encoding: six bytes per string
// byte covers JSON escaping, and indentation/field/scalar costs are deliberately
// conservative. time.Time is the only custom marshaler in the report model.
func reportFits(value any, maximum int64) bool {
	remaining := maximum
	consume := func(n int64) bool {
		if n > remaining {
			return false
		}
		remaining -= n
		return true
	}
	var visit func(reflect.Value, int) bool
	visit = func(v reflect.Value, depth int) bool {
		if depth > 128 {
			return false
		}
		if !v.IsValid() {
			return consume(4)
		}
		if v.Type() == reflect.TypeFor[time.Time]() {
			return consume(64)
		}
		switch v.Kind() {
		case reflect.Pointer, reflect.Interface:
			if v.IsNil() {
				return consume(4)
			}
			return visit(v.Elem(), depth)
		case reflect.String:
			return consume(2) && int64(v.Len()) <= remaining/6 && consume(6*int64(v.Len()))
		case reflect.Struct:
			if !consume(int64(depth*2 + 8)) {
				return false
			}
			for index := 0; index < v.NumField(); index++ {
				field := v.Type().Field(index)
				if field.PkgPath != "" {
					continue
				}
				tag := strings.Split(field.Tag.Get("json"), ",")
				if tag[0] == "-" {
					continue
				}
				if !consume(int64(depth*2+16+len(tag[0])*6)) || !visit(v.Field(index), depth+1) {
					return false
				}
			}
			return true
		case reflect.Slice, reflect.Array:
			if !consume(int64(depth*2 + 8)) {
				return false
			}
			for index := 0; index < v.Len(); index++ {
				if !consume(int64(depth*2+16)) || !visit(v.Index(index), depth+1) {
					return false
				}
			}
			return true
		case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
			return consume(32)
		}
		return false
	}
	return visit(reflect.ValueOf(value), 0)
}

type exactWriter struct{ io.Writer }

func (w exactWriter) Write(data []byte) (int, error) {
	n, err := w.Writer.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return n, err
}

func reportLimitError(maximum int64) error {
	return failure.New(failure.CodeResourceLimit, fmt.Sprintf("report exceeds the conservative %d-byte output budget; full evidence was not emitted", maximum))
}

type budgetWriter struct {
	writer             io.Writer
	remaining, maximum int64
}

func (w *budgetWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.remaining {
		return 0, reportLimitError(w.maximum)
	}
	n, err := w.writer.Write(data)
	w.remaining -= int64(n)
	return n, err
}
