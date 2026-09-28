package api

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/Resinat/Resin/internal/testutil"
)

func TestAPIContract_SubscriptionListCountsNodesOfReturnedPage(t *testing.T) {
	srv, cp, _ := newControlPlaneTestServer(t)

	// Each subscription gets nodeCount nodes, and only its first node is healthy.
	type subSpec struct {
		name      string
		nodeCount int
	}
	subs := []subSpec{
		{name: "alpha-1", nodeCount: 1},
		{name: "alpha-2", nodeCount: 2},
		{name: "other-3", nodeCount: 3},
	}
	nextServer := 1
	for _, s := range subs {
		createRec := doJSONRequest(t, srv, http.MethodPost, "/api/v1/subscriptions", map[string]any{
			"name": s.name,
			"url":  "https://example.com/" + s.name,
		}, true)
		if createRec.Code != http.StatusCreated {
			t.Fatalf("create subscription %s status: got %d, want %d, body=%s", s.name, createRec.Code, http.StatusCreated, createRec.Body.String())
		}
		subID, _ := decodeJSONMap(t, createRec)["id"].(string)
		sub := cp.SubMgr.Lookup(subID)
		if sub == nil {
			t.Fatalf("subscription %s not found in manager", subID)
		}

		for i := 0; i < s.nodeCount; i++ {
			raw := []byte(fmt.Sprintf(`{"type":"ss","server":"10.0.0.%d","port":443}`, nextServer))
			nextServer++
			hash := node.HashFromRawOptions(raw)
			cp.Pool.AddNodeFromSub(hash, raw, subID)
			sub.ManagedNodes().StoreNode(hash, subscription.ManagedNode{Tags: []string{fmt.Sprintf("%s-%d", s.name, i)}})
			if i > 0 {
				continue
			}
			entry, ok := cp.Pool.GetEntry(hash)
			if !ok {
				t.Fatalf("missing node %s in pool", hash.Hex())
			}
			outbound := testutil.NewNoopOutbound()
			entry.Outbound.Store(&outbound)
			entry.CircuitOpenSince.Store(0)
		}
	}

	assertPage := func(path string, wantTotal int, wantItems []subSpec) {
		t.Helper()
		rec := doJSONRequest(t, srv, http.MethodGet, path, nil, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status: got %d, want %d, body=%s", path, rec.Code, http.StatusOK, rec.Body.String())
		}
		body := decodeJSONMap(t, rec)
		if body["total"] != float64(wantTotal) {
			t.Fatalf("GET %s total: got %v, want %d", path, body["total"], wantTotal)
		}
		items, ok := body["items"].([]any)
		if !ok {
			t.Fatalf("GET %s items type: got %T", path, body["items"])
		}
		if len(items) != len(wantItems) {
			t.Fatalf("GET %s items len: got %d, want %d, body=%s", path, len(items), len(wantItems), rec.Body.String())
		}
		for i, want := range wantItems {
			item, ok := items[i].(map[string]any)
			if !ok {
				t.Fatalf("GET %s item %d type: got %T", path, i, items[i])
			}
			if item["name"] != want.name {
				t.Fatalf("GET %s item %d name: got %v, want %q", path, i, item["name"], want.name)
			}
			if item["node_count"] != float64(want.nodeCount) {
				t.Fatalf("GET %s %s node_count: got %v, want %d", path, want.name, item["node_count"], want.nodeCount)
			}
			if item["healthy_node_count"] != float64(1) {
				t.Fatalf("GET %s %s healthy_node_count: got %v, want 1", path, want.name, item["healthy_node_count"])
			}
		}
	}

	assertPage("/api/v1/subscriptions?sort_by=name&sort_order=asc&limit=2&offset=1", 3, subs[1:])
	// Total covers every subscription matching the keyword, not just the page.
	assertPage("/api/v1/subscriptions?keyword=alpha&sort_by=name&sort_order=desc&limit=1", 2, subs[1:2])
}
