package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
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
		execution.Err = errors.New("execute HTTP request: nil context")
		return execution
	}
	if client == nil {
		execution.Err = errors.New("execute HTTP request: nil client")
		return execution
	}

	httpRequest, err := http.NewRequestWithContext(
		ctx,
		execution.Request.Method,
		execution.Request.URL,
		bytes.NewReader(execution.Request.Body),
	)
	if err != nil {
		execution.Err = fmt.Errorf("create HTTP request: %w", err)
		return execution
	}
	httpRequest.Header = execution.Request.Header.Clone()

	httpResponse, err := client.Do(httpRequest)
	if err != nil {
		if httpResponse != nil {
			execution.Response = responseMetadata(httpResponse)
		}
		execution.Err = fmt.Errorf("send HTTP request: %w", err)
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
		execution.Err = errors.Join(
			wrapError("read HTTP response body", readErr),
			wrapError("close HTTP response body", closeErr),
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

func wrapError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", action, err)
}
