package ipcheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/ipcheck/requestguard"
	"github.com/Resinat/Resin/internal/node"
)

type guardedFixtureHost struct {
	*fixtureHost
	guards *requestguard.Controller
}

func (h *guardedFixtureHost) RequestGuards() *requestguard.Controller { return h.guards }

func newGuardedFixtureHost() *guardedFixtureHost {
	now := time.Date(2026, 10, 14, 15, 0, 0, 0, time.UTC)
	return &guardedFixtureHost{fixtureHost: newFixtureHost(), guards: requestguard.New(func() time.Time { return now })}
}

func guardPolicy(source string, daily uint64) SourceGuardPolicy {
	return SourceGuardPolicy{Source: source, Enabled: true, QPS: 100, Burst: 64, DailyLimit: daily, ConsecutiveFailureThreshold: 2, OpenFor: "1m"}
}

func TestGuardEightTargetPartialExecutionNeverPublishesObservation(t *testing.T) {
	h := newGuardedFixtureHost()
	store := newFixtureStore()
	cfg := DefaultConfig()
	cfg.EnabledSources = []string{"egress-consistency", "db-ip"}
	cfg.ConsistencyTargets = nil
	for i := range MaxTargets {
		cfg.ConsistencyTargets = append(cfg.ConsistencyTargets, fmt.Sprintf("https://fixture.example/%d?private=never-emit", i))
	}
	cfg.SourceGuards = []SourceGuardPolicy{guardPolicy("egress-consistency", 3)}
	e := newTestEngine(t, h, store, cfg)
	e.ActivateRequestGuards()
	result, err := e.CheckNode(context.Background(), testHash(1), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if h.viaCalls.Load() != 3 || len(result.Sources) != 1 || result.Sources[0].Source != "db-ip" || result.Summary.Country != "US" || result.Summary.EgressConsistent != nil || len(result.Summary.EgressIPsBySite) != 0 {
		t.Fatalf("partial source became observation: calls=%d result=%+v", h.viaCalls.Load(), result)
	}
	if len(result.SuppressedSources) != 1 || result.SuppressedSources[0].Admitted != 3 || result.SuppressedSources[0].Suppressed != 5 || result.SuppressedSources[0].Reasons[0].Reason != requestguard.DailyLimited {
		t.Fatalf("incorrect bounded control metadata: %+v", result.SuppressedSources)
	}
	if !result.CheckedAt.Equal(result.Sources[0].CheckedAt) {
		t.Fatal("control decision changed observation timestamp")
	}
	store.mu.Lock()
	_, persisted := store.rows[cacheKey{SubjectNode, testHash(1).Hex(), "egress-consistency"}]
	store.mu.Unlock()
	if persisted {
		t.Fatal("partial source persisted")
	}
	encoded, err := json.Marshal(result)
	if err != nil || strings.Contains(string(encoded), "private=") || strings.Contains(string(encoded), "fixture.example") {
		t.Fatalf("control metadata exposed target: %s %v", encoded, err)
	}
}

func TestGuardAllSuppressedHasNoObservationTimeAndNoCache(t *testing.T) {
	h := newGuardedFixtureHost()
	cfg := DefaultConfig()
	cfg.EnabledSources = []string{"egress-consistency"}
	cfg.SourceGuards = []SourceGuardPolicy{guardPolicy("egress-consistency", 1)}
	e := newTestEngine(t, h, newFixtureStore(), cfg)
	e.ActivateRequestGuards()
	if _, err := e.CheckNode(context.Background(), testHash(1), nil, true); err != nil {
		t.Fatal(err)
	}
	result, err := e.CheckNode(context.Background(), testHash(2), nil, true)
	if err != nil || len(result.Sources) != 0 || !result.CheckedAt.IsZero() || len(result.SuppressedSources) != 1 || h.viaCalls.Load() != 1 {
		t.Fatalf("unexpected suppressed outcome: %+v %v", result, err)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "checked_at") || strings.Contains(string(encoded), "expires_at") || strings.Contains(string(encoded), "0001-") {
		t.Fatalf("fabricated observation timestamp: %s", encoded)
	}
	if _, err := e.GetNode(context.Background(), testHash(2)); !errors.Is(err, ErrNotChecked) {
		t.Fatalf("suppression became cache: %v", err)
	}
}

func TestGuardPolicyReplacementPreservesCacheAndProcessLedger(t *testing.T) {
	h := newGuardedFixtureHost()
	store := newFixtureStore()
	cfg := DefaultConfig()
	cfg.EnabledSources = []string{"egress-consistency"}
	old := newTestEngine(t, h, store, cfg)
	old.ActivateRequestGuards()
	first, err := old.CheckNode(context.Background(), testHash(1), nil, true)
	if err != nil {
		t.Fatal(err)
	}
	old.Close()
	cfg.SourceGuards = []SourceGuardPolicy{guardPolicy("egress-consistency", 1)}
	e := newTestEngine(t, h, store, cfg)
	if e.fingerprint != old.fingerprint {
		t.Fatal("control policy invalidated observation fingerprint")
	}
	cfg.SourceGuards[0].DailyLimit = 99 // Construction must detach caller config.
	e.ActivateRequestGuards()
	cached, err := e.CheckNode(context.Background(), testHash(1), nil, false)
	if err != nil || !reflect.DeepEqual(first, cached) || h.viaCalls.Load() != 1 {
		t.Fatalf("persistent cache was charged or changed: %+v %v", cached, err)
	}
	blocked, err := e.CheckNode(context.Background(), testHash(1), nil, true)
	if err != nil || len(blocked.SuppressedSources) != 1 || len(blocked.Sources) != 0 || h.viaCalls.Load() != 1 {
		t.Fatalf("force bypassed retained ledger: %+v %v", blocked, err)
	}
	after, err := e.GetNode(context.Background(), testHash(1))
	if err != nil || !reflect.DeepEqual(first, after) {
		t.Fatalf("control rejection overwrote old cache: %+v %v", after, err)
	}
	e.Close()
	third := newTestEngine(t, h, store, e.config)
	third.ActivateRequestGuards()
	afterDisk, err := third.GetNode(context.Background(), testHash(1))
	if err != nil || !reflect.DeepEqual(first, afterDisk) {
		t.Fatalf("control rejection overwrote persisted cache: %+v %v", afterDisk, err)
	}
}

func TestGuardJoinedWaiterCancellationDoesNotChargeOrCancelOtherWaiter(t *testing.T) {
	h := newGuardedFixtureHost()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	h.via = func(ctx context.Context, _ node.Hash, _ *http.Request) (*http.Response, error) {
		close(entered)
		select {
		case <-release:
			return echoResponse("8.8.8.8"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	cfg := DefaultConfig()
	cfg.EnabledSources = []string{"egress-consistency"}
	cfg.SourceGuards = []SourceGuardPolicy{guardPolicy("egress-consistency", 1)}
	e := newTestEngine(t, h, nil, cfg)
	e.ActivateRequestGuards()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, second := make(chan error, 1), make(chan *Result, 1)
	go func() { _, err := e.CheckNode(ctx, testHash(1), nil, true); first <- err }()
	<-entered
	go func() { result, _ := e.CheckNode(context.Background(), testHash(1), nil, true); second <- result }()
	waitFor(t, func() bool { return waiters(e) == 2 })
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("first waiter: %v", err)
	}
	once.Do(func() { close(release) })
	if result := <-second; result == nil || len(result.Sources) != 1 || len(result.SuppressedSources) != 0 || h.viaCalls.Load() != 1 {
		t.Fatalf("joined request was independently charged: %+v", result)
	}
	blocked, err := e.CheckNode(context.Background(), testHash(2), nil, true)
	if err != nil || len(blocked.SuppressedSources) != 1 || h.viaCalls.Load() != 1 {
		t.Fatalf("admitted request refunded on waiter cancellation: %+v %v", blocked, err)
	}
}

func TestGuardHeaderFeedbackSeparatesTypedTransportAndRiskObservations(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		status int
		err    error
		want   requestguard.Outcome
	}{
		{200, nil, requestguard.Success}, {403, nil, requestguard.Success},
		{429, nil, requestguard.Failure}, {503, nil, requestguard.Failure},
		{0, nil, requestguard.Neutral}, {999, nil, requestguard.Neutral},
		{0, &HostError{Kind: HostTransportFailure}, requestguard.Failure},
		{0, &url.Error{Op: "Get", URL: "https://private.example/?token=secret", Err: &HostError{Kind: HostTransportFailure}}, requestguard.Failure},
		{0, &HostError{Kind: HostInvalidTarget}, requestguard.Neutral},
		{0, &HostError{Kind: HostDNSUnavailable}, requestguard.Neutral},
		{0, &HostError{Kind: HostNodeUnavailable}, requestguard.Neutral},
		{0, errors.New("unknown network-looking error"), requestguard.Neutral},
		{0, context.Canceled, requestguard.Neutral}, {0, context.DeadlineExceeded, requestguard.Neutral},
	} {
		if got := guardOutcome(ctx, &http.Response{StatusCode: tc.status}, tc.err); got != tc.want {
			t.Fatalf("status %d err %v outcome=%v want=%v", tc.status, tc.err, got, tc.want)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if got := guardOutcome(canceled, &http.Response{StatusCode: 503}, nil); got != requestguard.Neutral {
		t.Fatal("caller cancellation became provider failure")
	}
}

func TestGuardSuppressedFlightSharesAdmissionButNotMutableMetadata(t *testing.T) {
	h := newGuardedFixtureHost()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	h.via = func(ctx context.Context, _ node.Hash, _ *http.Request) (*http.Response, error) {
		close(entered)
		select {
		case <-release:
			return echoResponse("8.8.8.8"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	cfg := DefaultConfig()
	cfg.EnabledSources = []string{"egress-consistency"}
	cfg.ConsistencyTargets = []string{"https://fixture.example/a", "https://fixture.example/b"}
	cfg.SourceGuards = []SourceGuardPolicy{guardPolicy("egress-consistency", 1)}
	e := newTestEngine(t, h, nil, cfg)
	e.ActivateRequestGuards()
	results := make(chan *Result, 2)
	check := func() { result, _ := e.CheckNode(context.Background(), testHash(1), nil, true); results <- result }
	go check()
	<-entered
	go check()
	waitFor(t, func() bool { return waiters(e) == 2 })
	once.Do(func() { close(release) })
	first, second := <-results, <-results
	if first == nil || second == nil || len(first.SuppressedSources) != 1 || !reflect.DeepEqual(first, second) || h.viaCalls.Load() != 1 {
		t.Fatalf("shared suppression differs or charged waiter: first=%+v second=%+v calls=%d", first, second, h.viaCalls.Load())
	}
	first.SuppressedSources[0].Reasons[0].Count = 42
	*first.SuppressedSources[0].Reasons[0].RetryAt = time.Time{}
	if second.SuppressedSources[0].Reasons[0].Count != 1 || second.SuppressedSources[0].Reasons[0].RetryAt.IsZero() {
		t.Fatal("singleflight leaked shared mutable control metadata")
	}
}

func TestGuardClosedEngineCannotRestoreOldPolicy(t *testing.T) {
	h := newGuardedFixtureHost()
	cfg := DefaultConfig()
	cfg.SourceGuards = []SourceGuardPolicy{guardPolicy("egress-consistency", 100)}
	old := newTestEngine(t, h, nil, cfg)
	old.ActivateRequestGuards()
	old.Close()
	cfg.SourceGuards[0].DailyLimit = 1
	current := newTestEngine(t, h, nil, cfg)
	current.ActivateRequestGuards()
	permit, err := h.guards.Admit(context.Background(), requestguard.EgressConsistency)
	if err != nil {
		t.Fatal(err)
	}
	permit.Finish(requestguard.Neutral)
	old.ActivateRequestGuards()
	if permit, err := h.guards.Admit(context.Background(), requestguard.EgressConsistency); err == nil {
		permit.Finish(requestguard.Neutral)
		t.Fatal("closed engine restored old wide policy")
	}
}

func TestGuardHTTPFeedbackPreservesRealRiskAndChallengeEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, source, body, classification string
		status                             int
		trips                              bool
	}{
		{"AI429", "ai-region", "limited", "risky", 429, true},
		{"CF503Challenge", "cloudflare-challenge", "challenge-platform", "challenge", 503, true},
		{"CF403Blocked", "cloudflare-challenge", "Forbidden", "blocked", 403, false},
		{"BadEchoAfter200", "egress-consistency", "not an IP", "", 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newGuardedFixtureHost()
			h.via = func(context.Context, node.Hash, *http.Request) (*http.Response, error) {
				response := echoResponse(tc.body)
				response.StatusCode = tc.status
				return response, nil
			}
			cfg := DefaultConfig()
			cfg.EnabledSources = []string{tc.source}
			cfg.CloudflareChallengeURLs = []string{"https://fixture.example/challenge"}
			cfg.AIRegionTargets = map[string]AIRegionTargetConfig{"chatgpt": {URL: "https://fixture.example/region"}}
			policy := guardPolicy(tc.source, 10)
			policy.ConsecutiveFailureThreshold = 1
			cfg.SourceGuards = []SourceGuardPolicy{policy}
			e := newTestEngine(t, h, newFixtureStore(), cfg)
			e.ActivateRequestGuards()
			first, err := e.CheckNode(context.Background(), testHash(1), nil, true)
			if err != nil || len(first.Sources) != 1 || len(first.SuppressedSources) != 0 {
				t.Fatalf("real response lost: %+v %v", first, err)
			}
			data := first.Sources[0].Data
			if tc.source == "ai-region" && data.Services["chatgpt"] != tc.classification || tc.source == "cloudflare-challenge" && data.CFChallenge != tc.classification || tc.source == "egress-consistency" && first.Sources[0].OK {
				t.Fatalf("guard changed adapter evidence: %+v", first.Sources[0])
			}
			next, err := e.CheckNode(context.Background(), testHash(2), nil, true)
			if err != nil || (len(next.SuppressedSources) > 0) != tc.trips {
				t.Fatalf("wrong header feedback: %+v %v", next, err)
			}
			if tc.trips && (h.viaCalls.Load() != 1 || next.SuppressedSources[0].Reasons[0].Reason != requestguard.CircuitOpen) || !tc.trips && h.viaCalls.Load() != 2 {
				t.Fatalf("wrong gate/call count: %+v calls=%d", next, h.viaCalls.Load())
			}
			cached, err := e.GetNode(context.Background(), testHash(1))
			if err != nil || !reflect.DeepEqual(first, cached) {
				t.Fatalf("real response not retained independently: %+v %v", cached, err)
			}
		})
	}
}

func TestGuardBatchSuppressionIsCompletedWithoutObservationAndSnapshotIsDetached(t *testing.T) {
	h := newGuardedFixtureHost()
	cfg := DefaultConfig()
	cfg.EnabledSources = []string{"egress-consistency"}
	cfg.SourceGuards = []SourceGuardPolicy{guardPolicy("egress-consistency", 1)}
	e := newTestEngine(t, h, nil, cfg)
	e.ActivateRequestGuards()
	if _, err := e.CheckNode(context.Background(), testHash(1), nil, true); err != nil {
		t.Fatal(err)
	}
	task, _, err := e.SubmitBatch(context.Background(), "guarded", []node.Hash{testHash(2)}, cfg.EnabledSources, true)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		task, err = e.GetBatch(context.Background(), task.ID)
		return err == nil && task.State == "completed"
	})
	item := task.Items[0]
	if item.State != "completed" || item.ObservationStatus != "suppressed" || item.CheckedAt != nil || len(item.Sources) != 0 || len(item.SuppressedSources) != 1 || item.Error != nil {
		t.Fatalf("suppression represented as observation/failure: %+v", item)
	}
	item.SuppressedSources[0].Reasons[0].Count = 42
	*item.SuppressedSources[0].Reasons[0].RetryAt = time.Time{}
	again, err := e.GetBatch(context.Background(), task.ID)
	if err != nil || again.Items[0].SuppressedSources[0].Reasons[0].Count != 1 || again.Items[0].SuppressedSources[0].Reasons[0].RetryAt.IsZero() {
		t.Fatalf("mutable snapshot: %+v %v", again, err)
	}
}
