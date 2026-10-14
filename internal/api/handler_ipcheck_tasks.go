package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/Resinat/Resin/internal/ipcheck"
	"github.com/Resinat/Resin/internal/service"
	"golang.org/x/time/rate"
)

type ipCheckTaskRequest struct {
	RequestID  string   `json:"request_id"`
	NodeHashes []string `json:"node_hashes"`
	Sources    []string `json:"sources"`
	Force      bool     `json:"force"`
}

// Exact keys and no duplicates prevent ambiguous execution scope. Platform or
// subscription selectors are deliberately not accepted by this first slice.
func (b *ipCheckTaskRequest) UnmarshalJSON(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		return fmt.Errorf("request body must be a JSON object")
	}
	seen := map[string]bool{}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return err
		}
		name, ok := key.(string)
		if !ok || seen[name] {
			return fmt.Errorf("duplicate or invalid batch field")
		}
		seen[name] = true
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return err
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("batch fields must not be null")
		}
		switch name {
		case "request_id":
			err = json.Unmarshal(value, &b.RequestID)
		case "node_hashes":
			err = json.Unmarshal(value, &b.NodeHashes)
		case "sources":
			err = json.Unmarshal(value, &b.Sources)
		case "force":
			err = json.Unmarshal(value, &b.Force)
		default:
			return fmt.Errorf("unknown batch field")
		}
		if err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}

func ipCheckTaskTransport(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Cache-Control", "no-store")
	controller := http.NewResponseController(w)
	now := time.Now()
	for _, err := range []error{controller.SetReadDeadline(now.Add(5 * time.Second)), controller.SetWriteDeadline(now.Add(15 * time.Second))} {
		if err != nil && !errors.Is(err, http.ErrNotSupported) {
			WriteError(w, http.StatusInternalServerError, "INTERNAL", "batch task transport is unavailable")
			return false
		}
	}
	if r.Context().Err() != nil {
		WriteError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "batch task request was canceled")
		return false
	}
	return true
}

func writeIPCheckTask(w http.ResponseWriter, status int, task *ipcheck.BatchTask) {
	encoded, err := json.Marshal(task)
	if err != nil || len(encoded) > 512<<10 {
		WriteError(w, http.StatusInternalServerError, "INTERNAL", "batch task response exceeds its bound")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(append(encoded, '\n'))
}

// Separate endpoint instances reserve independent response slots, so polling
// cannot occupy the cancellation channel. Hold slots until writes finish.
func boundedIPCheckTaskHandler(handler http.HandlerFunc, limit rate.Limit, burst, concurrency int) http.HandlerFunc {
	limiter := rate.NewLimiter(limit, burst)
	active := make(chan struct{}, concurrency)
	return func(w http.ResponseWriter, r *http.Request) {
		if !ipCheckTaskTransport(w, r) {
			return
		}
		if !limiter.Allow() {
			writeIPCheckTaskLimited(w)
			return
		}
		select {
		case active <- struct{}{}:
			defer func() { <-active }()
		default:
			writeIPCheckTaskLimited(w)
			return
		}
		handler(w, r)
	}
}

func writeIPCheckTaskLimited(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	WriteError(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many IP quality task requests; retry later")
}

func HandleSubmitIPCheckBatch(cp *service.ControlPlaneService) http.HandlerFunc {
	return boundedIPCheckTaskHandler(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		var body *ipCheckTaskRequest
		if err := DecodeBody(r, &body); err != nil {
			var transportErr net.Error
			if r.Context().Err() != nil || (errors.As(err, &transportErr) && transportErr.Timeout()) {
				WriteError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "batch task request was canceled or timed out")
				return
			}
			writeDecodeBodyError(w, err)
			return
		}
		if body == nil {
			writeInvalidArgument(w, "request body must be a JSON object")
			return
		}
		task, reused, err := cp.SubmitIPCheckBatch(r.Context(), body.RequestID, body.NodeHashes, body.Sources, body.Force)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		status := http.StatusAccepted
		if reused {
			status = http.StatusOK
		}
		w.Header().Set("Location", "/api/v1/ip-checks/tasks/"+task.ID)
		writeIPCheckTask(w, status, task)
	}, 2, 2, 2)
}

func HandleGetIPCheckBatch(cp *service.ControlPlaneService) http.HandlerFunc {
	return boundedIPCheckTaskHandler(func(w http.ResponseWriter, r *http.Request) {
		task, err := cp.GetIPCheckBatch(r.Context(), PathParam(r, "id"))
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeIPCheckTask(w, http.StatusOK, task)
	}, 4, 4, 2)
}

func HandleCancelIPCheckBatch(cp *service.ControlPlaneService) http.HandlerFunc {
	return boundedIPCheckTaskHandler(func(w http.ResponseWriter, r *http.Request) {
		task, err := cp.CancelIPCheckBatch(r.Context(), PathParam(r, "id"))
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeIPCheckTask(w, http.StatusOK, task)
	}, 2, 4, 2)
}
