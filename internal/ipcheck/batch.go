package ipcheck

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Resinat/Resin/internal/node"
)

const (
	MaxBatchNodes       = 50
	MaxActiveBatchTasks = 4
	MaxBatchTaskRecords = 32
	batchWorkerCount    = 2
	batchTaskTimeout    = 30 * time.Minute
	batchRecordTTL      = 15 * time.Minute
)

var (
	ErrBatchNotFound = errors.New("batch task not found in the current plugin generation")
	ErrBatchConflict = errors.New("request_id was already used with a different batch")
	ErrBatchCapacity = errors.New("batch task capacity reached")
)

// BatchTask keeps bounded metadata, not raw source data or a historical cache
// snapshot. Completed items may contain entirely failed source observations.
type BatchTask struct {
	ID           string        `json:"id"`
	RequestID    string        `json:"request_id"`
	State        string        `json:"state"`
	Sources      []string      `json:"sources"`
	Force        bool          `json:"force"`
	CreatedAt    time.Time     `json:"created_at"`
	FinishedAt   *time.Time    `json:"finished_at,omitempty"`
	CancelReason string        `json:"cancel_reason,omitempty"`
	Progress     BatchProgress `json:"progress"`
	Items        []BatchItem   `json:"items"`
}

type BatchProgress struct {
	Total     int `json:"total"`
	Queued    int `json:"queued"`
	Running   int `json:"running"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
	Canceled  int `json:"canceled"`
}

type BatchItem struct {
	NodeHash          string             `json:"node_hash"`
	State             string             `json:"state"`
	StartedAt         *time.Time         `json:"started_at,omitempty"`
	FinishedAt        *time.Time         `json:"finished_at,omitempty"`
	EgressIP          string             `json:"egress_ip,omitempty"`
	CheckedAt         *time.Time         `json:"checked_at,omitempty"`
	ObservationStatus string             `json:"observation_status,omitempty"`
	Sources           []BatchSource      `json:"sources,omitempty"`
	SuppressedSources []SuppressedSource `json:"suppressed_sources,omitempty"`
	Error             *BatchError        `json:"error,omitempty"`
}

type BatchError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type BatchSource struct {
	Source    string     `json:"source"`
	Kind      SourceKind `json:"kind"`
	OK        bool       `json:"ok"`
	Status    string     `json:"status"`
	CheckedAt time.Time  `json:"checked_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	Error     string     `json:"error,omitempty"`
}

type batchJob struct {
	task     BatchTask
	hashes   []node.Hash
	identity string
	ctx      context.Context
	cancel   context.CancelFunc
}

type batchTasks struct {
	engine  *Engine
	mu      sync.Mutex
	closed  bool
	jobs    []*batchJob
	wake    chan struct{}
	workers sync.WaitGroup
	now     func() time.Time
}

func newBatchTasks(e *Engine) *batchTasks {
	b := &batchTasks{engine: e, wake: make(chan struct{}, batchWorkerCount), now: time.Now}
	b.workers.Add(batchWorkerCount)
	for range batchWorkerCount {
		go b.worker()
	}
	return b
}

func validBatchRequestID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// SubmitBatch atomically accepts the whole bounded selection. The caller's
// lifetime controls admission only; accepted jobs belong to this engine.
func (e *Engine) SubmitBatch(ctx context.Context, requestID string, hashes []node.Hash, names []string, force bool) (*BatchTask, bool, error) {
	if !validBatchRequestID(requestID) {
		return nil, false, invalid("request_id: use 1 to 64 ASCII letters, digits, '-' or '_'")
	}
	if len(hashes) == 0 || len(hashes) > MaxBatchNodes {
		return nil, false, invalid("node_hashes: must contain 1 to 50 node hashes")
	}
	if len(names) == 0 {
		return nil, false, invalid("sources: explicit nonempty selection is required")
	}
	selected, err := e.selectSources(names, false)
	if err != nil {
		return nil, false, err
	}
	seen := make(map[node.Hash]bool, len(hashes))
	unique := make([]node.Hash, 0, len(hashes))
	canonical := make([]string, 0, len(hashes))
	for _, hash := range hashes {
		if !seen[hash] {
			seen[hash] = true
			unique = append(unique, hash)
			canonical = append(canonical, hash.Hex())
		}
	}
	sort.Strings(canonical)
	sort.Strings(selected)
	identity := strings.Join(canonical, ",") + "|" + strings.Join(selected, ",") + "|" + strconv.FormatBool(force)
	b := e.batches
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if b.closed || e.ctx.Err() != nil {
		return nil, false, ErrClosed
	}
	b.pruneLocked()
	active := 0
	for _, job := range b.jobs {
		b.refreshLocked(job)
		if job.task.RequestID == requestID {
			if job.identity != identity {
				return nil, false, ErrBatchConflict
			}
			return b.snapshotLocked(job), true, nil
		}
		if job.task.FinishedAt == nil {
			active++
		}
	}
	if active >= MaxActiveBatchTasks {
		return nil, false, ErrBatchCapacity
	}
	if len(b.jobs) >= MaxBatchTaskRecords {
		oldest := -1
		for i, job := range b.jobs {
			if job.task.FinishedAt != nil && (oldest < 0 || job.task.FinishedAt.Before(*b.jobs[oldest].task.FinishedAt)) {
				oldest = i
			}
		}
		if oldest < 0 {
			return nil, false, ErrBatchCapacity
		}
		b.removeLocked(oldest)
	}
	var randomID [16]byte
	if _, err := rand.Read(randomID[:]); err != nil {
		return nil, false, err
	}
	jobCtx, cancel := context.WithTimeout(e.ctx, batchTaskTimeout)
	job := &batchJob{task: BatchTask{ID: hex.EncodeToString(randomID[:]), RequestID: requestID, State: "queued", Sources: selected, Force: force, CreatedAt: b.now(), Items: make([]BatchItem, len(unique))}, hashes: unique, identity: identity, ctx: jobCtx, cancel: cancel}
	for i, hash := range unique {
		job.task.Items[i] = BatchItem{NodeHash: hash.Hex(), State: "queued"}
	}
	b.jobs = append(b.jobs, job)
	for range batchWorkerCount {
		select {
		case b.wake <- struct{}{}:
		default:
		}
	}
	return b.snapshotLocked(job), false, nil
}

