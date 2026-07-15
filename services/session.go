package services

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const LeaseTTL = 45 * time.Second

type LockResult int

const (
	LockGranted  LockResult = iota // new device, lock claimed
	LockRenewed                    // same device, lease extended
	LockConflict                   // another device holds the lock
)

type SessionGuard struct {
	RDB *redis.Client
}

// EvaluateAndLock is the symmetrical single-device enforcement algorithm.
// Returns LockGranted/LockRenewed when access is authorized, LockConflict when blocked.
func (sg *SessionGuard) EvaluateAndLock(ticketToken, deviceFingerprint string) (LockResult, error) {
	ctx := context.Background()
	key := fmt.Sprintf("session:ticket:%s", ticketToken)

	val, err := sg.RDB.Get(ctx, key).Result()

	switch {
	case err == redis.Nil:
		// No active session — set lock with 45s TTL
		if err := sg.RDB.Set(ctx, key, deviceFingerprint, LeaseTTL).Err(); err != nil {
			return LockConflict, fmt.Errorf("set lock: %w", err)
		}
		return LockGranted, nil

	case err != nil:
		return LockConflict, fmt.Errorf("redis get: %w", err)

	case val == deviceFingerprint:
		// Same device refreshing — extend the lease
		if err := sg.RDB.Expire(ctx, key, LeaseTTL).Err(); err != nil {
			return LockConflict, fmt.Errorf("extend lease: %w", err)
		}
		return LockRenewed, nil

	default:
		// A different device holds the lock
		return LockConflict, nil
	}
}

// Release immediately removes the lock so the viewer can switch devices without waiting 45s.
func (sg *SessionGuard) Release(ticketToken string) error {
	key := fmt.Sprintf("session:ticket:%s", ticketToken)
	return sg.RDB.Del(context.Background(), key).Err()
}
