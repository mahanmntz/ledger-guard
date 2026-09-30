package main

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// setupTestStore connects to Redis and provides a cleanup function.
func setupTestStore(t *testing.T) (*Store, func()) {
	t.Helper()

	rdb := redis.NewClient(&redis.Options{
		Addr: "localhost:6380",
		DB:   1, // Use isolated DB 1 for integration testing
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Fatalf("Redis connection failed on localhost:6380: %v", err)
	}

	store := NewStore(rdb)

	cleanup := func() {
		// Flush DB 1 after test execution to keep the environment clean
		_ = rdb.FlushDB(context.Background()).Err()
		_ = rdb.Close()
	}

	return store, cleanup
}

func TestStore_LifecycleAndStates(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx := context.Background()
	testKey := fmt.Sprintf("test_idemp_%d", time.Now().UnixNano())
	originalHash := "hash_payload_v1"
	tamperedHash := "hash_payload_v2"
	mockResponse := []byte(`{"status":"CONFIRMED","txn":"txn_abc"}`)

	// 1. Initial State: Key does not exist -> StatusAcquired
	t.Run("Acquire fresh key", func(t *testing.T) {
		status, cached, err := store.Acquire(ctx, testKey, originalHash)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status != StatusAcquired {
			t.Errorf("expected StatusAcquired (%d), got %d", StatusAcquired, status)
		}
		if cached != nil {
			t.Errorf("expected nil cached payload, got %s", string(cached))
		}
	})

	// 2. In-flight Collision: Same key and same hash while PENDING -> StatusPending
	t.Run("Acquire in-flight duplicate", func(t *testing.T) {
		status, _, err := store.Acquire(ctx, testKey, originalHash)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status != StatusPending {
			t.Errorf("expected StatusPending (%d), got %d", StatusPending, status)
		}
	})

	// 3. Payload Tampering: Same key but altered hash -> StatusMismatch
	t.Run("Acquire with tampered payload", func(t *testing.T) {
		status, _, err := store.Acquire(ctx, testKey, tamperedHash)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status != StatusMismatch {
			t.Errorf("expected StatusMismatch (%d), got %d", StatusMismatch, status)
		}
	})

	// 4. Complete: Overwrite key with success response
	t.Run("Complete transaction", func(t *testing.T) {
		err := store.Complete(ctx, testKey, originalHash, mockResponse)
		if err != nil {
			t.Fatalf("store.Complete failed: %v", err)
		}
	})

	// 5. Cache Replay: Acquire on completed key -> StatusCompleted with exact bytes
	t.Run("Acquire completed key returns cached response", func(t *testing.T) {
		status, cached, err := store.Acquire(ctx, testKey, originalHash)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status != StatusCompleted {
			t.Errorf("expected StatusCompleted (%d), got %d", StatusCompleted, status)
		}
		if !bytes.Equal(cached, mockResponse) {
			t.Errorf("expected payload %s, got %s", string(mockResponse), string(cached))
		}
	})

	// 6. Release: Purge key on failure -> Key should be acquirable again
	t.Run("Release allows immediate retry", func(t *testing.T) {
		if err := store.Release(ctx, testKey); err != nil {
			t.Fatalf("store.Release failed: %v", err)
		}

		// Trying to acquire now should succeed as a fresh lock
		status, _, err := store.Acquire(ctx, testKey, originalHash)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if status != StatusAcquired {
			t.Errorf("expected StatusAcquired (%d) after release, got %d", StatusAcquired, status)
		}
	})
}

// BenchmarkStore_AcquireCached measures the throughput of reading an already completed key.
func BenchmarkStore_AcquireCached(b *testing.B) {
	store, cleanup := setupTestStoreBench(b)
	defer cleanup()

	ctx := context.Background()
	key := "bench_key_cached"
	hash := "sample_hash_value"
	payload := []byte(`{"status":"CONFIRMED","txn":"txn_bench_12345"}`)

	// Seed the key into Redis once
	if err := store.Complete(ctx, key, hash, payload); err != nil {
		b.Fatalf("seed failed: %v", err)
	}

	b.ResetTimer() // Exclude setup time from measurements
	b.ReportAllocs() // Measure heap allocations

	for i := 0; i < b.N; i++ {
		_, _, err := store.Acquire(ctx, key, hash)
		if err != nil {
			b.Fatalf("acquire failed: %v", err)
		}
	}
}

// BenchmarkStore_AcquireCached_Parallel simulates heavy concurrent traffic across multiple CPU cores.
func BenchmarkStore_AcquireCached_Parallel(b *testing.B) {
	store, cleanup := setupTestStoreBench(b)
	defer cleanup()

	ctx := context.Background()
	key := "bench_key_parallel"
	hash := "sample_hash_value"
	payload := []byte(`{"status":"CONFIRMED","txn":"txn_bench_parallel"}`)

	if err := store.Complete(ctx, key, hash, payload); err != nil {
		b.Fatalf("seed failed: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _, err := store.Acquire(ctx, key, hash)
			if err != nil {
				b.Fatalf("parallel acquire failed: %v", err)
			}
		}
	})
}

// setupTestStoreBench is a lightweight helper tailored for benchmarks.
func setupTestStoreBench(b *testing.B) (*Store, func()) {
	b.Helper()

	rdb := redis.NewClient(&redis.Options{
		Addr: "localhost:6380",
		DB:   2, // Use isolated DB 2 for benchmarks
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := rdb.Ping(ctx).Err(); err != nil {
		b.Fatalf("Redis connection failed on localhost:6380: %v", err)
	}

	store := NewStore(rdb)

	cleanup := func() {
		_ = rdb.FlushDB(context.Background()).Err()
		_ = rdb.Close()
	}

	return store, cleanup
}