package service

import (
	"bytes"
	"cmp"
	"math"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/probe"
	"github.com/Resinat/Resin/internal/subscription"
)

// ------------------------------------------------------------------
// Nodes
// ------------------------------------------------------------------

// NodeFilters holds query filters for listing nodes.
type NodeFilters struct {
	PlatformID     *string
	SubscriptionID *string
	Enabled        *bool
	Region         *string
	CircuitOpen    *bool
	HasOutbound    *bool
	EgressIP       *string
	ProbedSince    *time.Time
	TagKeyword     *string
}

// NodeListQuery selects one sorted page of the node list.
type NodeListQuery struct {
	Filters NodeFilters
	// SortBy is one of "tag" (default), "created_at", "failure_count", "region".
	SortBy string
	// SortOrder is "asc" (default) or "desc".
	SortOrder string
	Offset    int
	Limit     int
}

// NodeListPage is one page of the node list. Total and the unique egress IP
// counts cover the whole filtered result, not just Items.
type NodeListPage struct {
	Items                  []NodeSummary
	Total                  int
	UniqueEgressIPs        int
	UniqueHealthyEgressIPs int
}

// ListNodes returns nodes from the pool with optional filters.
func (s *ControlPlaneService) ListNodes(filters NodeFilters) ([]NodeSummary, error) {
	result := []NodeSummary{}
	err := s.rangeFilteredNodes(filters, func(h node.Hash, entry *node.NodeEntry) {
		result = append(result, s.nodeEntryToSummary(h, entry))
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ListNodesPage returns one sorted page of nodes matching q.Filters, plus
// aggregates over the whole filtered result.
//
// Only the nodes inside the page are converted to NodeSummary. The filtered
// scan keeps a light record per node and a bounded heap retains the first
// Offset+Limit of them, so a large pool is neither fully summarized nor fully
// sorted. Ties on the sort key are broken by node hash, which makes the order
// total and pages stable.
func (s *ControlPlaneService) ListNodesPage(q NodeListQuery) (*NodeListPage, error) {
	var subLookup node.SubLookupFunc
	if s.Pool != nil {
		subLookup = s.Pool.MakeSubLookup()
	}

	offset := max(q.Offset, 0)
	windowEnd := 0
	if q.Limit > 0 {
		windowEnd = math.MaxInt
		if offset <= math.MaxInt-q.Limit {
			windowEnd = offset + q.Limit
		}
	}
	candidateCmp := compareNodeListCandidates
	if q.SortOrder == "desc" {
		candidateCmp = func(a, b nodeListCandidate) int { return compareNodeListCandidates(b, a) }
	}
	selector := newTopKSelector(windowEnd, candidateCmp)

	total := 0
	egressIPs := make(map[netip.Addr]struct{})
	healthyEgressIPs := make(map[netip.Addr]struct{})
	err := s.rangeFilteredNodes(q.Filters, func(h node.Hash, entry *node.NodeEntry) {
		total++
		if ip := entry.GetEgressIP(); ip.IsValid() {
			egressIPs[ip] = struct{}{}
			if _, seen := healthyEgressIPs[ip]; !seen && nodeEntryHealthyAndEnabled(entry, subLookup) {
				healthyEgressIPs[ip] = struct{}{}
			}
		}
		if windowEnd > 0 {
			selector.offer(s.newNodeListCandidate(h, entry, q.SortBy))
		}
	})
	if err != nil {
		return nil, err
	}

	window := selector.sorted()
	if offset < len(window) {
		window = window[offset:]
	} else {
		window = nil
	}
	items := make([]NodeSummary, 0, len(window))
	for _, c := range window {
		items = append(items, s.nodeEntryToSummary(c.hash, c.entry))
	}
	return &NodeListPage{
		Items:                  items,
		Total:                  total,
		UniqueEgressIPs:        len(egressIPs),
		UniqueHealthyEgressIPs: len(healthyEgressIPs),
	}, nil
}

// nodeListCandidate is the light per-node record the node list is sorted by
// before any NodeSummary is built. Only the key of the requested sort field is
// set and the other stays zero, so one comparator serves every field.
type nodeListCandidate struct {
	hash   node.Hash
	entry  *node.NodeEntry
	numKey int64
	strKey string
}

// compareNodeListCandidates orders candidates by sort key, then by node hash.
// Byte order of the hash matches the order of its lowercase hex form.
func compareNodeListCandidates(a, b nodeListCandidate) int {
	if c := cmp.Compare(a.numKey, b.numKey); c != 0 {
		return c
	}
	if c := strings.Compare(a.strKey, b.strKey); c != 0 {
		return c
	}
	return bytes.Compare(a.hash[:], b.hash[:])
}

// newNodeListCandidate computes the sort key NodeSummary would expose for
// sortBy, without building the summary.
func (s *ControlPlaneService) newNodeListCandidate(h node.Hash, entry *node.NodeEntry, sortBy string) nodeListCandidate {
	c := nodeListCandidate{hash: h, entry: entry}
	switch sortBy {
	case "created_at":
		c.numKey = entry.CreatedAt.UnixNano()
	case "failure_count":
		c.numKey = int64(entry.FailureCount.Load())
	case "region":
		// NodeSummary only reports a region for nodes with a known egress IP.
		if entry.GetEgressIP().IsValid() {
			if s.GeoIP != nil {
				c.strKey = entry.GetRegion(s.GeoIP.Lookup)
			} else {
				c.strKey = entry.GetRegion(nil)
			}
		}
	default:
		c.strKey = s.nodeEntryTagSortKey(h, entry)
	}
	return c
}

// nodeEntryTagSortKey returns the node's display tag or, when none resolves,
// the "<SubscriptionName>/<Tag>" of the earliest-created subscription that
// lists the node, preferring the smaller tag on ties.
func (s *ControlPlaneService) nodeEntryTagSortKey(h node.Hash, entry *node.NodeEntry) string {
	if s.Pool != nil {
		if tag := s.Pool.ResolveNodeDisplayTag(h); tag != "" {
			return tag
		}
	}
	found := false
	var bestCreatedAtNs int64
	bestTag := ""
	for _, subID := range entry.SubscriptionIDs() {
		sub := s.SubMgr.Lookup(subID)
		if sub == nil {
			continue
		}
		managed, ok := sub.ManagedNodes().LoadNode(h)
		if !ok || len(managed.Tags) == 0 {
			continue
		}
		tag := sub.Name() + "/" + slices.Min(managed.Tags)
		if !found ||
			sub.CreatedAtNs < bestCreatedAtNs ||
			(sub.CreatedAtNs == bestCreatedAtNs && tag < bestTag) {
			found = true
			bestCreatedAtNs = sub.CreatedAtNs
			bestTag = tag
		}
	}
	return bestTag
}

// nodeEntryHealthyAndEnabled matches NodeSummary.IsHealthyAndEnabled for the
// entry without building the summary.
func nodeEntryHealthyAndEnabled(entry *node.NodeEntry, subLookup node.SubLookupFunc) bool {
	if !entry.HasOutbound() || entry.CircuitOpenSince.Load() > 0 {
		return false
	}
	return subLookup == nil || entry.HasEnabledSubscription(subLookup)
}

// topKSelector keeps the k smallest items offered to it under cmp, which must
// be a strict total order. It selects a page near the front of a large result
// in O(n log k) time and O(k) memory instead of sorting every item.
type topKSelector[T any] struct {
	k    int
	cmp  func(a, b T) int
	heap []T // max-heap under cmp
}

func newTopKSelector[T any](k int, cmp func(a, b T) int) *topKSelector[T] {
	return &topKSelector[T]{k: k, cmp: cmp}
}

func (t *topKSelector[T]) offer(item T) {
	if len(t.heap) < t.k {
		t.heap = append(t.heap, item)
		t.siftUp(len(t.heap) - 1)
		return
	}
	if len(t.heap) == 0 || t.cmp(item, t.heap[0]) >= 0 {
		return
	}
	t.heap[0] = item
	t.siftDown(0)
}

// sorted returns the kept items in ascending order. The selector must not be
// offered more items afterwards.
func (t *topKSelector[T]) sorted() []T {
	slices.SortFunc(t.heap, t.cmp)
	return t.heap
}

func (t *topKSelector[T]) siftUp(i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if t.cmp(t.heap[i], t.heap[parent]) <= 0 {
			return
		}
		t.heap[i], t.heap[parent] = t.heap[parent], t.heap[i]
		i = parent
	}
}

func (t *topKSelector[T]) siftDown(i int) {
	for {
		largest := i
		if l := 2*i + 1; l < len(t.heap) && t.cmp(t.heap[l], t.heap[largest]) > 0 {
			largest = l
		}
		if r := 2*i + 2; r < len(t.heap) && t.cmp(t.heap[r], t.heap[largest]) > 0 {
			largest = r
		}
		if largest == i {
			return
		}
		t.heap[i], t.heap[largest] = t.heap[largest], t.heap[i]
		i = largest
	}
}

// rangeFilteredNodes calls fn for every pool node that matches filters.
func (s *ControlPlaneService) rangeFilteredNodes(
	filters NodeFilters,
	fn func(h node.Hash, entry *node.NodeEntry),
) error {
	var subLookup node.SubLookupFunc
	if s != nil && s.Pool != nil {
		subLookup = s.Pool.MakeSubLookup()
	}

	// If platform_id filter, get the platform view.
	var platformView map[node.Hash]struct{}
	if filters.PlatformID != nil {
		plat, ok := s.Pool.GetPlatform(*filters.PlatformID)
		if !ok {
			return notFound("platform not found")
		}
		platformView = make(map[node.Hash]struct{}, plat.View().Size())
		plat.View().Range(func(h node.Hash) bool {
			platformView[h] = struct{}{}
			return true
		})
	}

	var subNodes map[node.Hash]struct{}
	if filters.SubscriptionID != nil {
		sub := s.SubMgr.Lookup(*filters.SubscriptionID)
		if sub == nil {
			return notFound("subscription not found")
		}
		subNodes = make(map[node.Hash]struct{})
		sub.ManagedNodes().RangeNodes(func(h node.Hash, managed subscription.ManagedNode) bool {
			if managed.Evicted {
				return true
			}
			subNodes[h] = struct{}{}
			return true
		})
	}

	visitIfMatched := func(h node.Hash, entry *node.NodeEntry) {
		if !s.nodeEntryMatchesFilters(entry, filters, subLookup) {
			return
		}
		fn(h, entry)
	}

	visitIfMatchedHash := func(h node.Hash) {
		entry, ok := s.Pool.GetEntry(h)
		if !ok {
			return
		}
		visitIfMatched(h, entry)
	}

	switch {
	case platformView != nil && subNodes != nil:
		// Iterate the smaller candidate set, then intersect by membership.
		if len(platformView) <= len(subNodes) {
			for h := range platformView {
				if _, ok := subNodes[h]; !ok {
					continue
				}
				visitIfMatchedHash(h)
			}
		} else {
			for h := range subNodes {
				if _, ok := platformView[h]; !ok {
					continue
				}
				visitIfMatchedHash(h)
			}
		}
	case platformView != nil:
		for h := range platformView {
			visitIfMatchedHash(h)
		}
	case subNodes != nil:
		for h := range subNodes {
			visitIfMatchedHash(h)
		}
	default:
		s.Pool.Range(func(h node.Hash, entry *node.NodeEntry) bool {
			visitIfMatched(h, entry)
			return true
		})
	}
	return nil
}

func (s *ControlPlaneService) nodeEntryMatchesFilters(
	entry *node.NodeEntry,
	filters NodeFilters,
	subLookup node.SubLookupFunc,
) bool {
	// Enabled/disabled filter.
	if filters.Enabled != nil {
		enabled := true
		if subLookup != nil {
			enabled = entry.HasEnabledSubscription(subLookup)
		}
		if enabled != *filters.Enabled {
			return false
		}
	}

	// Node tag fuzzy search filter.
	if filters.TagKeyword != nil {
		keyword := strings.ToLower(strings.TrimSpace(*filters.TagKeyword))
		if keyword != "" {
			matched := false
			for _, subID := range entry.SubscriptionIDs() {
				sub := s.SubMgr.Lookup(subID)
				if sub == nil {
					continue
				}
				managed, ok := sub.ManagedNodes().LoadNode(entry.Hash)
				if !ok {
					continue
				}
				tags := managed.Tags
				for _, tag := range tags {
					displayTag := sub.Name() + "/" + tag
					if strings.Contains(strings.ToLower(displayTag), keyword) {
						matched = true
						break
					}
				}
				if matched {
					break
				}
			}
			if !matched {
				return false
			}
		}
	}

	// Region filter.
	if filters.Region != nil {
		region := entry.GetRegion(nil)
		if s.GeoIP != nil {
			region = entry.GetRegion(s.GeoIP.Lookup)
		}
		if region == "" || region != *filters.Region {
			return false
		}
	}
	// Circuit open filter.
	if filters.CircuitOpen != nil {
		if entry.IsCircuitOpen() != *filters.CircuitOpen {
			return false
		}
	}
	// Has outbound filter.
	if filters.HasOutbound != nil {
		if entry.HasOutbound() != *filters.HasOutbound {
			return false
		}
	}
	// Egress IP filter.
	if filters.EgressIP != nil {
		egressIP := entry.GetEgressIP()
		if !egressIP.IsValid() || egressIP.String() != *filters.EgressIP {
			return false
		}
	}
	// Probed since filter.
	if filters.ProbedSince != nil {
		lastUpdate := entry.LastLatencyProbeAttempt.Load()
		if lastUpdate < filters.ProbedSince.UnixNano() {
			return false
		}
	}
	return true
}

// GetNode returns a single node by hash.
func (s *ControlPlaneService) GetNode(hashStr string) (*NodeSummary, error) {
	h, err := node.ParseHex(hashStr)
	if err != nil {
		return nil, invalidArg("node_hash: invalid format")
	}
	entry, ok := s.Pool.GetEntry(h)
	if !ok {
		return nil, notFound("node not found")
	}
	ns := s.nodeEntryToSummary(h, entry)
	return &ns, nil
}

// ProbeEgress triggers a synchronous egress probe and returns results.
func (s *ControlPlaneService) ProbeEgress(hashStr string) (*probe.EgressProbeResult, error) {
	h, err := node.ParseHex(hashStr)
	if err != nil {
		return nil, invalidArg("node_hash: invalid format")
	}
	entry, ok := s.Pool.GetEntry(h)
	if !ok {
		return nil, notFound("node not found")
	}
	result, err := s.ProbeMgr.ProbeEgressSync(h)
	if err != nil {
		return nil, internal("egress probe failed", err)
	}
	result.Region = entry.GetRegion(nil)
	if s.GeoIP != nil {
		result.Region = entry.GetRegion(s.GeoIP.Lookup)
	}
	return result, nil
}

// ProbeLatency triggers a synchronous latency probe and returns results.
func (s *ControlPlaneService) ProbeLatency(hashStr string) (*probe.LatencyProbeResult, error) {
	h, err := node.ParseHex(hashStr)
	if err != nil {
		return nil, invalidArg("node_hash: invalid format")
	}
	if _, ok := s.Pool.GetEntry(h); !ok {
		return nil, notFound("node not found")
	}
	result, err := s.ProbeMgr.ProbeLatencySync(h)
	if err != nil {
		return nil, internal("latency probe failed", err)
	}
	return result, nil
}
