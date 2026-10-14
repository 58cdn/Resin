package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/Resinat/Resin/internal/ipcheck"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/service"
	"golang.org/x/time/rate"
)

const (
	ipCheckBatchMaxNodes       = 50
	ipCheckBatchMaxBodyBytes   = 16 << 10
	ipCheckBatchMaxResultBytes = 64 << 10
	ipCheckBatchConcurrency    = 2
	ipCheckBatchTimeout        = 5 * time.Second
)

type ipCheckBatchRequest struct {
	NodeHashes []string `json:"node_hashes"`
}

// Keep this endpoint's whole-selection validation unambiguous: encoding/json's
// default last-key-wins and case-insensitive field matching are not suitable.
func (b *ipCheckBatchRequest) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return fmt.Errorf("request body must be a JSON object")
	}
	seen := false
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil || key != "node_hashes" || seen {
			return fmt.Errorf("request must contain only one exact node_hashes field")
		}
		seen = true
		if err := decoder.Decode(&b.NodeHashes); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

type ipCheckBatchItem struct {
	NodeHash string          `json:"node_hash"`
	Result   json.RawMessage `json:"result,omitempty"`
	Error    *ErrorDetail    `json:"error,omitempty"`
}

type ipCheckBatchResponse struct {
	Items []ipCheckBatchItem `json:"items"`
	Total int                `json:"total"`
}

type nodeIPCheckCacheReader interface {
	GetNodeIPCheck(context.Context, string) (*ipcheck.Result, error)
}

// HandleGetNodeIPCheckBatch handles POST /api/v1/ip-checks/cache/nodes.
// POST carries a bounded selection; this endpoint can only read existing cache.
// Admission limits belong to this handler instance, shared by all API clients.
func HandleGetNodeIPCheckBatch(cp *service.ControlPlaneService) http.HandlerFunc {
	return newNodeIPCheckBatchHandler(cp, rate.NewLimiter(2, 2))
}

func newNodeIPCheckBatchHandler(reader nodeIPCheckCacheReader, limiter *rate.Limiter) http.HandlerFunc {
	active := make(chan struct{}, ipCheckBatchConcurrency)
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		// Endpoint-local transport deadlines also bound slow request bodies and
		// response readers while an admission slot is held. Native net/http
		// writers support them; in-memory test recorders have no transport.
		controller := http.NewResponseController(w)
		now := time.Now()
		for _, err := range []error{
			controller.SetReadDeadline(now.Add(ipCheckBatchTimeout)),
			controller.SetWriteDeadline(now.Add(3 * ipCheckBatchTimeout)),
		} {
			if err != nil && !errors.Is(err, http.ErrNotSupported) {
				WriteError(w, http.StatusInternalServerError, "INTERNAL", "IP quality batch transport is unavailable")
				return
			}
		}
		// Do not clear these before net/http flushes the response. The server
		// resets connection deadlines when it proceeds to the next request.
		if r.Context().Err() != nil {
			writeIPCheckBatchCanceled(w)
			return
		}
		// Never queue work or create a worker goroutine for an overloaded request.
		if !limiter.Allow() {
			writeIPCheckBatchLimited(w)
			return
		}
		select {
		case active <- struct{}{}:
			defer func() { <-active }()
		default:
			writeIPCheckBatchLimited(w)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, ipCheckBatchMaxBodyBytes)
		var body *ipCheckBatchRequest
		if err := DecodeBody(r, &body); err != nil {
			var transportErr net.Error
			if r.Context().Err() != nil || (errors.As(err, &transportErr) && transportErr.Timeout()) {
				writeIPCheckBatchCanceled(w)
				return
			}
			writeDecodeBodyError(w, err)
			return
		}
		if body == nil || len(body.NodeHashes) == 0 || len(body.NodeHashes) > ipCheckBatchMaxNodes {
			writeInvalidArgument(w, "node_hashes: must contain 1 to 50 node hashes")
			return
		}
		// Validate the entire selection before reading any node; the limit applies
		// before deduplication so repeated entries cannot evade the request bound.
		hashes := make([]string, 0, len(body.NodeHashes))
		seen := make(map[node.Hash]bool, len(body.NodeHashes))
		for _, value := range body.NodeHashes {
			hash, err := node.ParseHex(value)
			if err != nil {
				writeInvalidArgument(w, "node_hashes: invalid node hash")
				return
			}
			if !seen[hash] {
				seen[hash] = true
				hashes = append(hashes, hash.Hex())
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), ipCheckBatchTimeout)
		defer cancel()
		response := ipCheckBatchResponse{Items: make([]ipCheckBatchItem, 0, len(hashes)), Total: len(hashes)}
		for _, hash := range hashes {
			if ctx.Err() != nil {
				writeIPCheckBatchCanceled(w)
				return
			}
			// Reads are sequential. The service resolves the current plugin on each
			// call and enforces source TTL/configuration and node egress versions.
			result, err := reader.GetNodeIPCheck(ctx, hash)
			if ctx.Err() != nil {
				writeIPCheckBatchCanceled(w)
				return
			}
			item := ipCheckBatchItem{NodeHash: hash}
			if err != nil {
				var svcErr *service.ServiceError
				if errors.As(err, &svcErr) {
					switch svcErr.Code {
					case "SERVICE_UNAVAILABLE":
						// Do not present an interrupted batch as a completed partial read.
						WriteError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "IP quality cache is disabled or unavailable; retry later")
						return
					case "NOT_FOUND", "CONFLICT":
						item.Error = &ErrorDetail{Code: svcErr.Code, Message: svcErr.Message}
					}
				}
				if item.Error == nil {
					item.Error = &ErrorDetail{Code: "INTERNAL", Message: "IP quality cache read failed"}
				}
			} else if result == nil {
				item.Error = &ErrorDetail{Code: "INTERNAL", Message: "IP quality cache read failed"}
			} else {
				data, encodeErr := json.Marshal(publicIPCheckResult(result))
				switch {
				case encodeErr != nil:
					item.Error = &ErrorDetail{Code: "INTERNAL", Message: "IP quality cache result could not be encoded"}
				case len(data) > ipCheckBatchMaxResultBytes:
					// Keep the batch bounded without silently truncating source evidence.
					item.Error = &ErrorDetail{Code: "RESULT_TOO_LARGE", Message: "cached result exceeds 64 KiB; use the single-node cache endpoint"}
				default:
					item.Result = data
				}
			}
			response.Items = append(response.Items, item)
		}
		if ctx.Err() != nil {
			writeIPCheckBatchCanceled(w)
			return
		}
		WriteJSON(w, http.StatusOK, response)
	}
}

func writeIPCheckBatchLimited(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	WriteError(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many IP quality cache batches; retry later")
}

func writeIPCheckBatchCanceled(w http.ResponseWriter) {
	WriteError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "IP quality cache batch was canceled or timed out")
}
