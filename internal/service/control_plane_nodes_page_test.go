package service

import (
	"cmp"
	"fmt"
	"math"
	"math/rand/v2"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/geoip"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/Resinat/Resin/internal/testutil"
	"github.com/Resinat/Resin/internal/topology"
)

// populateNodeListFixture registers a fixed set of subscriptions and loads
// nodeCount random nodes into every pool. The nodes cover the edge cases of
// node list sorting: shared tags and creation times, evicted and stale
// subscription references, empty subscription names and tags, and nodes
// without an egress IP.
func populateNodeListFixture(
	tb testing.TB,
	rng *rand.Rand,
	subMgr *topology.SubscriptionManager,
	nodeCount int,
	pools ...*topology.GlobalNodePool,
) {
	tb.Helper()

	subs := []*subscription.Subscription{
		subscription.NewSubscription("sub-a", "alpha", "https://example.com/a", true, false),
		subscription.NewSubscription("sub-b", "beta", "https://example.com/b", true, false),
		subscription.NewSubscription("sub-c", "gamma", "https://example.com/c", false, false),
		subscription.NewSubscription("sub-d", "", "https://example.com/d", true, false),
	}
	// alpha and beta share a creation time; the disabled gamma is the oldest.
	createdAtNs := []int64{100, 100, 50, 200}
	for i, sub := range subs {
		sub.CreatedAtNs = createdAtNs[i]
		subMgr.Register(sub)
	}
	// Nodes may also reference a subscription that no longer exists.
	subIDs := []string{"sub-a", "sub-b", "sub-c", "sub-d", "sub-missing"}
	tagPool := []string{"hk-01", "hk-02", "jp-01", "us-01", ""}
	egressPool := []netip.Addr{
		netip.MustParseAddr("203.0.113.1"),
		netip.MustParseAddr("203.0.113.2"),
		netip.MustParseAddr("198.51.100.7"),
		netip.MustParseAddr("2001:db8::1"),
		netip.MustParseAddr("::ffff:203.0.113.1"),
	}
	regionPool := []string{"hk", "jp", "us"}
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	for i := range nodeCount {
		raw := []byte(fmt.Sprintf(`{"type":"ss","server":"fixture-%d.example","port":443}`, i))
		hash := node.HashFromRawOptions(raw)
		// 25ms steps make creation-time ties common and vary the number of
		// fractional digits RFC3339Nano prints.
		createdAt := base.Add(time.Duration(rng.IntN(40)) * 25 * time.Millisecond)
		entry := node.NewNodeEntry(hash, raw, createdAt, 0)

		for _, subID := range subIDs {
			if rng.IntN(3) != 0 {
				continue
			}
			entry.AddSubscriptionID(subID)
			sub := subMgr.Lookup(subID)
			if sub == nil || rng.IntN(10) == 0 {
				continue // stale reference without a managed-node entry
			}
			var tags []string
			for range rng.IntN(3) {
				tags = append(tags, tagPool[rng.IntN(len(tagPool))])
			}
			sub.ManagedNodes().StoreNode(hash, subscription.ManagedNode{
				Tags:    tags,
				Evicted: rng.IntN(6) == 0,
			})
		}

		entry.FailureCount.Store(int32(rng.IntN(4)))
		if rng.IntN(4) != 0 {
			entry.SetEgressIP(egressPool[rng.IntN(len(egressPool))])
		}
		if rng.IntN(3) == 0 {
			// Summaries drop a stored region when the egress IP is unknown.
			entry.SetEgressRegion(regionPool[rng.IntN(len(regionPool))])
		}
		if rng.IntN(4) != 0 {
			ob := testutil.NewNoopOutbound()
			entry.Outbound.Store(&ob)
		}
		if rng.IntN(4) == 0 {
			entry.CircuitOpenSince.Store(base.Add(time.Duration(i) * time.Second).UnixNano())
		}
		for _, pool := range pools {
			pool.LoadNodeFromBootstrap(entry)
		}
	}
}

// referenceNodeListPage reproduces the list path ListNodesPage replaces:
// summarize every filtered node, stable-sort the summaries, then slice out the
// page. created_at compares as time, the order ListNodesPage is meant to give.
func referenceNodeListPage(tb testing.TB, cp *ControlPlaneService, q NodeListQuery) *NodeListPage {
	tb.Helper()

	nodes, err := cp.ListNodes(q.Filters)
	if err != nil {
		tb.Fatalf("ListNodes: %v", err)
	}
	slices.SortStableFunc(nodes, func(a, b NodeSummary) int {
		order := referenceCompareNodeSummaries(tb, q.SortBy, a, b)
		if q.SortOrder == "desc" {
			return -order
		}
		return order
	})

	page := &NodeListPage{Items: []NodeSummary{}, Total: len(nodes)}
	if q.Offset < len(nodes) {
		page.Items = nodes[q.Offset:min(q.Offset+q.Limit, len(nodes))]
	}
	egressIPs := make(map[string]struct{})
	healthyEgressIPs := make(map[string]struct{})
	for _, n := range nodes {
		if n.EgressIP == "" {
			continue
		}
		egressIPs[n.EgressIP] = struct{}{}
		if n.IsHealthyAndEnabled() {
			healthyEgressIPs[n.EgressIP] = struct{}{}
		}
	}
	page.UniqueEgressIPs = len(egressIPs)
	page.UniqueHealthyEgressIPs = len(healthyEgressIPs)
	return page
}

