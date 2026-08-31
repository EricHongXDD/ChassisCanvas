package recognitionrelay

import (
	"math"
	"sync"
	"time"
)

type tokenBucket struct {
	tokens  float64
	updated time.Time
	seen    time.Time
}

type rateLimiter struct {
	mu          sync.Mutex
	rate        float64
	burst       float64
	clientTTL   time.Duration
	maxClients  int
	lastCleanup time.Time
	clients     map[string]*tokenBucket
	overflow    tokenBucket
}

func newRateLimiter(rate float64, burst, maxClients int, clientTTL time.Duration) *rateLimiter {
	return &rateLimiter{
		rate: rate, burst: float64(burst), maxClients: maxClients, clientTTL: clientTTL,
		clients: make(map[string]*tokenBucket),
	}
}

func (limiter *rateLimiter) allow(client string, now time.Time) bool {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if limiter.lastCleanup.IsZero() || now.Sub(limiter.lastCleanup) >= limiter.clientTTL {
		for key, bucket := range limiter.clients {
			if now.Sub(bucket.seen) >= limiter.clientTTL {
				delete(limiter.clients, key)
			}
		}
		limiter.lastCleanup = now
	}
	bucket, found := limiter.clients[client]
	if !found {
		if len(limiter.clients) >= limiter.maxClients {
			return limiter.take(&limiter.overflow, now)
		}
		bucket = &tokenBucket{tokens: limiter.burst, updated: now, seen: now}
		limiter.clients[client] = bucket
	}
	return limiter.take(bucket, now)
}

func (limiter *rateLimiter) take(bucket *tokenBucket, now time.Time) bool {
	if bucket.updated.IsZero() {
		bucket.tokens = limiter.burst
		bucket.updated = now
	}
	elapsed := now.Sub(bucket.updated).Seconds()
	if elapsed > 0 {
		bucket.tokens = math.Min(limiter.burst, bucket.tokens+elapsed*limiter.rate)
		bucket.updated = now
	}
	bucket.seen = now
	if bucket.tokens < 1 {
		return false
	}
	bucket.tokens--
	return true
}

func (limiter *rateLimiter) retryAfterSeconds() int {
	return max(1, int(math.Ceil(1/limiter.rate)))
}
