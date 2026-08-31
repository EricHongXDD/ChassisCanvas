package edgesync

import (
	"testing"
	"time"
)

func TestRateLimiterRefillAndClientIsolation(t *testing.T) {
	limiter := newRateLimiter(2, 2, 2, time.Minute)
	now := time.Unix(100, 0)
	if !limiter.allow("one", now) || !limiter.allow("one", now) {
		t.Fatal("突发额度未正常放行")
	}
	if limiter.allow("one", now) {
		t.Fatal("超出突发额度的请求被放行")
	}
	if !limiter.allow("two", now) {
		t.Fatal("一个客户端耗尽了另一个客户端的额度")
	}
	if !limiter.allow("one", now.Add(500*time.Millisecond)) {
		t.Fatal("令牌桶没有按速率补充")
	}
}

func TestRateLimiterBoundsClientMap(t *testing.T) {
	limiter := newRateLimiter(1, 1, 1, time.Minute)
	now := time.Unix(100, 0)
	if !limiter.allow("known", now) {
		t.Fatal("首个客户端未放行")
	}
	if !limiter.allow("overflow-one", now) {
		t.Fatal("溢出桶首个请求未放行")
	}
	if limiter.allow("overflow-two", now) {
		t.Fatal("不同的新地址绕过了共享溢出桶")
	}
	if len(limiter.clients) != 1 {
		t.Fatalf("客户端表没有保持上限: %d", len(limiter.clients))
	}
}
