package ipcheck

import (
	"context"
	"errors"
	"net/netip"
	"sync/atomic"
	"testing"
)

func TestCanceledCallerCannotEnterSourceAfterLockWait(t *testing.T) {
	host := newFixtureHost()
	e := newTestEngine(t, host, nil, DefaultConfig())
	ip := netip.MustParseAddr("8.8.8.8")
	if _, err := e.Lookup(context.Background(), ip, []string{"db-ip"}, false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	done := make(chan struct{})
	var result *SourceResult
	var err error
	e.mu.Lock()
	go func() {
		close(entered)
		result, err = e.checkSource(ctx, CheckRequest{EgressIP: ip}, 0, "db-ip", false)
		close(done)
	}()
	<-entered
	// The caller cannot pass admission until this lock is released.
	cancel()
	e.mu.Unlock()
	<-done
	if !errors.Is(err, context.Canceled) || result != nil {
		t.Fatalf("canceled admission returned result=%+v err=%v", result, err)
	}
	if host.geoCalls.Load() != 1 {
		t.Fatal("canceled caller must not start another source")
	}
}

type cancelLoadStore struct {
	*fixtureStore
	entered chan struct{}
}

func (s *cancelLoadStore) Load(ctx context.Context, _, _, _ string) (*SourceResult, error) {
	close(s.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

type admissionSource struct{ calls atomic.Int32 }

func (*admissionSource) Name() string      { return "db-ip" }
func (*admissionSource) Kind() SourceKind  { return SourceKindByIP }
func (*admissionSource) RequiresKey() bool { return false }
func (s *admissionSource) Check(context.Context, CheckRequest) (*SourceResult, error) {
	s.calls.Add(1)
	return &SourceResult{OK: true, Status: "ok"}, nil
}

func TestCanceledCacheLoadCannotStartSource(t *testing.T) {
	store := &cancelLoadStore{fixtureStore: newFixtureStore(), entered: make(chan struct{})}
	e := newTestEngine(t, newFixtureHost(), store, DefaultConfig())
	source := &admissionSource{}
	e.sources["db-ip"] = source
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := e.Lookup(ctx, netip.MustParseAddr("8.8.8.8"), []string{"db-ip"}, false)
		done <- err
	}()
	<-store.entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("lookup after cancellation: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if got := source.calls.Load(); got != 0 {
		t.Fatalf("source started %d times after canceled cache load", got)
	}
	if len(store.rows) != 0 {
		t.Fatal("canceled work must not save a result")
	}
}
