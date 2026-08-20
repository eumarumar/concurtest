package engine_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eumarumar/concurtest/internal/engine"
)

func TestExecuteHTTPRecordsRequestAndResponse(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			return
		}
		if request.Method != http.MethodPost {
			t.Errorf("method = %q, want %q", request.Method, http.MethodPost)
		}
		if request.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q, want %q", request.Header.Get("Content-Type"), "application/json")
		}
		if string(body) != `{"quantity":1}` {
			t.Errorf("body = %q, want %q", body, `{"quantity":1}`)
		}

		writer.Header().Set("X-Request-ID", "request-1")
		writer.WriteHeader(http.StatusCreated)
		if _, err := io.WriteString(writer, `{"accepted":true}`); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	request := engine.HTTPRequest{
		Method: http.MethodPost,
		URL:    server.URL + "/purchase",
		Header: http.Header{"Content-Type": {"application/json"}},
		Body:   []byte(`{"quantity":1}`),
	}
	execution := engine.ExecuteHTTP(context.Background(), server.Client(), request)

	if execution.Err != nil {
		t.Fatalf("ExecuteHTTP() error = %v", execution.Err)
	}
	if execution.Response == nil {
		t.Fatal("ExecuteHTTP() response is nil")
	}
	if execution.Response.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want %d", execution.Response.StatusCode, http.StatusCreated)
	}
	if execution.Response.Header.Get("X-Request-ID") != "request-1" {
		t.Errorf("X-Request-ID = %q, want %q", execution.Response.Header.Get("X-Request-ID"), "request-1")
	}
	if string(execution.Response.Body) != `{"accepted":true}` {
		t.Errorf("response body = %q, want %q", execution.Response.Body, `{"accepted":true}`)
	}
	if execution.Response.BodyTruncated {
		t.Error("BodyTruncated = true, want false")
	}
	if execution.StartedAt.IsZero() || execution.CompletedAt.IsZero() {
		t.Error("execution timestamps must be populated")
	}
	if execution.CompletedAt.Before(execution.StartedAt) {
		t.Errorf("execution completed at %v before it started at %v", execution.CompletedAt, execution.StartedAt)
	}
	if execution.Duration() < 0 {
		t.Errorf("duration = %v, want a non-negative duration", execution.Duration())
	}

	request.Header.Set("Content-Type", "text/plain")
	request.Body[0] = 'x'
	if execution.Request.Header.Get("Content-Type") != "application/json" {
		t.Error("recorded request header changed when caller mutated its input")
	}
	if string(execution.Request.Body) != `{"quantity":1}` {
		t.Error("recorded request body changed when caller mutated its input")
	}
}

func TestExecuteHTTPRecordsNonSuccessfulStatusWithoutError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "out of stock", http.StatusConflict)
	}))
	t.Cleanup(server.Close)

	execution := engine.ExecuteHTTP(context.Background(), server.Client(), engine.HTTPRequest{
		Method: http.MethodPost,
		URL:    server.URL + "/purchase",
	})

	if execution.Err != nil {
		t.Fatalf("ExecuteHTTP() error = %v", execution.Err)
	}
	if execution.Response == nil {
		t.Fatal("ExecuteHTTP() response is nil")
	}
	if execution.Response.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want %d", execution.Response.StatusCode, http.StatusConflict)
	}
}

func TestExecuteHTTPHonorsCancellation(t *testing.T) {
	t.Parallel()

	requestStarted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(requestStarted)
		<-request.Context().Done()
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan engine.HTTPExecution, 1)
	go func() {
		result <- engine.ExecuteHTTP(ctx, server.Client(), engine.HTTPRequest{
			Method: http.MethodGet,
			URL:    server.URL,
		})
	}()

	select {
	case <-requestStarted:
		cancel()
	case <-time.After(time.Second):
		cancel()
		t.Fatal("server did not receive request")
	}

	select {
	case execution := <-result:
		if !errors.Is(execution.Err, context.Canceled) {
			t.Fatalf("ExecuteHTTP() error = %v, want context.Canceled", execution.Err)
		}
	case <-time.After(time.Second):
		t.Fatal("ExecuteHTTP() did not return after cancellation")
	}
}

func TestExecuteHTTPRecordsTransportError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("network unavailable")
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, wantErr
	})}

	execution := engine.ExecuteHTTP(context.Background(), client, engine.HTTPRequest{
		Method: http.MethodGet,
		URL:    "http://example.test/state",
	})

	if !errors.Is(execution.Err, wantErr) {
		t.Fatalf("ExecuteHTTP() error = %v, want wrapped %v", execution.Err, wantErr)
	}
	if execution.Response != nil {
		t.Errorf("ExecuteHTTP() response = %#v, want nil", execution.Response)
	}
}

func TestExecuteHTTPBoundsCapturedResponseBody(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if _, err := io.WriteString(writer, strings.Repeat("x", engine.MaxHTTPBodyBytes+1)); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	execution := engine.ExecuteHTTP(context.Background(), server.Client(), engine.HTTPRequest{
		Method: http.MethodGet,
		URL:    server.URL,
	})

	if execution.Err != nil {
		t.Fatalf("ExecuteHTTP() error = %v", execution.Err)
	}
	if execution.Response == nil {
		t.Fatal("ExecuteHTTP() response is nil")
	}
	if len(execution.Response.Body) != engine.MaxHTTPBodyBytes {
		t.Errorf("captured body length = %d, want %d", len(execution.Response.Body), engine.MaxHTTPBodyBytes)
	}
	if !execution.Response.BodyTruncated {
		t.Error("BodyTruncated = false, want true")
	}
}

func TestExecuteHTTPReportsResponseCloseError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("close failed")
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       &errorOnCloseBody{Reader: strings.NewReader("response"), err: wantErr},
		}, nil
	})}

	execution := engine.ExecuteHTTP(context.Background(), client, engine.HTTPRequest{
		Method: http.MethodGet,
		URL:    "http://example.test/state",
	})

	if !errors.Is(execution.Err, wantErr) {
		t.Fatalf("ExecuteHTTP() error = %v, want wrapped %v", execution.Err, wantErr)
	}
	if execution.Response == nil || string(execution.Response.Body) != "response" {
		t.Fatalf("ExecuteHTTP() response = %#v, want captured response body", execution.Response)
	}
}

func TestExecuteHTTPRejectsNilDependencies(t *testing.T) {
	t.Parallel()

	request := engine.HTTPRequest{Method: http.MethodGet, URL: "http://example.test"}

	if execution := engine.ExecuteHTTP(nil, http.DefaultClient, request); execution.Err == nil {
		t.Error("ExecuteHTTP() with nil context returned nil error")
	}
	if execution := engine.ExecuteHTTP(context.Background(), nil, request); execution.Err == nil {
		t.Error("ExecuteHTTP() with nil client returned nil error")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type errorOnCloseBody struct {
	io.Reader
	err error
}

func (body *errorOnCloseBody) Close() error {
	return body.err
}
