package engine_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eumarumar/concurtest/internal/engine"
)

func TestExecuteConcurrentBoundsConcurrencyAndRecordsHistory(t *testing.T) {
	t.Parallel()

	const (
		attempts    = 6
		concurrency = 3
	)

	var active atomic.Int32
	var maximum atomic.Int32
	firstBatchReady := make(chan struct{})
	var releaseFirstBatch sync.Once

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		updateMaximum(&maximum, current)
		if current == concurrency {
			releaseFirstBatch.Do(func() { close(firstBatchReady) })
		}

		select {
		case <-firstBatchReady:
			writer.WriteHeader(http.StatusCreated)
		case <-request.Context().Done():
		}
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	history, err := engine.ExecuteConcurrent(ctx, server.Client(), engine.Operation{
		Name: "purchase",
		Request: engine.HTTPRequest{
			Method: http.MethodPost,
			URL:    server.URL + "/purchase",
		},
		Attempts: attempts,
	}, concurrency)
	if err != nil {
		t.Fatalf("ExecuteConcurrent() error = %v", err)
	}

	if got := maximum.Load(); got != concurrency {
		t.Errorf("maximum concurrency = %d, want %d", got, concurrency)
	}
	if len(history.Attempts) != attempts {
		t.Fatalf("history attempt count = %d, want %d", len(history.Attempts), attempts)
	}
	if history.StartedAt.IsZero() || history.CompletedAt.IsZero() {
		t.Error("history timestamps must be populated")
	}
	if history.CompletedAt.Before(history.StartedAt) {
		t.Errorf("history completed at %v before it started at %v", history.CompletedAt, history.StartedAt)
	}
	if history.Duration() < 0 {
		t.Errorf("history duration = %v, want a non-negative duration", history.Duration())
	}

	for index, attempt := range history.Attempts {
		wantID := index + 1
		if attempt.ID != wantID {
			t.Errorf("attempt[%d].ID = %d, want %d", index, attempt.ID, wantID)
		}
		if attempt.OperationName != "purchase" {
			t.Errorf("attempt[%d].OperationName = %q, want %q", index, attempt.OperationName, "purchase")
		}
		if attempt.Execution == nil {
			t.Errorf("attempt[%d].Execution is nil", index)
			continue
		}
		if attempt.Execution.Err != nil {
			t.Errorf("attempt[%d] error = %v", index, attempt.Execution.Err)
		}
		if attempt.Execution.Response == nil || attempt.Execution.Response.StatusCode != http.StatusCreated {
			t.Errorf("attempt[%d] response = %#v, want status %d", index, attempt.Execution.Response, http.StatusCreated)
		}
		if attempt.Execution.StartedAt.Before(history.StartedAt) {
			t.Errorf("attempt[%d] started before history", index)
		}
		if attempt.Execution.CompletedAt.After(history.CompletedAt) {
			t.Errorf("attempt[%d] completed after history", index)
		}
	}
}

func TestExecuteConcurrentKeepsIndividualFailuresInHistory(t *testing.T) {
	t.Parallel()

	transportErr := errors.New("network unavailable")
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		switch calls.Add(1) {
		case 1:
			return testHTTPResponse(http.StatusConflict, "out of stock"), nil
		case 2:
			return nil, transportErr
		default:
			return testHTTPResponse(http.StatusCreated, "purchased"), nil
		}
	})}

	history, err := engine.ExecuteConcurrent(context.Background(), client, engine.Operation{
		Name:     "purchase",
		Request:  engine.HTTPRequest{Method: http.MethodPost, URL: "http://example.test/purchase"},
		Attempts: 3,
	}, 3)
	if err != nil {
		t.Fatalf("ExecuteConcurrent() error = %v", err)
	}

	var conflicts, created, transportErrors int
	for _, attempt := range history.Attempts {
		if attempt.Execution == nil {
			t.Fatal("completed run contains an unstarted attempt")
		}
		if errors.Is(attempt.Execution.Err, transportErr) {
			transportErrors++
			continue
		}
		if attempt.Execution.Err != nil {
			t.Fatalf("unexpected attempt error: %v", attempt.Execution.Err)
		}
		switch attempt.Execution.Response.StatusCode {
		case http.StatusConflict:
			conflicts++
		case http.StatusCreated:
			created++
		}
	}

	if conflicts != 1 || created != 1 || transportErrors != 1 {
		t.Errorf(
			"outcomes = conflicts:%d created:%d transport-errors:%d, want 1 each",
			conflicts,
			created,
			transportErrors,
		)
	}
}

