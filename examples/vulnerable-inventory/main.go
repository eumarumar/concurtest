package main

import (
	"errors"
	"log"
	"net/http"
	"time"

	inventoryapp "github.com/eumarumar/concurtest/examples/vulnerable-inventory/app"
)

const listenAddress = "127.0.0.1:8080"

func main() {
	server := &http.Server{
		Addr:              listenAddress,
		Handler:           inventoryapp.NewHandler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("Deliberately vulnerable inventory is ready at http://%s", listenAddress)
	log.Print("Use it only for the local ConcurTest demonstration.")
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("Inventory example stopped: %v", err)
	}
}
