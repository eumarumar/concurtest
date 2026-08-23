// Package app provides the deliberately vulnerable inventory service used by
// the ConcurTest v0 demonstration.
package app

import (
	"fmt"
	"io"
	"net/http"
	"sync"
)

const (
	initialStock              = 1
	purchasesPerRound         = 2
	purchaseAcceptedResponse  = `{"accepted":true}` + "\n"
	purchaseUnavailableReason = "purchase rejected: stock is unavailable"
	purchaseRoundFullReason   = "purchase rejected: two purchases are already in progress"
	purchaseResetReason       = "purchase rejected: inventory was reset"
)

// NewHandler returns an isolated deliberately vulnerable inventory service.
func NewHandler() http.Handler {
	return newInventory().handler()
}

type inventory struct {
	mu    sync.Mutex
	stock int
	round *purchaseRound
}

type purchaseRound struct {
	checked int
	// ready is closed by the second purchase after both requests have checked
	// stock, but before either request is allowed to decrement it.
	ready chan struct{}
	// canceled is closed by reset before this round is replaced.
	canceled chan struct{}
}

func newInventory() *inventory {
	return &inventory{
		stock: initialStock,
		round: newPurchaseRound(),
	}
}

func newPurchaseRound() *purchaseRound {
	return &purchaseRound{
		ready:    make(chan struct{}),
		canceled: make(chan struct{}),
	}
}

func (inventory *inventory) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /reset", inventory.reset)
	mux.HandleFunc("POST /purchase", inventory.purchase)
	mux.HandleFunc("GET /state", inventory.observe)
	return mux
}

func (inventory *inventory) reset(writer http.ResponseWriter, _ *http.Request) {
	inventory.mu.Lock()
	close(inventory.round.canceled)
	inventory.stock = initialStock
	inventory.round = newPurchaseRound()
	inventory.mu.Unlock()

	writer.WriteHeader(http.StatusNoContent)
}

func (inventory *inventory) purchase(writer http.ResponseWriter, request *http.Request) {
	inventory.mu.Lock()
	if inventory.stock <= 0 {
		inventory.mu.Unlock()
		http.Error(writer, purchaseUnavailableReason, http.StatusConflict)
		return
	}

	round := inventory.round
	if round.checked >= purchasesPerRound {
		inventory.mu.Unlock()
		http.Error(writer, purchaseRoundFullReason, http.StatusConflict)
		return
	}
	round.checked++
	if round.checked == purchasesPerRound {
		close(round.ready)
	}
	inventory.mu.Unlock()

	// The check above and the decrement below are intentionally separate
	// critical sections. That business-level bug is what this example exists to
	// let ConcurTest discover.
	select {
	case <-round.ready:
	case <-round.canceled:
		http.Error(writer, purchaseResetReason, http.StatusConflict)
		return
	case <-request.Context().Done():
		return
	}
	if request.Context().Err() != nil {
		return
	}

	inventory.mu.Lock()
	if inventory.round != round {
		inventory.mu.Unlock()
		http.Error(writer, purchaseResetReason, http.StatusConflict)
		return
	}
	inventory.stock--
	inventory.mu.Unlock()

	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusCreated)
	if _, err := io.WriteString(writer, purchaseAcceptedResponse); err != nil {
		return
	}
}

func (inventory *inventory) observe(writer http.ResponseWriter, _ *http.Request) {
	inventory.mu.Lock()
	stock := inventory.stock
	inventory.mu.Unlock()

	writer.Header().Set("Content-Type", "application/json")
	if _, err := fmt.Fprintf(writer, `{"stock":%d}`+"\n", stock); err != nil {
		return
	}
}