func referenceCompareNodeSummaries(tb testing.TB, sortBy string, a, b NodeSummary) int {
	order := 0
	switch sortBy {
	case "created_at":
		order = parseSummaryTime(tb, a.CreatedAt).Compare(parseSummaryTime(tb, b.CreatedAt))
	case "failure_count":
		order = cmp.Compare(a.FailureCount, b.FailureCount)
	case "region":
		order = strings.Compare(a.Region, b.Region)
	default:
		order = strings.Compare(referenceNodeTagSortKey(a), referenceNodeTagSortKey(b))
	}
	if order != 0 {
		return order
	}
	return strings.Compare(a.NodeHash, b.NodeHash)
}

func referenceNodeTagSortKey(n NodeSummary) string {
	if n.DisplayTag != "" {
		return n.DisplayTag
	}
	bestCreated := int64(math.MaxInt64)
	bestTag := ""
	for _, t := range n.Tags {
		if t.SubscriptionCreatedAtNs < bestCreated {
			bestCreated = t.SubscriptionCreatedAtNs
			bestTag = t.Tag
			continue
		}
		if t.SubscriptionCreatedAtNs == bestCreated && (bestTag == "" || t.Tag < bestTag) {
			bestTag = t.Tag
		}
	}
	return bestTag
}

func parseSummaryTime(tb testing.TB, s string) time.Time {
	tb.Helper()
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		tb.Fatalf("parse summary time %q: %v", s, err)
	}
	return ts
}

func assertNodeListPagesEqual(t *testing.T, got, want *NodeListPage) {
	t.Helper()

	if got.Total != want.Total ||
		got.UniqueEgressIPs != want.UniqueEgressIPs ||
		got.UniqueHealthyEgressIPs != want.UniqueHealthyEgressIPs {
		t.Fatalf(
			"aggregates: got total=%d unique=%d healthy=%d, want total=%d unique=%d healthy=%d",
			got.Total, got.UniqueEgressIPs, got.UniqueHealthyEgressIPs,
			want.Total, want.UniqueEgressIPs, want.UniqueHealthyEgressIPs,
		)
	}
	if len(got.Items) != len(want.Items) {
		t.Fatalf("items: got %d, want %d", len(got.Items), len(want.Items))
	}
	for i := range want.Items {
		if !reflect.DeepEqual(got.Items[i], want.Items[i]) {
			t.Fatalf("item %d:\n got %+v\nwant %+v", i, got.Items[i], want.Items[i])
		}
	}
}

func TestListNodesPage_MatchesFullSortAndPaginate(t *testing.T) {
	subMgr := topology.NewSubscriptionManager()
	withLookup := newNodeListTestPool(subMgr)
	// Without SubLookup the pool resolves no display tags, so every tag sort
	// key comes from the subscription manager fallback.
	withoutLookup := topology.NewGlobalNodePool(topology.PoolConfig{
		MaxConsecutiveFailures: func() int { return 3 },
	})
	populateNodeListFixture(t, rand.New(rand.NewPCG(20260928, 4)), subMgr, 400, withLookup, withoutLookup)

	services := []struct {
		name string
		cp   *ControlPlaneService
	}{
		{"sub_lookup", &ControlPlaneService{Pool: withLookup, SubMgr: subMgr, GeoIP: &geoip.Service{}}},
		{"no_sub_lookup", &ControlPlaneService{Pool: withoutLookup, SubMgr: subMgr}},
	}

	// Guard the fixture: some nodes must sort by a tag other than their
	// display tag, or the fallback path goes untested.
	all, err := services[0].cp.ListNodes(NodeFilters{})
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	fallbacks := 0
	for _, n := range all {
		if n.DisplayTag == "" && referenceNodeTagSortKey(n) != "" {
			fallbacks++
		}
	}
	if fallbacks == 0 {
		t.Fatal("fixture has no node whose tag sort key falls back past the display tag")
	}

	subID := "sub-a"
	enabled, disabled := true, false
	keyword := "hk"
	filterCases := []struct {
		name    string
		filters NodeFilters
	}{
		{"all", NodeFilters{}},
		{"subscription", NodeFilters{SubscriptionID: &subID}},
		{"enabled", NodeFilters{Enabled: &enabled}},
		{"disabled", NodeFilters{Enabled: &disabled}},
		{"tag_keyword", NodeFilters{TagKeyword: &keyword}},
	}
	windows := []struct{ offset, limit int }{
		{0, 1}, {0, 7}, {3, 50}, {0, 100000}, {390, 50}, {5000, 10},
	}

	for _, svc := range services {
		for _, fc := range filterCases {
			for _, sortBy := range []string{"tag", "created_at", "failure_count", "region"} {
				for _, sortOrder := range []string{"asc", "desc"} {
					for _, w := range windows {
						q := NodeListQuery{
							Filters:   fc.filters,
							SortBy:    sortBy,
							SortOrder: sortOrder,
							Offset:    w.offset,
							Limit:     w.limit,
						}
						name := fmt.Sprintf("%s,%s,%s_%s,offset=%d,limit=%d", svc.name, fc.name, sortBy, sortOrder, w.offset, w.limit)
						t.Run(name, func(t *testing.T) {
							want := referenceNodeListPage(t, svc.cp, q)
							if want.Total == 0 {
								t.Fatal("fixture matched no nodes")
							}
							got, err := svc.cp.ListNodesPage(q)
							if err != nil {
								t.Fatalf("ListNodesPage: %v", err)
							}
							assertNodeListPagesEqual(t, got, want)
						})
					}
				}
			}
		}
	}
}