func (e *Engine) GetBatch(ctx context.Context, id string) (*BatchTask, error) {
	b := e.batches
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if b.closed || e.ctx.Err() != nil {
		return nil, ErrClosed
	}
	b.pruneLocked()
	for _, job := range b.jobs {
		if job.task.ID == id {
			b.refreshLocked(job)
			return b.snapshotLocked(job), nil
		}
	}
	return nil, ErrBatchNotFound
}

func (e *Engine) CancelBatch(ctx context.Context, id string) (*BatchTask, error) {
	b := e.batches
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if b.closed || e.ctx.Err() != nil {
		return nil, ErrClosed
	}
	b.pruneLocked()
	for _, job := range b.jobs {
		if job.task.ID == id {
			b.refreshLocked(job)
			if job.task.FinishedAt == nil {
				b.cancelLocked(job, "requested")
			}
			return b.snapshotLocked(job), nil
		}
	}
	return nil, ErrBatchNotFound
}

func (b *batchTasks) removeLocked(i int) {
	b.jobs[i].cancel()
	copy(b.jobs[i:], b.jobs[i+1:])
	b.jobs[len(b.jobs)-1] = nil
	b.jobs = b.jobs[:len(b.jobs)-1]
}

func (b *batchTasks) pruneLocked() {
	now := b.now()
	for i := len(b.jobs) - 1; i >= 0; i-- {
		if end := b.jobs[i].task.FinishedAt; end != nil && !now.Before(end.Add(batchRecordTTL)) {
			b.removeLocked(i)
		}
	}
}

func (b *batchTasks) cancelLocked(job *batchJob, reason string) {
	if job.task.FinishedAt != nil {
		return
	}
	if job.task.CancelReason == "" {
		job.task.CancelReason = reason
	}
	job.cancel()
	for i := range job.task.Items {
		if job.task.Items[i].State == "queued" {
			job.task.Items[i].State = "canceled"
			job.task.Items[i].FinishedAt = timePointer(b.now())
		}
	}
	b.finishLocked(job)
}

func (b *batchTasks) refreshLocked(job *batchJob) {
	if job.task.FinishedAt == nil && job.ctx.Err() != nil {
		reason := "engine_stopped"
		if errors.Is(job.ctx.Err(), context.DeadlineExceeded) {
			reason = "deadline"
		}
		b.cancelLocked(job, reason)
	}
}

func (b *batchTasks) finishLocked(job *batchJob) {
	if job.task.FinishedAt != nil {
		return
	}
	queued, running := 0, 0
	for _, item := range job.task.Items {
		if item.State == "queued" {
			queued++
		}
		if item.State == "running" {
			running++
		}
	}
	if queued+running == 0 {
		job.task.State = "completed"
		if job.task.CancelReason != "" {
			job.task.State = "canceled"
		}
		job.task.FinishedAt = timePointer(b.now())
		job.cancel()
	} else if job.task.CancelReason != "" {
		job.task.State = "canceling"
	} else if running > 0 {
		job.task.State = "running"
	}
}

func (b *batchTasks) claim() (*batchJob, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.engine.ctx.Err() != nil {
		return nil, 0
	}
	for _, job := range b.jobs {
		b.refreshLocked(job)
		if job.task.FinishedAt != nil || job.task.CancelReason != "" {
			continue
		}
		for i := range job.task.Items {
			if job.task.Items[i].State == "queued" {
				job.task.Items[i].State = "running"
				job.task.Items[i].StartedAt = timePointer(b.now())
				job.task.State = "running"
				return job, i
			}
		}
	}
	return nil, 0
}

