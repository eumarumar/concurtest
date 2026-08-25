package engine

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/eumarumar/concurtest/internal/failure"
)

// Operation describes one named HTTP operation.
type Operation struct {
	Name    string
	Request HTTPRequest
}

// Attempt records the identity and result of one scheduled operation attempt.
// Execution is nil when cancellation prevented the attempt from starting.
type Attempt struct {
	ID            int
	OperationName string
	Execution     *HTTPExecution
}

// History records the attempts made during one concurrent execution.
// Attempts are stored in stable ID order, not completion order.
type History struct {
	StartedAt   time.Time
	CompletedAt time.Time
	Attempts    []Attempt
}

// Duration reports the elapsed time of the concurrent execution.
func (h History) Duration() time.Duration {
	return h.CompletedAt.Sub(h.StartedAt)
}

// ExecuteConcurrent repeats one operation with bounded concurrency.
// Individual HTTP execution errors are recorded in History and do not stop
// sibling attempts. A returned error describes invalid input or cancellation of
// the overall execution.
func ExecuteConcurrent(
	ctx context.Context,
	client *http.Client,
	operation Operation,
	attempts int,
	concurrency int,
) (History, error) {
	if err := validateConcurrentInput(ctx, client, operation, attempts, concurrency); err != nil {
		return History{}, err
	}

	operation.Request = cloneHTTPRequest(operation.Request)
	history := History{Attempts: make([]Attempt, attempts)}
	jobs := make(chan int, attempts)
	for index := range history.Attempts {
		history.Attempts[index] = Attempt{
			ID:            index + 1,
			OperationName: operation.Name,
		}
		jobs <- index
	}
	close(jobs)

	start := make(chan struct{})
	var ready sync.WaitGroup
	var workers sync.WaitGroup
	ready.Add(concurrency)
	workers.Add(concurrency)

	for range concurrency {
		go func() {
			defer workers.Done()
			ready.Done()

			select {
			case <-start:
			case <-ctx.Done():
				return
			}

			for {
				if ctx.Err() != nil {
					return
				}

				select {
				case <-ctx.Done():
					return
				case index, ok := <-jobs:
					if !ok {
						return
					}
					if ctx.Err() != nil {
						return
					}

					execution := ExecuteHTTP(ctx, client, operation.Request)
					history.Attempts[index].Execution = &execution
				}
			}
		}()
	}

	ready.Wait()
	history.StartedAt = time.Now()
	close(start)
	workers.Wait()
	history.CompletedAt = time.Now()

	if err := ctx.Err(); err != nil {
		return history, failure.Wrap(failure.CodeOperationBatchFailed, "execute concurrent operation", err)
	}
	return history, nil
}

func validateConcurrentInput(
	ctx context.Context,
	client *http.Client,
	operation Operation,
	attempts int,
	concurrency int,
) error {
	if ctx == nil {
		return failure.New(failure.CodeInvalidExecution, "execute concurrent operation: nil context")
	}
	if err := ctx.Err(); err != nil {
		return failure.Wrap(failure.CodeOperationBatchFailed, "execute concurrent operation", err)
	}
	if client == nil {
		return failure.New(failure.CodeInvalidExecution, "execute concurrent operation: nil client")
	}
	if strings.TrimSpace(operation.Name) == "" {
		return failure.New(failure.CodeInvalidExecution, "execute concurrent operation: empty operation name")
	}
	if attempts <= 0 {
		return failure.New(
			failure.CodeInvalidExecution,
			fmt.Sprintf("execute concurrent operation: attempts must be positive: %d", attempts),
		)
	}
	if concurrency <= 0 {
		return failure.New(
			failure.CodeInvalidExecution,
			fmt.Sprintf("execute concurrent operation: concurrency must be positive: %d", concurrency),
		)
	}
	if concurrency > attempts {
		return failure.New(
			failure.CodeInvalidExecution,
			fmt.Sprintf("execute concurrent operation: concurrency %d exceeds attempts %d", concurrency, attempts),
		)
	}
	return nil
}
