package ipcheck

import (
	"context"
	"time"

	"github.com/Resinat/Resin/internal/node"
)

const MaxBackgroundNodes = 50
const backgroundCycleBudget = 5 * time.Minute

// ActivateBackground is fast, idempotent and infallible. Call only after the
// owning plugin's settings have been committed. Construction and validation
// never schedule checks. Close cancels this engine's worker and waits for it.
func (e *Engine) ActivateBackground() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || e.backgroundStarted || !e.config.BackgroundEnabled {
		return
	}
	e.backgroundStarted = true
	e.workers.Add(1)
	go e.runBackground()
}

func (e *Engine) runBackground() {
	defer e.workers.Done()
	interval, _ := time.ParseDuration(e.config.BackgroundInterval) // validated
	hashes := make([]node.Hash, len(e.config.BackgroundNodes))
	for i, value := range e.config.BackgroundNodes {
		hashes[i], _ = node.ParseHex(value)
	}
	// Fixed delay, including the first cycle; no catch-up, overlap or restart
	// replay. A slow cycle advances its cursor before every attempted node.
	cursor := 0
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-timer.C:
		}
		ctx, cancel := context.WithTimeout(e.ctx, backgroundCycleBudget)
		for visited := 0; visited < len(hashes) && ctx.Err() == nil; visited++ {
			hash := hashes[cursor]
			cursor = (cursor + 1) % len(hashes)
			// Eligibility is checked at admission. The existing engine additionally
			// revalidates node existence and egress generation before publishing.
			snapshot, exists := e.host.NodeSnapshot(ctx, hash)
			if !exists || !snapshot.Healthy || !snapshot.Enabled || !IsPublicIP(snapshot.EgressIP) {
				continue
			}
			_, _ = e.CheckNode(ctx, hash, e.config.BackgroundSources, false)
		}
		cancel()
		if e.ctx.Err() != nil {
			return
		}
		timer.Reset(interval)
	}
}
