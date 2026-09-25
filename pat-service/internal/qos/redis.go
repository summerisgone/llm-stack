package qos

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStore implements Store and ActiveStore against Valkey
// (envoy-ratelimit-valkey, the same instance the rate limiter already uses --
// docs/adr/0008-per-user-fair-share.md "State: Valkey, not in-process
// memory"). Valkey speaks the Redis protocol, so an ordinary Redis client
// works unmodified.
type RedisStore struct {
	rdb *redis.Client
}

// NewRedisStore dials addr (host:port, no auth configured on this
// deployment). Dialing is lazy in go-redis; connectivity is only proven on
// first use, which is fine here since every use already tolerates failure.
func NewRedisStore(addr string) *RedisStore {
	return &RedisStore{rdb: redis.NewClient(&redis.Options{Addr: addr})}
}

const activeUsersKey = "qos:active-users"

func chainKey(sub, link string) string       { return "qos:chain:" + sub + ":" + link }
func stepsKey(sub, session string) string    { return "qos:steps:" + sub + ":" + session }
func activeKey(sub string) string            { return "qos:active:" + sub }
func streakSessionKey(sub string) string     { return "qos:streak:session:" + sub }
func streakCountKey(sub string) string       { return "qos:streak:count:" + sub }
func dispatchKey(sub, session string) string { return "qos:dispatch:" + sub + ":" + session }
func spendKey(sub string) string             { return "qos:spend:" + sub }

// IncrWindow increments a fixed-window counter and returns its new value;
// the key expires with the window. Used for the per-user MCP call limit
// (docs/adr/0017-web-search-mcp-openserp-kagent.md section 3).
func (s *RedisStore) IncrWindow(ctx context.Context, key string, window time.Duration) (int64, error) {
	n, err := s.rdb.Incr(ctx, key).Result()
	if err != nil {
		return 0, err
	}
	if n == 1 {
		s.rdb.Expire(ctx, key, window)
	}
	return n, nil
}

func (s *RedisStore) GetSessionForChain(ctx context.Context, sub, link string) (string, error) {
	v, err := s.rdb.Get(ctx, chainKey(sub, link)).Result()
	if err == redis.Nil {
		return "", nil
	}
	return v, err
}

func (s *RedisStore) SetSessionForChain(ctx context.Context, sub, link, sessionID string, ttl time.Duration) error {
	return s.rdb.Set(ctx, chainKey(sub, link), sessionID, ttl).Err()
}

func (s *RedisStore) IncrStep(ctx context.Context, sub, sessionID string, ttl time.Duration) (int64, error) {
	key := stepsKey(sub, sessionID)
	n, err := s.rdb.Incr(ctx, key).Result()
	if err != nil {
		return 0, err
	}
	// Best-effort TTL refresh: a failure here just means the step counter
	// expires a little earlier than intended, never a request-path error.
	s.rdb.Expire(ctx, key, ttl)
	return n, nil
}

// BumpStreak is a best-effort read-then-write, not an atomic
// check-and-increment: a race between two truly concurrent requests for the
// same user (at most two, bounded by --max-num-seqs) can under-count a
// streak by one. Acceptable here for the same reason the rest of this
// package tolerates it -- band assignment is a heuristic fairness signal,
// not a correctness-critical admission decision (ADR 0008 "Known
// limitations").
func (s *RedisStore) BumpStreak(ctx context.Context, sub, sessionID string, ttl time.Duration) (int64, error) {
	sessKey := streakSessionKey(sub)
	cntKey := streakCountKey(sub)
	prevSession, err := s.rdb.Get(ctx, sessKey).Result()
	if err != nil && err != redis.Nil {
		return 0, err
	}
	var count int64
	if prevSession == sessionID {
		count, err = s.rdb.Incr(ctx, cntKey).Result()
		if err != nil {
			return 0, err
		}
	} else {
		count = 1
		if err := s.rdb.Set(ctx, cntKey, count, ttl).Err(); err != nil {
			return 0, err
		}
	}
	if err := s.rdb.Set(ctx, sessKey, sessionID, ttl).Err(); err != nil {
		return 0, err
	}
	s.rdb.Expire(ctx, cntKey, ttl)
	return count, nil
}

