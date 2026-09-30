package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/redis/go-redis/v9"
)

type PaymentRequest struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
	Account  string `json:"account_id"`
}

type PaymentResponse struct {
	Status        string    `json:"status"`
	TransactionID string    `json:"transaction_id"`
	Amount        int64     `json:"amount"`
	Currency      string    `json:"currency"`
	ProcessedAt   time.Time `json:"processed_at"`
}

func main() {
	redisAddr := getEnv("REDIS_ADDR", "localhost:6380")
	serverPort := getEnv("PORT", "8080")

	rdb := redis.NewClient(&redis.Options{
		Addr: redisAddr,
		DB:   0,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("Failed to connect to Redis on %s: %v", redisAddr, err)
	}
	log.Printf("Connected to Redis successfully on %s.\n", redisAddr)

	store := NewStore(rdb)

	protectWithIdempotency := IdempotencyMiddleware(store)
	http.Handle("/pay", protectWithIdempotency(http.HandlerFunc(handlePay)))

	addr := ":" + serverPort
	log.Printf("Server running on http://localhost%s\n", addr)
	if err := http.ListenAndServe(addr, nil); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("Server error: %v", err)
	}
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func handlePay(w http.ResponseWriter, r *http.Request) {
	var req PaymentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error": "invalid request body"}`))
		return
	}

	time.Sleep(2 * time.Second)

	resp := PaymentResponse{
		Status:        "CONFIRMED",
		TransactionID: fmt.Sprintf("txn_%d", time.Now().UnixNano()),
		Amount:        req.Amount,
		Currency:      req.Currency,
		ProcessedAt:   time.Now().UTC(),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}