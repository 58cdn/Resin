package plugin

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Resinat/Resin/pkg/pluginsdk"
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

	dropped atomic.Int64

	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
}

func newEventQueue(size, batchSize int, flush time.Duration,
	deliver func(ctx context.Context, events []pluginsdk.Event) error,
	onResult func(delivered int, err error),
) *eventQueue {
	q := &eventQueue{
		ch:        make(chan *lazyEvent, size),
		batchSize: batchSize,
		flush:     flush,
		deliver:   deliver,
		onResult:  onResult,
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
	send := func(timeout time.Duration) {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		err := q.deliver(ctx, batch)
		cancel()
		if q.onResult != nil {
			q.onResult(len(batch), err)
		}
		batch = make([]pluginsdk.Event, 0, q.batchSize)
	}
	for {
		select {
		case ev := <-q.ch:
			batch = append(batch, ev.event())
			if len(batch) >= q.batchSize {
				send(eventDeliveryTimeout)
			}
		case <-ticker.C:
			send(eventDeliveryTimeout)
		case <-q.stopCh:
			// Best-effort final flush of what is already queued.
			for drained := false; !drained; {
				select {
				case ev := <-q.ch:
					batch = append(batch, ev.event())
					if len(batch) >= q.batchSize {
						send(2 * time.Second)
					}
				default:
					drained = true
				}
			}
			send(2 * time.Second)
			return
		}
	}
}

// stop ends the worker after a final flush and waits for it (bounded by ctx).
func (q *eventQueue) stop(ctx context.Context) {
	q.stopOnce.Do(func() { close(q.stopCh) })
	select {
	case <-q.doneCh:
	case <-ctx.Done():
	}
}