func (s *RedisStore) TouchDispatch(ctx context.Context, sub, sessionID string, now time.Time, ttl time.Duration) (time.Duration, bool, error) {
	key := dispatchKey(sub, sessionID)
	prev, err := s.rdb.Get(ctx, key).Result()
	if err != nil && err != redis.Nil {
		return 0, false, err
	}
	if err := s.rdb.Set(ctx, key, now.Unix(), ttl).Err(); err != nil {
		return 0, false, err
	}
	if prev == "" {
		return 0, false, nil
	}
	prevUnix, err := strconv.ParseInt(prev, 10, 64)
	if err != nil {
		return 0, false, nil
	}
	return now.Sub(time.Unix(prevUnix, 0)), true, nil
}

func (s *RedisStore) GetSpend(ctx context.Context, sub string) (float64, error) {
	v, err := s.rdb.Get(ctx, spendKey(sub)).Result()
	if err == redis.Nil {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, nil
	}
	return f, nil
}

func (s *RedisStore) AddSpend(ctx context.Context, sub string, cost float64, window time.Duration) (float64, error) {
	key := spendKey(sub)
	total, err := s.rdb.IncrByFloat(ctx, key, cost).Result()
	if err != nil {
		return 0, err
	}
	// Best-effort TTL refresh, same as the other counters in this file: a
	// failure here just means spend expires a little earlier than
	// intended, never a request-path error.
	s.rdb.Expire(ctx, key, window)
	return total, nil
}

// MinActiveSpend is a plain loop over the (single-digit) active-user set,
// not a Redis-side aggregation -- there are only a handful of developers
// sharing this GPU (TASK-qos-fair-share.md §1), so this never needs to
// scale further. A user whose spend key has expired but who still lingers
// in qos:active-users (pruned lazily by the sweep in metrics.go, up to
// ~30s stale) reads as spend 0, which very slightly biases toward
// demoting others sooner rather than later; acceptable for the same
// reason the rest of this package tolerates imprecision.
func (s *RedisStore) MinActiveSpend(ctx context.Context, excludeSub string) (float64, bool, error) {
	users, err := s.ActiveUsers(ctx)
	if err != nil {
		return 0, false, err
	}
	min := 0.0
	found := false
	for _, u := range users {
		if u == excludeSub {
			continue
		}
		v, err := s.GetSpend(ctx, u)
		if err != nil {
			continue
		}
		if !found || v < min {
			min = v
			found = true
		}
	}
	return min, found, nil
}

func (s *RedisStore) TouchActive(ctx context.Context, sub, sessionID string, now time.Time) error {
	pipe := s.rdb.TxPipeline()
	pipe.ZAdd(ctx, activeKey(sub), redis.Z{Score: float64(now.Unix()), Member: sessionID})
	pipe.SAdd(ctx, activeUsersKey, sub)
	_, err := pipe.Exec(ctx)
	return err
}

func (s *RedisStore) ActiveUsers(ctx context.Context) ([]string, error) {
	return s.rdb.SMembers(ctx, activeUsersKey).Result()
}

// ActiveSessionCount prunes sub's active-session set to entries newer than
// cutoff and returns the remaining count. It also drops sub from the known-
// users set once its active set is empty, so the sweep does not grow
// unbounded with one-off/departed users.
func (s *RedisStore) ActiveSessionCount(ctx context.Context, sub string, cutoff time.Time) (int64, error) {
	key := activeKey(sub)
	if err := s.rdb.ZRemRangeByScore(ctx, key, "-inf", "("+strconv.FormatInt(cutoff.Unix(), 10)).Err(); err != nil {
		return 0, err
	}
	count, err := s.rdb.ZCard(ctx, key).Result()
	if err != nil {
		return 0, err
	}
	if count == 0 {
		s.rdb.SRem(ctx, activeUsersKey, sub)
	}
	return count, nil
}
