// Command rate-limit is an example Resin package plugin written with
// pkg/pluginsdk. It applies a token-bucket rate limit per account, client IP
// or platform and rejects requests over the limit with HTTP 429.
//
// Build and package (see README.md):
//
//	go build -o dist/example.rate-limit/bin/rate-limit ./examples/plugins/rate-limit
//	cp examples/plugins/rate-limit/plugin.json dist/example.rate-limit/
//	(cd dist && zip -r example.rate-limit.zip example.rate-limit)
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/maphash"
	"log"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Resinat/Resin/pkg/pluginsdk"
)

const (
	shardCount   = 64
	idleEviction = 10 * time.Minute
)

type config struct {
	Key            string   `json:"key"`
	RatePerSecond  float64  `json:"rate_per_second"`
	Burst          int      `json:"burst"`
	RejectStatus   int      `json:"reject_status"`
	RejectMessage  string   `json:"reject_message"`
	Platforms      []string `json:"platforms"`
	ExemptAccounts []string `json:"exempt_accounts"`
}

type settings struct {
	key       string
	rate      float64
	burst     float64
	status    int
	message   string
	platforms map[string]bool
	exempt    map[string]bool
}

type bucket struct {
	tokens float64
	last   time.Time
}

type shard struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type rateLimit struct {
	cfg    atomic.Pointer[settings]
	seed   maphash.Seed
	shards [shardCount]shard
	stop   chan struct{}
	once   sync.Once
}

func newRateLimit() *rateLimit {
	r := &rateLimit{seed: maphash.MakeSeed(), stop: make(chan struct{})}
	for i := range r.shards {
		r.shards[i].buckets = make(map[string]*bucket)
	}
	go r.evictLoop()
	return r
}

func (r *rateLimit) Init(_ context.Context, params pluginsdk.RegisterParams) error {
	log.Printf("rate-limit %s starting (resin %s)", params.PluginID, params.ResinVersion)
	return nil
}

func (r *rateLimit) Configure(_ context.Context, raw json.RawMessage) error {
	cfg := config{Key: "account", RatePerSecond: 10, Burst: 20, RejectStatus: http.StatusTooManyRequests}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return fmt.Errorf("invalid config: %w", err)
		}
	}
	switch cfg.Key {
	case "account", "client_ip", "platform", "account_or_ip":
	default:
		return fmt.Errorf("key must be account, client_ip, platform or account_or_ip")
	}
	if cfg.RatePerSecond <= 0 || math.IsInf(cfg.RatePerSecond, 0) || math.IsNaN(cfg.RatePerSecond) {
		return errors.New("rate_per_second must be greater than 0")
	}
	if cfg.Burst < 1 {
		return errors.New("burst must be at least 1")
	}
	if cfg.RejectStatus == 0 {
		cfg.RejectStatus = http.StatusTooManyRequests
	}
	if cfg.RejectStatus < 400 || cfg.RejectStatus > 599 {
		return errors.New("reject_status must be between 400 and 599")
	}
	s := &settings{
		key:     cfg.Key,
		rate:    cfg.RatePerSecond,
		burst:   float64(cfg.Burst),
		status:  cfg.RejectStatus,
		message: strings.TrimSpace(cfg.RejectMessage),
		exempt:  toSet(cfg.ExemptAccounts, false),
	}
	if len(cfg.Platforms) > 0 {
		s.platforms = toSet(cfg.Platforms, true)
	}
	if s.message == "" {
		s.message = "Rate limit exceeded"
	}
	old := r.cfg.Swap(s)
	if old != nil && old.key != s.key {
		// Buckets keyed by a different dimension are meaningless now.
		r.reset()
	}
	return nil
}

func (r *rateLimit) InspectRequest(_ context.Context, req *pluginsdk.RequestInfo) (*pluginsdk.RequestDecision, error) {
	s := r.cfg.Load()
	if s == nil {
		return nil, nil
	}
	if s.platforms != nil && !s.platforms[strings.ToLower(req.Platform)] {
		return nil, nil
	}
	if req.Account != "" && s.exempt[req.Account] {
		return nil, nil
	}
	key := bucketKey(s.key, req)
	if key == "" {
		return nil, nil
	}
	if r.allow(s, key, time.Now()) {
		return nil, nil
	}
	return pluginsdk.Reject(s.status, s.message), nil
}

func (r *rateLimit) Shutdown(context.Context) error {
	r.once.Do(func() { close(r.stop) })
	return nil
}

func bucketKey(kind string, req *pluginsdk.RequestInfo) string {
	switch kind {
	case "account":
		if req.Account == "" {
			return ""
		}
		return req.Platform + "\x00" + req.Account
	case "client_ip":
		return req.ClientIP
	case "platform":
		return req.Platform
	default: // account_or_ip
		if req.Account != "" {
			return "a\x00" + req.Platform + "\x00" + req.Account
		}
		return "ip\x00" + req.ClientIP
	}
}

func (r *rateLimit) allow(s *settings, key string, now time.Time) bool {
	sh := &r.shards[maphash.String(r.seed, key)%shardCount]
	sh.mu.Lock()
	defer sh.mu.Unlock()
	b := sh.buckets[key]
	if b == nil {
		b = &bucket{tokens: s.burst, last: now}
		sh.buckets[key] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens = math.Min(s.burst, b.tokens+elapsed*s.rate)
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (r *rateLimit) reset() {
	for i := range r.shards {
		sh := &r.shards[i]
		sh.mu.Lock()
		sh.buckets = make(map[string]*bucket)
		sh.mu.Unlock()
	}
}

func (r *rateLimit) evictLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case now := <-ticker.C:
			for i := range r.shards {
				sh := &r.shards[i]
				sh.mu.Lock()
				for k, b := range sh.buckets {
					if now.Sub(b.last) > idleEviction {
						delete(sh.buckets, k)
					}
				}
				sh.mu.Unlock()
			}
		}
	}
}

func toSet(values []string, foldCase bool) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if foldCase {
			v = strings.ToLower(v)
		}
		if v != "" {
			out[v] = true
		}
	}
	return out
}

func main() {
	// stdout carries the protocol; logs go to stderr and show up in the Resin log.
	log.SetOutput(os.Stderr)
	log.SetFlags(0)
	if err := pluginsdk.Serve(newRateLimit()); err != nil {
		log.Fatal(err)
	}
}
