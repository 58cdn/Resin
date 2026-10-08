package state

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/ipcheck"
	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/node"
)

func ipCheckFixture(source string, kind ipcheck.SourceKind) *ipcheck.SourceResult {
	now := time.Now().UTC()
	return &ipcheck.SourceResult{
		Source: source, Kind: kind, OK: true, Status: "ok", CheckedAt: now, ExpiresAt: now.Add(time.Hour),
		EgressIP: "203.0.113.10", Data: ipcheck.Data{Country: "us", AbuseFlags: []string{"fixture"}},
	}
}

func countIPChecks(t *testing.T, e *StateEngine) int {
	t.Helper()
	var count int
	if err := e.CacheRepo.db.QueryRow(`SELECT COUNT(*) FROM ip_check_results`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestIPCheckStoreBatchRoundTripAndCopies(t *testing.T) {
	e, _, _ := newTestEngine(t)
	store := e.IPCheckStore()
	ctx := context.Background()
	result := ipCheckFixture("db-ip", ipcheck.SourceKindByIP)
	if err := store.Save(ctx, ipcheck.SubjectIP, result.EgressIP, result); err != nil {
		t.Fatal(err)
	}
	result.Data.AbuseFlags[0] = "caller mutation"
	if countIPChecks(t, e) != 0 || e.DirtyCount() != 1 {
		t.Fatal("Save must only enqueue a weak-persist write")
	}
	loaded, err := store.Load(ctx, ipcheck.SubjectIP, result.EgressIP, result.Source)
	if err != nil || loaded == nil || loaded.Data.AbuseFlags[0] != "fixture" {
		t.Fatalf("pending round trip: %+v, %v", loaded, err)
	}
	loaded.Data.AbuseFlags[0] = "reader mutation"
	if err := e.FlushDirtySets(CacheReaders{}); err != nil {
		t.Fatal(err)
	}
	if countIPChecks(t, e) != 1 || e.DirtyCount() != 0 {
		t.Fatal("flush did not persist and clear pending write")
	}
	loaded, err = store.Load(ctx, ipcheck.SubjectIP, result.EgressIP, result.Source)
	if err != nil || loaded == nil || loaded.Data.AbuseFlags[0] != "fixture" {
		t.Fatalf("persisted round trip: %+v, %v", loaded, err)
	}
}

func TestIPCheckStoreExpiryAndRestart(t *testing.T) {
	stateDir, cacheDir := t.TempDir(), t.TempDir()
	e, closer, err := PersistenceBootstrap(stateDir, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	store := e.IPCheckStore()
	ctx := context.Background()
	fresh := ipCheckFixture("db-ip", ipcheck.SourceKindByIP)
	if err := store.Save(ctx, ipcheck.SubjectIP, fresh.EgressIP, fresh); err != nil {
		t.Fatal(err)
	}
	// Exercise final shutdown flushing with no unrelated dirty readers.
	worker := NewCacheFlushWorker(e, CacheReaders{}, func() int { return 1000 }, func() time.Duration { return time.Hour }, time.Hour)
	worker.Start()
	worker.Stop()
	if countIPChecks(t, e) != 1 {
		t.Fatal("shutdown did not flush IP checks")
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	e, closer, err = PersistenceBootstrap(stateDir, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	store = e.IPCheckStore()
	if loaded, err := store.Load(ctx, ipcheck.SubjectIP, fresh.EgressIP, fresh.Source); err != nil || loaded == nil {
		t.Fatalf("restart lost result: %+v, %v", loaded, err)
	}
	// Expired persisted entries are never returned, and reads queue cleanup.
	if _, err := e.CacheRepo.db.Exec(`UPDATE ip_check_results SET expires_at_ns = ?`, time.Now().Add(-time.Second).UnixNano()); err != nil {
		t.Fatal(err)
	}
	if loaded, err := store.Load(ctx, ipcheck.SubjectIP, fresh.EgressIP, fresh.Source); err != nil || loaded != nil {
		t.Fatalf("expired result returned: %+v, %v", loaded, err)
	}
	if err := e.FlushDirtySets(CacheReaders{}); err != nil {
		t.Fatal(err)
	}
	if countIPChecks(t, e) != 0 {
		t.Fatal("expired row not reclaimed")
	}
}

func TestIPCheckStoreNodeInvalidationAndPhysicalDeletion(t *testing.T) {
	e, _, _ := newTestEngine(t)
	ctx := context.Background()
	store := e.IPCheckStore()
	hash := node.Hash{1}
	if err := e.BulkUpsertNodesStatic([]model.NodeStatic{{Hash: hash.Hex(), RawOptions: json.RawMessage(`{}`), CreatedAtNs: 1}}); err != nil {
		t.Fatal(err)
	}
	result := ipCheckFixture("egress-consistency", ipcheck.SourceKindViaNode)
	if err := store.Save(ctx, ipcheck.SubjectNode, hash.Hex(), result); err != nil {
		t.Fatal(err)
	}
	if err := e.FlushDirtySets(CacheReaders{}); err != nil {
		t.Fatal(err)
	}
	if countIPChecks(t, e) != 1 {
		t.Fatal("node-owned result not persisted")
	}
	if err := store.DeleteNode(ctx, hash); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Load(ctx, ipcheck.SubjectNode, hash.Hex(), result.Source); err != nil || got != nil {
		t.Fatalf("invalidated result visible before flush: %+v, %v", got, err)
	}
	result.EgressIP = "203.0.113.11"
	result.CheckedAt = result.CheckedAt.Add(time.Second)
	if err := store.Save(ctx, ipcheck.SubjectNode, hash.Hex(), result); err != nil {
		t.Fatal(err)
	}
	if err := e.FlushDirtySets(CacheReaders{}); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Load(ctx, ipcheck.SubjectNode, hash.Hex(), result.Source); err != nil || got == nil || got.EgressIP != result.EgressIP {
		t.Fatalf("new result lost after invalidation: %+v, %v", got, err)
	}
	// Physical deletion must clean results even outside the store adapter.
	if err := e.BulkDeleteNodesStatic([]string{hash.Hex()}); err != nil {
		t.Fatal(err)
	}
	if countIPChecks(t, e) != 0 {
		t.Fatal("physical node deletion left results")
	}
	// A late check cannot recreate a persisted orphan.
	if err := store.Save(ctx, ipcheck.SubjectNode, hash.Hex(), result); err != nil {
		t.Fatal(err)
	}
	if err := e.FlushDirtySets(CacheReaders{}); err != nil {
		t.Fatal(err)
	}
	if countIPChecks(t, e) != 0 {
		t.Fatal("late save recreated deleted-node result")
	}
}

func TestIPCheckStoreFlushFailureRetainsJournal(t *testing.T) {
	e, _, _ := newTestEngine(t)
	ctx := context.Background()
	store := e.IPCheckStore()
	result := ipCheckFixture("db-ip", ipcheck.SourceKindByIP)
	if err := store.Save(ctx, ipcheck.SubjectIP, result.EgressIP, result); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CacheRepo.db.Exec(`DROP TABLE ip_check_results`); err != nil {
		t.Fatal(err)
	}
	if err := e.FlushDirtySets(CacheReaders{}); err == nil {
		t.Fatal("expected flush failure")
	}
	if e.DirtyCount() != 1 {
		t.Fatal("failed flush discarded journal")
	}
	migration, err := migrationsFS.ReadFile(cacheMigrationsPath + "/000003_ip_check_results.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.CacheRepo.db.Exec(string(migration)); err != nil {
		t.Fatal(err)
	}
	if err := e.FlushDirtySets(CacheReaders{}); err != nil {
		t.Fatal(err)
	}
	if countIPChecks(t, e) != 1 {
		t.Fatal("retry lost pending result")
	}
}

func TestIPCheckStoreInputAndCorruptionValidation(t *testing.T) {
	e, _, _ := newTestEngine(t)
	store := e.IPCheckStore()
	ctx := context.Background()
	result := ipCheckFixture("db-ip", ipcheck.SourceKindByIP)
	for _, pair := range [][2]string{{"unknown", result.EgressIP}, {ipcheck.SubjectIP, "bad ip"}, {ipcheck.SubjectNode, "bad hash"}} {
		if err := store.Save(ctx, pair[0], pair[1], result); err == nil {
			t.Errorf("accepted invalid subject %+v", pair)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := store.Save(cancelled, ipcheck.SubjectIP, result.EgressIP, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("Save ignored cancellation: %v", err)
	}
	if _, err := store.Load(cancelled, ipcheck.SubjectIP, result.EgressIP, result.Source); !errors.Is(err, context.Canceled) {
		t.Fatalf("Load ignored cancellation: %v", err)
	}
	if err := store.DeleteNode(cancelled, node.Hash{1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("DeleteNode ignored cancellation: %v", err)
	}
	result.Error = strings.Repeat("x", maxIPCheckResultBytes)
	if err := store.Save(ctx, ipcheck.SubjectIP, result.EgressIP, result); err == nil {
		t.Fatal("oversized record accepted")
	}
	result.Error = ""
	if err := store.Save(ctx, ipcheck.SubjectIP, result.EgressIP, result); err != nil {
		t.Fatal(err)
	}
	if err := e.FlushDirtySets(CacheReaders{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CacheRepo.db.Exec(`UPDATE ip_check_results SET result_json = '{broken'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(ctx, ipcheck.SubjectIP, result.EgressIP, result.Source); err == nil {
		t.Fatal("corrupt persisted JSON accepted")
	}
}

func TestIPCheckCacheMigrationAndBootstrapCleanup(t *testing.T) {
	stateDir, cacheDir := t.TempDir(), t.TempDir()
	e, closer, err := PersistenceBootstrap(stateDir, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	// Insert fixture rows directly to represent data left by an older process.
	for _, values := range [][3]any{
		{ipcheck.SubjectIP, "203.0.113.10", time.Now().Add(-time.Second).UnixNano()},
		{ipcheck.SubjectNode, node.Hash{1}.Hex(), time.Now().Add(time.Hour).UnixNano()},
		{ipcheck.SubjectIP, "203.0.113.11", time.Now().Add(time.Hour).UnixNano()},
	} {
		if _, err := e.CacheRepo.db.Exec(`INSERT INTO ip_check_results VALUES (?, ?, 'fixture', '{}', 1, ?)`, values[0], values[1], values[2]); err != nil {
			t.Fatal(err)
		}
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	e, closer, err = PersistenceBootstrap(stateDir, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	if countIPChecks(t, e) != 1 {
		t.Fatal("bootstrap did not clean expired and orphaned node rows")
	}
	// Upgrade an existing v2 cache while preserving its node data.
	db, err := OpenDB(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, name := range []string{"000001_cache_base.up.sql", "000002_subscription_nodes_evicted.up.sql"} {
		migration, err := migrationsFS.ReadFile(cacheMigrationsPath + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(migration)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version uint64 PRIMARY KEY, dirty bool); INSERT INTO schema_migrations VALUES (2, 0); INSERT INTO nodes_static VALUES ('fixture', '{}', 1)`); err != nil {
		t.Fatal(err)
	}
	if err := MigrateCacheDB(db); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM nodes_static`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("migration lost existing data: %d, %v", count, err)
	}
	if exists, err := hasTable(db, "ip_check_results"); err != nil || !exists {
		t.Fatalf("migration missing table: %v, %v", exists, err)
	}
}

func TestIPCheckStoreConcurrentSaveLoadFlush(t *testing.T) {
	e, _, _ := newTestEngine(t)
	store := e.IPCheckStore()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 8; j++ {
				result := ipCheckFixture("db-ip", ipcheck.SourceKindByIP)
				if err := store.Save(context.Background(), ipcheck.SubjectIP, result.EgressIP, result); err != nil {
					t.Error(err)
				}
				if _, err := store.Load(context.Background(), ipcheck.SubjectIP, result.EgressIP, result.Source); err != nil {
					t.Error(err)
				}
				if err := e.FlushDirtySets(CacheReaders{}); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if countIPChecks(t, e) != 1 {
		t.Fatal("concurrent operations lost result")
	}
}
