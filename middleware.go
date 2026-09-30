package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"net/http"
)

// responseRecorder intercepts the HTTP status and body written by downstream handlers.
type responseRecorder struct {
	http.ResponseWriter
	statusCode int
	body       *bytes.Buffer
}

func newResponseRecorder(w http.ResponseWriter) *responseRecorder {
	return &responseRecorder{
		ResponseWriter: w,
		statusCode:     http.StatusOK, // Default status if WriteHeader is not explicitly called
		body:           &bytes.Buffer{},
	}
}

func (rec *responseRecorder) WriteHeader(code int) {
	rec.statusCode = code
	rec.ResponseWriter.WriteHeader(code)
}

func (rec *responseRecorder) Write(b []byte) (int, error) {
	rec.body.Write(b) // Save a copy for Redis caching
	return rec.ResponseWriter.Write(b) // Forward directly to the client
}

// IdempotencyMiddleware wraps any standard http.Handler with end-to-end idempotency protection.
func IdempotencyMiddleware(store *Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Idempotency applies strictly to state-mutating requests
			if r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodPatch {
				next.ServeHTTP(w, r)
				return
			}

			idempKey := r.Header.Get("X-Idempotency-Key")
			if idempKey == "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"error": "missing required header X-Idempotency-Key"}`))
				return
			}

			// Read body to compute hash
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil || len(bodyBytes) == 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"error": "empty or unreadable request body"}`))
				return
			}
			r.Body.Close()

			// Restore body stream so the downstream business handler can read it again
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

			// Calculate SHA-256 hash
			hasher := sha256.New()
			hasher.Write(bodyBytes)
			reqHash := hex.EncodeToString(hasher.Sum(nil))

			ctx := r.Context()

			// 1. Acquire Step
			status, cachedResponse, err := store.Acquire(ctx, idempKey, reqHash)
			if err != nil {
				log.Printf("[store.Acquire] key=%s err=%v", idempKey, err)
				http.Error(w, `{"error": "internal state error"}`, http.StatusInternalServerError)
				return
			}

			switch status {
			case StatusMismatch:
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnprocessableEntity)
				w.Write([]byte(`{"error": "idempotency key reused with different request payload"}`))
				return

			case StatusPending:
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				w.Write([]byte(`{"error": "request currently in flight", "status": "PENDING"}`))
				return

			case StatusCompleted:
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Cache", "HIT")
				w.WriteHeader(http.StatusOK)
				w.Write(cachedResponse)
				return

			case StatusAcquired:
				// Lock secured; proceed to actual payment handler
			}

			// Wrap writer to record the downstream handler's response
			recorder := newResponseRecorder(w)
			recorder.Header().Set("X-Cache", "MISS")

			next.ServeHTTP(recorder, r)

			// 2. Lifecycle completion decision
			if recorder.statusCode >= 200 && recorder.statusCode < 300 {
				// Success: Cache the captured JSON payload
				if err := store.Complete(ctx, idempKey, reqHash, recorder.body.Bytes()); err != nil {
					log.Printf("[store.Complete] key=%s err=%v", idempKey, err)
				}
			} else {
				// Downstream failed: Release the lock immediately to allow safe retry
				if err := store.Release(ctx, idempKey); err != nil {
					log.Printf("[store.Release] key=%s err=%v", idempKey, err)
				}
			}
		})
	}
}