func TestListNodesPage_CreatedAtSortsChronologically(t *testing.T) {
	subMgr := topology.NewSubscriptionManager()
	pool := newNodeListTestPool(subMgr)
	sub := subscription.NewSubscription("sub-a", "sub-a", "https://example.com/a", true, false)
	subMgr.Register(sub)

	// RFC3339Nano drops trailing zeros, so these format as "...05Z",
	// "...05.1Z" and "...05.123Z", whose string order is the reverse of their
	// time order.
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	var chronological []string
	for i, offset := range []time.Duration{0, 100 * time.Millisecond, 123 * time.Millisecond} {
		raw := []byte(fmt.Sprintf(`{"type":"ss","server":"1.1.1.%d","port":443}`, i+1))
		hash := node.HashFromRawOptions(raw)
		entry := node.NewNodeEntry(hash, raw, base.Add(offset), 0)
		entry.AddSubscriptionID(sub.ID)
		pool.LoadNodeFromBootstrap(entry)
		sub.ManagedNodes().StoreNode(hash, subscription.ManagedNode{Tags: []string{"tag"}})
		chronological = append(chronological, hash.Hex())
	}

	cp := &ControlPlaneService{Pool: pool, SubMgr: subMgr, GeoIP: &geoip.Service{}}
	for _, sortOrder := range []string{"asc", "desc"} {
		page, err := cp.ListNodesPage(NodeListQuery{SortBy: "created_at", SortOrder: sortOrder, Limit: 10})
		if err != nil {
			t.Fatalf("ListNodesPage(%s): %v", sortOrder, err)
		}
		got := make([]string, 0, len(page.Items))
		for _, item := range page.Items {
			got = append(got, item.NodeHash)
		}
		want := slices.Clone(chronological)
		if sortOrder == "desc" {
			slices.Reverse(want)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("created_at %s order = %v, want %v", sortOrder, got, want)
		}
	}
}

func TestTopKSelector_KeepsSmallestInOrder(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	for _, n := range []int{0, 1, 2, 10, 257} {
		items := make([]int, n)
		for i := range items {
			items[i] = rng.IntN(50) // duplicates on purpose
		}
		all := slices.Sorted(slices.Values(items))
		for _, k := range []int{0, 1, 2, 3, n / 2, n - 1, n, n + 5} {
			if k < 0 {
				continue
			}
			selector := newTopKSelector(k, cmp.Compare[int])
			for _, item := range items {
				selector.offer(item)
			}
			if got, want := selector.sorted(), all[:min(k, n)]; !slices.Equal(got, want) {
				t.Fatalf("n=%d k=%d: got %v, want %v", n, k, got, want)
			}
		}
	}
}

// BenchmarkListNodesFirstPage compares the first page of the default tag
// order against summarizing and sorting the whole pool.
func BenchmarkListNodesFirstPage(b *testing.B) {
	subMgr := topology.NewSubscriptionManager()
	pool := newNodeListTestPool(subMgr)
	populateNodeListFixture(b, rand.New(rand.NewPCG(1, 2)), subMgr, 100_000, pool)
	cp := &ControlPlaneService{Pool: pool, SubMgr: subMgr, GeoIP: &geoip.Service{}}
	q := NodeListQuery{SortBy: "tag", SortOrder: "asc", Limit: 50}

	b.Run("page", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := cp.ListNodesPage(q); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("full_sort", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			referenceNodeListPage(b, cp, q)
		}
	})
}
