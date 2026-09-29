package plugin

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Resinat/Resin/pkg/pluginsdk"
)

const (
	// maxEventBatchBytes caps the encoded size of one batch, well below the
	// protocol line limit of package plugins.
	maxEventBatchBytes = 4 << 20
	// eventOverheadBytes approximates the encoding of an event besides its
	// data (type and time).
	eventOverheadBytes = 64
	// eventStopTimeout bounds the final flush when a plugin is stopped.
	eventStopTimeout = 3 * time.Second
	// eventStopGrace is how long a cancelled delivery gets to return.
	eventStopGrace = time.Second
)

// lazyEvent is shared by every subscriber of one event. The payload is
// marshaled at most once, by the first delivery worker that needs it, so the
// producer (proxy hot path / lease callbacks) only pays for an allocation.
type lazyEvent struct {
	typ   string
	at    time.Time
	build func() any

	once sync.Once
	data json.RawMessage
}

func newLazyEvent(typ string, at time.Time, build func() any) *lazyEvent {
	return &lazyEvent{typ: typ, at: at, build: build}
}

func (e *lazyEvent) event() pluginsdk.Event {
	e.once.Do(func() {
		data, err := json.Marshal(e.build())
		if err != nil {
			data = json.RawMessage(`{}`)
		}
		e.data = data
		e.build = nil
	})
	return pluginsdk.Event{Type: e.typ, Time: e.at, Data: e.data}
}

// eventQueue is a bounded per-plugin queue with a batching delivery worker.
// Pushes never block; when the queue is full the event is dropped and counted.
type eventQueue struct {
	ch        chan *lazyEvent
	batchSize int
	flush     time.Duration
	deliver   func(ctx context.Context, events []pluginsdk.Event) error
	onResult  func(delivered int, err error)

	// ctx is cancelled when stop gives up on the final flush. It aborts the
	// delivery in progress; everything not delivered yet is dropped.
	ctx    context.Context
	cancel context.CancelFunc

	dropped atomic.Int64

	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
}

func newEventQueue(size, batchSize int, flush time.Duration,
	deliver func(ctx context.Context, events []pluginsdk.Event) error,
	onResult func(delivered int, err error),
) *eventQueue {
	ctx, cancel := context.WithCancel(context.Background())
	q := &eventQueue{
		ch:        make(chan *lazyEvent, size),
		batchSize: batchSize,
		flush:     flush,
		deliver:   deliver,
		onResult:  onResult,
		ctx:       ctx,
		cancel:    cancel,
		stopCh:    make(chan struct{}),
		doneCh:    make(chan struct{}),
	}
	go q.run()
	return q
}

func (q *eventQueue) push(ev *lazyEvent) {
	select {
	case <-q.stopCh:
		return
	default:
	}
	select {
	case q.ch <- ev:
	default:
		q.dropped.Add(1)
	}
}

func (q *eventQueue) run() {
	defer close(q.doneCh)
	ticker := time.NewTicker(q.flush)
	defer ticker.Stop()
	batch := make([]pluginsdk.Event, 0, q.batchSize)
	size := 0
	send := func() {
		if len(batch) == 0 {
			return
		}
		if q.ctx.Err() != nil {
			q.dropped.Add(int64(len(batch)))
		} else {
			ctx, cancel := context.WithTimeout(q.ctx, eventDeliveryTimeout)
			err := q.deliver(ctx, batch)
			cancel()
			if q.onResult != nil {
				q.onResult(len(batch), err)
			}
		}
		batch = make([]pluginsdk.Event, 0, q.batchSize)
		size = 0
	}
	add := func(le *lazyEvent) {
		ev := le.event()
		n := len(ev.Data) + eventOverheadBytes
		if size+n > maxEventBatchBytes {
			send()
		}
		batch = append(batch, ev)
		size += n
		if len(batch) >= q.batchSize || size >= maxEventBatchBytes {
			send()
		}
	}
	for {
		select {
		case ev := <-q.ch:
			add(ev)
		case <-ticker.C:
			send()
		case <-q.stopCh:
			// Final flush of what is already queued, until stop gives up.
			for q.ctx.Err() == nil {
				select {
				case ev := <-q.ch:
					add(ev)
					continue
				default:
				}
				send()
				return
			}
			q.dropped.Add(int64(len(batch) + len(q.ch)))
			return
		}
	}
}

// stop ends the worker after a final flush of the queued events. The flush
// may take eventStopTimeout (less if ctx ends first); then the delivery in
// progress is cancelled and the remaining events are dropped. stop returns
// once the worker has exited, so the plugin can be closed right after, or
// after eventStopGrace if a delivery ignores the cancellation.
func (q *eventQueue) stop(ctx context.Context) {
	q.stopOnce.Do(func() { close(q.stopCh) })
	timer := time.NewTimer(eventStopTimeout)
	defer timer.Stop()
	select {
	case <-q.doneCh:
		return
	case <-timer.C:
	case <-ctx.Done():
	}
	q.cancel()
	grace := time.NewTimer(eventStopGrace)
	defer grace.Stop()
	select {
	case <-q.doneCh:
	case <-grace.C:
	}
}