func TestExecuteConcurrentCancellationReturnsPartialHistory(t *testing.T) {
	t.Parallel()

	const concurrency = 2
	var active atomic.Int32
	requestsStarted := make(chan struct{})
	var signalStarted sync.Once

	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		if active.Add(1) == concurrency {
			signalStarted.Do(func() { close(requestsStarted) })
		}
		defer active.Add(-1)
		<-request.Context().Done()
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithCancel(context.Background())
	type runResult struct {
		history engine.History
		err     error
	}
	result := make(chan runResult, 1)
	go func() {
		history, err := engine.ExecuteConcurrent(ctx, server.Client(), engine.Operation{
			Name:     "purchase",
			Request:  engine.HTTPRequest{Method: http.MethodPost, URL: server.URL + "/purchase"},
			Attempts: 5,
		}, concurrency)
		result <- runResult{history: history, err: err}
	}()

	select {
	case <-requestsStarted:
		cancel()
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("concurrent requests did not start")
	}

	var run runResult
	select {
	case run = <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("ExecuteConcurrent() did not return after cancellation")
	}

	if !errors.Is(run.err, context.Canceled) {
		t.Fatalf("ExecuteConcurrent() error = %v, want context.Canceled", run.err)
	}
	if len(run.history.Attempts) != 5 {
		t.Fatalf("history attempt count = %d, want 5", len(run.history.Attempts))
	}

	started := 0
	for _, attempt := range run.history.Attempts {
		if attempt.Execution == nil {
			continue
		}
		started++
		if !errors.Is(attempt.Execution.Err, context.Canceled) {
			t.Errorf("attempt %d error = %v, want context.Canceled", attempt.ID, attempt.Execution.Err)
		}
	}
	if started != concurrency {
		t.Errorf("started attempts = %d, want %d", started, concurrency)
	}
}

func TestExecuteConcurrentSnapshotsRequestBeforeRunningWorkers(t *testing.T) {
	t.Parallel()

	firstRequestStarted := make(chan struct{})
	releaseFirstRequest := make(chan struct{})
	var requestNumber atomic.Int32
	var mu sync.Mutex
	var bodies []string
	var contentTypes []string

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			return
		}
		mu.Lock()
		bodies = append(bodies, string(body))
		contentTypes = append(contentTypes, request.Header.Get("Content-Type"))
		mu.Unlock()

		if requestNumber.Add(1) == 1 {
			close(firstRequestStarted)
			select {
			case <-releaseFirstRequest:
			case <-request.Context().Done():
				return
			}
		}
		writer.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(server.Close)

	operation := engine.Operation{
		Name: "purchase",
		Request: engine.HTTPRequest{
			Method: http.MethodPost,
			URL:    server.URL + "/purchase",
			Header: http.Header{"Content-Type": {"application/json"}},
			Body:   []byte(`{"quantity":1}`),
		},
		Attempts: 2,
	}

	type runResult struct {
		history engine.History
		err     error
	}
	result := make(chan runResult, 1)
	go func() {
		history, err := engine.ExecuteConcurrent(context.Background(), server.Client(), operation, 1)
		result <- runResult{history: history, err: err}
	}()

	select {
	case <-firstRequestStarted:
		operation.Request.Header.Set("Content-Type", "text/plain")
		operation.Request.Body[0] = 'x'
		close(releaseFirstRequest)
	case <-time.After(5 * time.Second):
		close(releaseFirstRequest)
		t.Fatal("first request did not start")
	}

	run := <-result
	if run.err != nil {
		t.Fatalf("ExecuteConcurrent() error = %v", run.err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 || len(contentTypes) != 2 {
		t.Fatalf("captured requests = %d bodies and %d content types, want 2 each", len(bodies), len(contentTypes))
	}
	for index := range bodies {
		if bodies[index] != `{"quantity":1}` {
			t.Errorf("request[%d] body = %q, want original body", index, bodies[index])
		}
		if contentTypes[index] != "application/json" {
			t.Errorf("request[%d] Content-Type = %q, want application/json", index, contentTypes[index])
		}
	}
}

func TestExecuteConcurrentRejectsInvalidInputWithoutStartingWork(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return testHTTPResponse(http.StatusOK, "ok"), nil
	})}
	validOperation := engine.Operation{
		Name:     "purchase",
		Request:  engine.HTTPRequest{Method: http.MethodPost, URL: "http://example.test/purchase"},
		Attempts: 2,
	}
	cancelledContext, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name        string
		ctx         context.Context
		client      *http.Client
		operation   engine.Operation
		concurrency int
	}{
		{name: "nil context", ctx: nil, client: client, operation: validOperation, concurrency: 1},
		{name: "cancelled context", ctx: cancelledContext, client: client, operation: validOperation, concurrency: 1},
		{name: "nil client", ctx: context.Background(), client: nil, operation: validOperation, concurrency: 1},
		{name: "empty operation name", ctx: context.Background(), client: client, operation: engine.Operation{Attempts: 2}, concurrency: 1},
		{name: "zero attempts", ctx: context.Background(), client: client, operation: engine.Operation{Name: "purchase"}, concurrency: 1},
		{name: "zero concurrency", ctx: context.Background(), client: client, operation: validOperation, concurrency: 0},
		{name: "concurrency exceeds attempts", ctx: context.Background(), client: client, operation: validOperation, concurrency: 3},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := calls.Load()
			history, err := engine.ExecuteConcurrent(test.ctx, test.client, test.operation, test.concurrency)
			if err == nil {
				t.Fatal("ExecuteConcurrent() error = nil, want validation error")
			}
			if len(history.Attempts) != 0 {
				t.Errorf("history attempts = %d, want 0", len(history.Attempts))
			}
			if after := calls.Load(); after != before {
				t.Errorf("transport calls changed from %d to %d", before, after)
			}
		})
	}
}

func updateMaximum(maximum *atomic.Int32, candidate int32) {
	for {
		current := maximum.Load()
		if candidate <= current || maximum.CompareAndSwap(current, candidate) {
			return
		}
	}
}

func testHTTPResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