func (b *batchTasks) worker() {
	defer b.workers.Done()
	for {
		job, index := b.claim()
		if job == nil {
			select {
			case <-b.engine.ctx.Done():
				return
			case <-b.wake:
				continue
			}
		}
		var result *Result
		err := job.ctx.Err() // Cancellation can win after the claim but before work.
		if err == nil {
			result, err = b.engine.CheckNode(job.ctx, job.hashes[index], job.task.Sources, job.task.Force)
		}
		b.mu.Lock()
		b.refreshLocked(job)
		item := &job.task.Items[index]
		item.FinishedAt = timePointer(b.now())
		if job.task.CancelReason != "" {
			item.State = "canceled"
		} else if err != nil || result == nil {
			item.State = "failed"
			item.Error = batchItemError(err)
		} else {
			item.State = "completed"
			item.EgressIP = result.EgressIP
			if !result.CheckedAt.IsZero() {
				item.CheckedAt = timePointer(result.CheckedAt)
			}
			for _, source := range result.SuppressedSources {
				item.SuppressedSources = append(item.SuppressedSources, cloneSuppressed(source))
			}
			item.ObservationStatus = "all_failed"
			ok, unavailable := 0, 0
			for _, source := range result.Sources {
				item.Sources = append(item.Sources, BatchSource{Source: source.Source, Kind: source.Kind, OK: source.OK, Status: source.Status, CheckedAt: source.CheckedAt, ExpiresAt: source.ExpiresAt, Error: boundedBatchError(source.Error)})
				if source.OK && source.Status == "ok" {
					ok++
				}
				if source.Status == "unavailable" {
					unavailable++
				}
			}
			switch {
			case len(result.SuppressedSources) > 0 && len(result.Sources) == 0:
				item.ObservationStatus = "suppressed"
			case len(result.SuppressedSources) > 0:
				item.ObservationStatus = "partial_suppression"
			case len(result.Sources) == 0:
				item.ObservationStatus = "no_observations"
			case ok == len(result.Sources):
				item.ObservationStatus = "all_ok"
			case ok > 0:
				item.ObservationStatus = "partial_failure"
			case unavailable == len(result.Sources):
				item.ObservationStatus = "unavailable"
			}
		}
		b.finishLocked(job)
		b.mu.Unlock()
	}
}

func (b *batchTasks) close() {
	b.mu.Lock()
	b.closed = true
	for _, job := range b.jobs {
		b.cancelLocked(job, "engine_stopped")
	}
	b.mu.Unlock()
	// Neither the batch mutex nor Engine.mu is held while workers stop. Workers
	// call this fixed engine directly and never re-enter the plugin manager.
	b.workers.Wait()
}

func (b *batchTasks) snapshotLocked(job *batchJob) *BatchTask {
	task := job.task
	task.Sources = append([]string{}, job.task.Sources...)
	task.FinishedAt = copyTime(job.task.FinishedAt)
	task.Items = make([]BatchItem, len(job.task.Items))
	task.Progress = BatchProgress{Total: len(task.Items)}
	for i, item := range job.task.Items {
		task.Items[i] = item
		task.Items[i].StartedAt, task.Items[i].FinishedAt, task.Items[i].CheckedAt = copyTime(item.StartedAt), copyTime(item.FinishedAt), copyTime(item.CheckedAt)
		task.Items[i].Sources = append([]BatchSource(nil), item.Sources...)
		task.Items[i].SuppressedSources = make([]SuppressedSource, len(item.SuppressedSources))
		for j, source := range item.SuppressedSources {
			task.Items[i].SuppressedSources[j] = cloneSuppressed(source)
		}
		if item.Error != nil {
			copy := *item.Error
			task.Items[i].Error = &copy
		}
		switch item.State {
		case "queued":
			task.Progress.Queued++
		case "running":
			task.Progress.Running++
		case "completed":
			task.Progress.Completed++
		case "failed":
			task.Progress.Failed++
		case "canceled":
			task.Progress.Canceled++
		}
	}
	return &task
}

func timePointer(value time.Time) *time.Time { return &value }
func copyTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	return timePointer(*value)
}
func boundedBatchError(value string) string {
	if len(value) > 512 {
		value = value[:512]
	}
	return strings.ToValidUTF8(value, "?")
}
func batchItemError(err error) *BatchError {
	switch {
	case errors.Is(err, ErrNodeNotFound):
		return &BatchError{Code: "NOT_FOUND", Message: "node not found"}
	case errors.Is(err, ErrNoEgressIP):
		return &BatchError{Code: "CONFLICT", Message: "node has no public egress IP"}
	case errors.Is(err, ErrNodeChanged):
		return &BatchError{Code: "CONFLICT", Message: "node egress changed during check"}
	case errors.Is(err, ErrBusy):
		return &BatchError{Code: "SERVICE_UNAVAILABLE", Message: "IP quality checks are busy"}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), errors.Is(err, ErrClosed):
		return &BatchError{Code: "SERVICE_UNAVAILABLE", Message: "check canceled or timed out"}
	default:
		return &BatchError{Code: "INTERNAL", Message: "IP quality check failed"}
	}
}
