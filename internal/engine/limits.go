package engine

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/eumarumar/concurtest/internal/failure"
)

// Limits applies to one complete execution, including all reduction candidates.
// Zero fields use defaults. Hard ceilings keep explicitly raised limits bounded.
type Limits struct {
	MaxAttempts      int
	MaxConcurrency   int
	MaxTotalAttempts int64
	MaxExchanges     int64
	MaxRetainedBytes int64
	MaxDuration      time.Duration
	MaxReportBytes   int64
}

// Effective returns the defaults for omitted fields without changing the input.
func (l Limits) Effective() Limits {
	if l.MaxAttempts == 0 {
		l.MaxAttempts = 1000
	}
	if l.MaxConcurrency == 0 {
		l.MaxConcurrency = 64
	}
	if l.MaxTotalAttempts == 0 {
		l.MaxTotalAttempts = 10000
	}
	if l.MaxExchanges == 0 {
		l.MaxExchanges = 20000
	}
	if l.MaxRetainedBytes == 0 {
		l.MaxRetainedBytes = 16 << 20
	}
	if l.MaxDuration == 0 {
		l.MaxDuration = 2 * time.Minute
	}
	if l.MaxReportBytes == 0 {
		l.MaxReportBytes = 32 << 20
	}
	return l
}

func (l Limits) Validate() error {
	l = l.Effective()
	if l.MaxDuration <= 0 || l.MaxDuration > 30*time.Minute {
		return fmt.Errorf("limits.max_duration must be positive and at most 30m")
	}
	for _, limit := range []struct {
		name           string
		value, ceiling int64
	}{
		{"max_attempts", int64(l.MaxAttempts), 10000}, {"max_concurrency", int64(l.MaxConcurrency), 256},
		{"max_total_attempts", l.MaxTotalAttempts, 100000}, {"max_exchanges", l.MaxExchanges, 100000},
		{"max_retained_bytes", l.MaxRetainedBytes, 64 << 20},
		{"max_report_bytes", l.MaxReportBytes, 64 << 20},
	} {
		if limit.value <= 0 || limit.value > limit.ceiling {
			return fmt.Errorf("limits.%s must be between 1 and %d", limit.name, limit.ceiling)
		}
	}
	return nil
}

func (l Limits) ValidateWork(attempts, concurrency int) error {
	if err := l.Validate(); err != nil {
		return err
	}
	l = l.Effective()
	if attempts > l.MaxAttempts {
		return fmt.Errorf("attempts %d exceed limits.max_attempts %d", attempts, l.MaxAttempts)
	}
	if concurrency > l.MaxConcurrency {
		return fmt.Errorf("concurrency %d exceeds limits.max_concurrency %d", concurrency, l.MaxConcurrency)
	}
	if int64(attempts) > l.MaxTotalAttempts {
		return fmt.Errorf("attempts %d exceed limits.max_total_attempts %d", attempts, l.MaxTotalAttempts)
	}
	return nil
}

// ResourceUsage is a monotonic checkpoint, not process RSS. RetainedBytes counts
// captured request metadata/payloads, header overhead, and response payloads.
type ResourceUsage struct {
	Limits        Limits
	AttemptSlots  int64
	Exchanges     int64
	RetainedBytes int64
	Elapsed       time.Duration
	Exhausted     string
}

// Execution owns one deadline and shared admission/retention counters. Reduce
// reuses this owner across baseline and candidates. Rejected evidence is not
// refunded: this is a cumulative work budget, not a garbage-collector estimate.
// The caller must Close it; Close cancels active work and stops its timer.
type Execution struct {
	ctx                        context.Context
	cancel                     context.CancelCauseFunc
	stopTimer                  context.CancelFunc
	limits                     Limits
	started                    time.Time
	closedAt                   time.Time
	durationErr                error
	mu                         sync.Mutex
	slots, exchanges, retained int64
	exhausted                  string
	limitErr                   error
}

// NewExecution creates an owner whose deadline distinguishes its own limit from
// parent cancellation. Package-level runners create and close an owner for you.
func NewExecution(ctx context.Context, limits Limits) (*Execution, error) {
	if ctx == nil {
		return nil, failure.New(failure.CodeInvalidExecution, "create execution: nil context")
	}
	if err := limits.Validate(); err != nil {
		return nil, failure.Wrap(failure.CodeInvalidExecution, "validate execution limits", err)
	}
	limits = limits.Effective()
	durationErr := failure.New(failure.CodeResourceLimit, fmt.Sprintf("limits.max_duration reached (%s)", limits.MaxDuration))
	timed, stop := context.WithTimeoutCause(ctx, limits.MaxDuration, durationErr)
	bounded, cancel := context.WithCancelCause(timed)
	return &Execution{ctx: bounded, cancel: cancel, stopTimer: stop, limits: limits, started: time.Now(), durationErr: durationErr}, nil
}

// Close cancels active work, stops the deadline timer, and freezes elapsed time.
func (e *Execution) Close() {
	e.mu.Lock()
	if e.closedAt.IsZero() {
		e.closedAt = time.Now()
	}
	e.mu.Unlock()
	e.cancel(context.Canceled)
	e.stopTimer()
}

func (e *Execution) cause() error { return context.Cause(e.ctx) }

// Snapshot returns an independent checkpoint of cumulative accounting.
func (e *Execution) Snapshot() *ResourceUsage {
	e.mu.Lock()
	defer e.mu.Unlock()
	exhausted := e.exhausted
	if exhausted == "" && e.cause() == e.durationErr {
		exhausted = "max_duration"
	}
	elapsed := time.Since(e.started)
	if !e.closedAt.IsZero() {
		elapsed = e.closedAt.Sub(e.started)
	}
	return &ResourceUsage{Limits: e.limits, AttemptSlots: e.slots, Exchanges: e.exchanges, RetainedBytes: e.retained, Elapsed: elapsed, Exhausted: exhausted}
}

func (e *Execution) fail(resource string, maximum int64) error {
	e.mu.Lock()
	if e.limitErr == nil {
		e.exhausted = resource
		e.limitErr = failure.New(failure.CodeResourceLimit, fmt.Sprintf("limits.%s reached (%d)", resource, maximum))
	}
	err := e.limitErr
	e.mu.Unlock()
	e.cancel(err)
	return err
}

func (e *Execution) claim(counter *int64, amount, maximum int64, resource string) error {
	e.mu.Lock()
	if resource != "max_retained_bytes" && e.limitErr != nil {
		err := e.limitErr
		e.mu.Unlock()
		return err
	}
	if amount > maximum-*counter {
		e.mu.Unlock()
		return e.fail(resource, maximum)
	}
	*counter += amount
	e.mu.Unlock()
	return nil
}

func (e *Execution) retainBody(body []byte) (int, error) {
	e.mu.Lock()
	kept := min(int64(len(body)), e.limits.MaxRetainedBytes-e.retained)
	e.retained += kept
	e.mu.Unlock()
	if kept < int64(len(body)) {
		return int(kept), e.fail("max_retained_bytes", e.limits.MaxRetainedBytes)
	}
	return int(kept), nil
}

const MaxHTTPHeaderBytes = 64 << 10

func headerBytes(header http.Header) int64 {
	var size int64
	for name, values := range header {
		size += int64(len(name)) + 32
		for _, value := range values {
			size += int64(len(value)) + 16
		}
		if size > MaxHTTPHeaderBytes {
			return size
		}
	}
	return size
}
