package engine

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"time"

	"github.com/eumarumar/concurtest/internal/failure"
)

// MaxHTTPBodyBytes is the largest response body ExecuteHTTP retains in memory.
const MaxHTTPBodyBytes = 1 << 20

// HTTPRequest describes one request to a target system.
type HTTPRequest struct {
	Method string
	URL    string
	Header http.Header
	Body   []byte
}

// HTTPResponse records the externally visible result of an HTTP request.
type HTTPResponse struct {
	StatusCode    int
	Header        http.Header
	Body          []byte
	BodyTruncated bool
}

// HTTPExecution records one request attempt and its timing information.
// Err is nil when the request and response were exchanged successfully,
// regardless of the HTTP status code.
type HTTPExecution struct {
	Request     HTTPRequest
	Response    *HTTPResponse
	StartedAt   time.Time
	CompletedAt time.Time
	Err         error
}

// Duration reports the elapsed time of the execution.
func (e HTTPExecution) Duration() time.Duration {
	return e.CompletedAt.Sub(e.StartedAt)
}

// ExecuteHTTP performs one HTTP request and records its observable result.
// The caller owns client and should reuse it across executions.
func ExecuteHTTP(ctx context.Context, client *http.Client, request HTTPRequest) (execution HTTPExecution) {
	execution.Request = cloneHTTPRequest(request)
	execution.StartedAt = time.Now()
	defer func() {
		execution.CompletedAt = time.Now()
	}()

	if ctx == nil {
		execution.Err = failure.New(failure.CodeInvalidExecution, "execute HTTP request: nil context")
		return execution
	}
	if client == nil {
		execution.Err = failure.New(failure.CodeInvalidExecution, "execute HTTP request: nil client")
		return execution
	}

	httpRequest, err := http.NewRequestWithContext(
		ctx,
		execution.Request.Method,
		execution.Request.URL,
		bytes.NewReader(execution.Request.Body),
	)
	if err != nil {
		execution.Err = failure.Wrap(failure.CodeRequestCreateFailed, "create HTTP request", err)
		return execution
	}
	httpRequest.Header = execution.Request.Header.Clone()

	httpResponse, err := client.Do(httpRequest)
	if err != nil {
		if httpResponse != nil {
			execution.Response = responseMetadata(httpResponse)
		}
		execution.Err = failure.Wrap(failure.CodeRequestSendFailed, "send HTTP request", err)
		return execution
	}

	body, readErr := io.ReadAll(io.LimitReader(httpResponse.Body, MaxHTTPBodyBytes+1))
	closeErr := httpResponse.Body.Close()

	response := responseMetadata(httpResponse)
	if len(body) > MaxHTTPBodyBytes {
		body = body[:MaxHTTPBodyBytes]
		response.BodyTruncated = true
	}
	response.Body = body
	execution.Response = response

	if readErr != nil || closeErr != nil {
		execution.Err = failure.Join(
			failure.CodeResponseFailed,
			"read HTTP response",
			failure.Wrap(failure.CodeResponseReadFailed, "read HTTP response body", readErr),
			failure.Wrap(failure.CodeResponseCloseFailed, "close HTTP response body", closeErr),
		)
	}

	return execution
}

func cloneHTTPRequest(request HTTPRequest) HTTPRequest {
	return HTTPRequest{
		Method: request.Method,
		URL:    request.URL,
		Header: request.Header.Clone(),
		Body:   bytes.Clone(request.Body),
	}
}

func responseMetadata(response *http.Response) *HTTPResponse {
	return &HTTPResponse{
		StatusCode: response.StatusCode,
		Header:     response.Header.Clone(),
	}
}
