package engine_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eumarumar/concurtest/internal/engine"
	"github.com/eumarumar/concurtest/internal/failure"
)

func TestExecuteHTTPExposesRetryAfterCommit(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, method, key, body string
		wantCommits             int32
	}{
		{"keyed POST without body", http.MethodPost, "Idempotency-Key", "", 2},
		{"keyed POST with body", http.MethodPost, "Idempotency-Key", `{"quantity":1}`, 2},
		{"alternate key", http.MethodPost, "X-Idempotency-Key", "payload", 2},
		{"GET", http.MethodGet, "", "", 2},
		{"POST without key control", http.MethodPost, "", "payload", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var commits atomic.Int32
			var warmConnection atomic.Value
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					t.Errorf("read request: %v", err)
					return
				}
				if r.URL.Path == "/warm" {
					warmConnection.Store(r.RemoteAddr)
					w.WriteHeader(http.StatusNoContent)
					return
				}
				if test.key != "" && r.Header.Get(test.key) != "test-key" {
					t.Error("idempotency key was changed or removed")
				}
				if commits.Add(1) == 1 {
					if r.RemoteAddr != warmConnection.Load() {
						t.Error("first operation did not reuse the warmed connection")
					}
					// Commit before dropping the response. A replayable request on a
					// reused connection triggers net/http's implicit retry here.
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Errorf("hijack connection: %v", err)
						return
					}
					if err := conn.Close(); err != nil {
						t.Errorf("close connection: %v", err)
					}
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(server.Close)
			client := server.Client()
			client.Timeout = 2 * time.Second
			t.Cleanup(client.CloseIdleConnections)
			warm := engine.ExecuteHTTP(context.Background(), client, engine.HTTPRequest{
				Method: http.MethodPost, URL: server.URL + "/warm",
			})
			if warm.Err != nil {
				t.Fatalf("warm connection: %v", warm.Err)
			}
			header := make(http.Header)
			if test.key != "" {
				header.Set(test.key, "test-key")
			}
			execution := engine.ExecuteHTTP(context.Background(), client, engine.HTTPRequest{
				Method: test.method, URL: server.URL + "/commit", Header: header, Body: []byte(test.body),
			})
			t.Logf("target commits=%d, response=%+v, error=%v", commits.Load(), execution.Response, execution.Err)
			if got := commits.Load(); got != test.wantCommits {
				t.Fatalf("target commits = %d, want %d", got, test.wantCommits)
			}
			if execution.Err == nil {
				t.Error("request returned no error after an implicit retry")
			}
			if execution.RequestWrites != int64(test.wantCommits) {
				t.Errorf("request writes = %d, want %d", execution.RequestWrites, test.wantCommits)
			}
			if test.wantCommits == 2 && failure.CodeOf(execution.Err) != failure.CodeRequestRetried {
				t.Errorf("error code = %q, want request_retried", failure.CodeOf(execution.Err))
			}
			if test.wantCommits == 2 && (execution.Response == nil || execution.Response.StatusCode != http.StatusOK) {
				t.Fatalf("retried response = %+v, want retained HTTP 200", execution.Response)
			}
		})
	}
}

func TestExecuteHTTPTracesHTTPVersionsAndPreservesCallerTrace(t *testing.T) {
	t.Parallel()
	for _, http2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("http2=%t", http2), func(t *testing.T) {
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wantVersion := 1
				if http2 {
					wantVersion = 2
				}
				if r.ProtoMajor != wantVersion {
					t.Errorf("HTTP version = %d, want %d", r.ProtoMajor, wantVersion)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			server.EnableHTTP2 = http2
			server.StartTLS()
			t.Cleanup(server.Close)
			var callerWrites atomic.Int32
			ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
				WroteHeaders: func() { callerWrites.Add(1) },
			})
			execution := engine.ExecuteHTTP(ctx, server.Client(), engine.HTTPRequest{
				Method: http.MethodGet, URL: server.URL,
			})
			if execution.Err != nil || execution.RequestWrites != 1 || callerWrites.Load() != 1 {
				t.Fatalf("error=%v, writes=%d, caller writes=%d", execution.Err, execution.RequestWrites, callerWrites.Load())
			}
		})
	}
}

func TestExecuteHTTPDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var destinationCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/login" {
					destinationCalls.Add(1)
					if _, err := io.WriteString(w, "login page"); err != nil {
						t.Errorf("write destination: %v", err)
					}
					return
				}
				w.Header().Set("Location", "/login")
				w.WriteHeader(status)
				if _, err := io.WriteString(w, "original response"); err != nil {
					t.Errorf("write redirect: %v", err)
				}
			}))
			t.Cleanup(server.Close)
			client := server.Client()
			execution := engine.ExecuteHTTP(context.Background(), client, engine.HTTPRequest{
				Method: http.MethodPost, URL: server.URL + "/operation", Body: []byte("payload"),
			})
			t.Logf("destination calls=%d, response=%+v", destinationCalls.Load(), execution.Response)
			if execution.Err != nil {
				t.Fatalf("ExecuteHTTP: %v", execution.Err)
			}
			if execution.Response == nil || execution.Response.StatusCode != status || string(execution.Response.Body) != "original response" {
				t.Errorf("response = %+v, want original HTTP %d and body", execution.Response, status)
			}
			if destinationCalls.Load() != 0 {
				t.Errorf("redirect destination received %d requests, want 0", destinationCalls.Load())
			}
			if client.CheckRedirect != nil {
				t.Error("ExecuteHTTP mutated the caller's redirect policy")
			}
		})
	}
}

func TestExecuteHTTPPreservesErrorsAlongsideRetryEvidence(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"send", "close"} {
		t.Run(stage, func(t *testing.T) {
			wantErr := errors.New("response close failed")
			if stage == "send" {
				wantErr = context.DeadlineExceeded
			}
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				// Simulate a tracing transport that retries, then fails. Real
				// connection retries are covered by the close-after-commit test.
				trace := httptrace.ContextClientTrace(r.Context())
				trace.WroteHeaders()
				trace.WroteHeaders()
				if stage == "send" {
					return nil, wantErr
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
					Body: &errorOnCloseBody{Reader: strings.NewReader("retained"), err: wantErr}}, nil
			})}
			execution := engine.ExecuteHTTP(context.Background(), client, engine.HTTPRequest{
				Method: http.MethodGet, URL: "http://example.test/state",
			})
			if execution.RequestWrites != 2 || failure.CodeOf(execution.Err) != failure.CodeRequestRetried || !errors.Is(execution.Err, wantErr) {
				t.Fatalf("writes=%d, error=%v, want retry evidence and wrapped %v", execution.RequestWrites, execution.Err, wantErr)
			}
			if stage == "close" && (execution.Response == nil || string(execution.Response.Body) != "retained") {
				t.Fatalf("lost response on close failure: %+v", execution.Response)
			}
		})
	}
}
