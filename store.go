package main

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"
)

type State int

const (
	StatusAcquired  State = iota // 0
	StatusPending                // 1
	StatusCompleted              // 2
	StatusMismatch               // 3
)

const (
	lockTTL      = 15 * time.Second
	retentionTTL = 24 * time.Hour
)

type Record struct {
	Status      string `json:"status"`
	RequestHash string `json:"request_hash"`
	Response    []byte `json:"response,omitempty"`
}

type Store struct {
	client     *redis.Client
	acquireLua *redis.Script
}

// Lua script executed atomically inside Redis engine in 1 Network Round-Trip
var acquireScript = redis.NewScript(`
	local key = KEYS[1]
	local incoming_hash = ARGV[1]
	local pending_payload = ARGV[2]
	local ttl_seconds = tonumber(ARGV[3])

	-- 1. Try to set if not exists
	local set_result = redis.call("SET", key, pending_payload, "NX", "EX", ttl_seconds)
	if set_result then
		return {0, ""} -- StatusAcquired (0)
	end

	-- 2. If exists, read current record
	local current_val = redis.call("GET", key)
	if not current_val then
		return {1, ""} -- Key expired in between, return StatusPending (1)
	end

	return {99, current_val} -- Return raw value for Go to parse hash & status
`)

func NewStore(client *redis.Client) *Store {
	return &Store{
		client:     client,
		acquireLua: acquireScript,
	}
}

func (s *Store) Acquire(ctx context.Context, key string, reqHash string) (State, []byte, error) {
	initialRecord := Record{
		Status:      "PENDING",
		RequestHash: reqHash,
	}
	recordBytes, err := json.Marshal(initialRecord)
	if err != nil {
		return 0, nil, err
	}

	// Execute Lua script: 1 Network Round-Trip
	res, err := s.acquireLua.Run(ctx, s.client, []string{key}, reqHash, recordBytes, int(lockTTL.Seconds())).Slice()
	if err != nil {
		return 0, nil, err
	}

	statusCode := res[0].(int64)

	// Fresh lock acquired by Lua
	if statusCode == 0 {
		return StatusAcquired, nil, nil
	}

	// Parse existing record returned by Lua
	rawBytes := []byte(res[1].(string))
	var existing Record
	if err := json.Unmarshal(rawBytes, &existing); err != nil {
		return 0, nil, err
	}

	if existing.RequestHash != reqHash {
		return StatusMismatch, nil, nil
	}

	if existing.Status == "PENDING" {
		return StatusPending, nil, nil
	}

	return StatusCompleted, existing.Response, nil
}

func (s *Store) Complete(ctx context.Context, key string, reqHash string, payload []byte) error {
	record := Record{
		Status:      "COMPLETED",
		RequestHash: reqHash,
		Response:    payload,
	}

	recordBytes, err := json.Marshal(record)
	if err != nil {
		return err
	}

	return s.client.Set(ctx, key, recordBytes, retentionTTL).Err()
}

func (s *Store) Release(ctx context.Context, key string) error {
	return s.client.Del(ctx, key).Err()
}