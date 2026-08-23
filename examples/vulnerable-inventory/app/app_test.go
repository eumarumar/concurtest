package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestHandlerDeterministicallyOversellsInventory(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(NewHandler())
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	initial := requestResultFor(t, ctx, server.Client(), http.MethodGet, server.URL+"/state")
	if initial.status != http.StatusOK || initial.contentType != "application/json" || initial.body != `{"stock":1}`+"\n" {
		t.Fatalf(
			"initial state = status %d content type %q body %q, want JSON status 200 and stock 1",
			initial.status,
			initial.contentType,
			initial.body,
		)
	}

	reset := requestResultFor(t, ctx, server.Client(), http.MethodPost, server.URL+"/reset")
	if reset.status != http.StatusNoContent || reset.body != "" {
		t.Fatalf("reset = status %d body %q, want status 204 and empty body", reset.status, reset.body)
	}

	start := make(chan struct{})
	results := make(chan requestResult, purchasesPerRound)
	for range purchasesPerRound {
		go func() {
			<-start
			results <- performRequest(ctx, server.Client(), http.MethodPost, server.URL+"/purchase")
		}()
	}
	close(start)

	purchaseResults := make([]requestResult, 0, purchasesPerRound)
	for range purchasesPerRound {
		purchaseResults = append(purchaseResults, <-results)
	}
	for _, result := range purchaseResults {
		if result.err != nil {
			t.Fatalf("purchase request: %v", result.err)
		}
		if result.status != http.StatusCreated || result.contentType != "application/json" || result.body != purchaseAcceptedResponse {
			t.Errorf(
				"purchase = status %d content type %q body %q, want JSON status 201 and %q",
				result.status,
				result.contentType,
				result.body,
				purchaseAcceptedResponse,
			)
		}
	}

	observed := requestResultFor(t, ctx, server.Client(), http.MethodGet, server.URL+"/state")
	if observed.status != http.StatusOK || observed.body != `{"stock":-1}`+"\n" {
		t.Fatalf("final state = status %d body %q, want status 200 and stock -1", observed.status, observed.body)
	}

	rejected := requestResultFor(t, ctx, server.Client(), http.MethodPost, server.URL+"/purchase")
	if rejected.status != http.StatusConflict || !strings.Contains(rejected.body, purchaseUnavailableReason) {
		t.Errorf("purchase after oversell = status %d body %q, want status 409", rejected.status, rejected.body)
	}
}

func TestResetReleasesPurchaseWaitingInOldRound(t *testing.T) {
	t.Parallel()

	inventory := newInventory()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "/purchase", nil).WithContext(ctx)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		inventory.purchase(response, request)
		close(done)
	}()

	deadline := time.Now().Add(time.Second)
	for {
		inventory.mu.Lock()
		waiting := inventory.round.checked == 1
		inventory.mu.Unlock()
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("purchase did not reach the rendezvous")
		}
		runtime.Gosched()
	}

	resetResponse := httptest.NewRecorder()
	inventory.reset(resetResponse, httptest.NewRequest(http.MethodPost, "/reset", nil))
	if resetResponse.Code != http.StatusNoContent {
		t.Errorf("reset status = %d, want 204", resetResponse.Code)
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		cancel()
		<-done
		t.Fatal("reset did not release the waiting purchase")
	}
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), purchaseResetReason) {
		t.Errorf("waiting purchase = status %d body %q, want reset conflict", response.Code, response.Body.String())
	}

	inventory.mu.Lock()
	stock := inventory.stock
	checked := inventory.round.checked
	inventory.mu.Unlock()
	if stock != initialStock || checked != 0 {
		t.Errorf("state after reset = stock %d checked %d, want stock 1 and no checked purchases", stock, checked)
	}
}

func TestHandlerUsesMethodSpecificRoutes(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(NewHandler())
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tests := []struct {
		method string
		path   string
		want   int
	}{
		{method: http.MethodGet, path: "/reset", want: http.StatusMethodNotAllowed},
		{method: http.MethodGet, path: "/purchase", want: http.StatusMethodNotAllowed},
		{method: http.MethodPost, path: "/state", want: http.StatusMethodNotAllowed},
		{method: http.MethodGet, path: "/missing", want: http.StatusNotFound},
	}

	for _, test := range tests {
		result := requestResultFor(t, ctx, server.Client(), test.method, server.URL+test.path)
		if result.status != test.want {
			t.Errorf("%s %s status = %d, want %d", test.method, test.path, result.status, test.want)
		}
	}
}

type requestResult struct {
	status      int
	contentType string
	body        string
	err         error
}

func requestResultFor(
	t *testing.T,
	ctx context.Context,
	client *http.Client,
	method string,
	url string,
) requestResult {
	t.Helper()
	result := performRequest(ctx, client, method, url)
	if result.err != nil {
		t.Fatalf("%s %s: %v", method, url, result.err)
	}
	return result
}

func performRequest(ctx context.Context, client *http.Client, method string, url string) requestResult {
	request, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return requestResult{err: err}
	}
	response, err := client.Do(request)
	if err != nil {
		return requestResult{err: err}
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	return requestResult{
		status:      response.StatusCode,
		contentType: response.Header.Get("Content-Type"),
		body:        string(body),
		err:         errors.Join(readErr, closeErr),
	}
}
