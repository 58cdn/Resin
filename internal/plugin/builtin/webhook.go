package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Resinat/Resin/internal/plugin"
	"github.com/Resinat/Resin/pkg/pluginsdk"
)

// WebhookID is the id of the webhook builtin.
const WebhookID = "resin.webhook"

func webhookBuiltin() plugin.Builtin {
	return plugin.Builtin{
		Manifest: pluginsdk.Manifest{
			SchemaVersion: pluginsdk.SchemaVersion,
			ID:            WebhookID,
			Name:          "Webhook",
			Version:       "1.0.0",
			Description:   `POST batches of Resin events (request.finished, lease.*) as JSON {"source":"resin","events":[...]} to an HTTP endpoint.`,
			Author:        "Resin",
			License:       "MIT",
			Capabilities:  pluginsdk.Capabilities{Events: []string{"*"}},
			ConfigFields: []pluginsdk.ConfigField{
				{Name: "url", Label: "URL", Type: pluginsdk.FieldString, Required: true,
					Description: "http(s) endpoint receiving event batches."},
				{Name: "headers", Label: "Headers", Type: pluginsdk.FieldJSON, Default: raw(`{}`),
					Description: `Extra request headers, e.g. {"Authorization":"Bearer ..."}.`},
				{Name: "events", Label: "Events", Type: pluginsdk.FieldStringList, Default: raw(`["*"]`),
					Description: "Event types to send: *, request.finished, lease.*, lease.created, lease.replaced, lease.removed, lease.expired."},
				{Name: "timeout_ms", Label: "Timeout (ms)", Type: pluginsdk.FieldInteger, Default: raw(`5000`)},
			},
		},
		New: func() pluginsdk.Plugin { return &webhook{client: &http.Client{}} },
	}
}

type webhookConfig struct {
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers"`
	Events    []string          `json:"events"`
	TimeoutMs int               `json:"timeout_ms"`
}

type webhook struct {
	client *http.Client
	cfg    atomic.Pointer[webhookConfig]
}

func (w *webhook) Configure(_ context.Context, config json.RawMessage) error {
	cfg := webhookConfig{Events: []string{"*"}, TimeoutMs: 5000}
	if err := decodeConfig(config, &cfg); err != nil {
		return err
	}
	cfg.URL = strings.TrimSpace(cfg.URL)
	u, err := url.Parse(cfg.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("url must be an absolute http(s) URL")
	}
	for k, v := range cfg.Headers {
		if strings.TrimSpace(k) == "" || strings.ContainsAny(k+v, "\r\n") {
			return fmt.Errorf("headers: invalid header %q", k)
		}
	}
	if len(cfg.Events) == 0 {
		cfg.Events = []string{"*"}
	}
	for _, ev := range cfg.Events {
		if !pluginsdk.MatchEvent([]string{ev}, pluginsdk.EventRequestFinished) &&
			!pluginsdk.MatchEvent([]string{ev}, pluginsdk.EventLeaseCreated) &&
			!pluginsdk.MatchEvent([]string{ev}, pluginsdk.EventLeaseReplaced) &&
			!pluginsdk.MatchEvent([]string{ev}, pluginsdk.EventLeaseRemoved) &&
			!pluginsdk.MatchEvent([]string{ev}, pluginsdk.EventLeaseExpired) {
			return fmt.Errorf("events: unknown event type %q", ev)
		}
	}
	if cfg.TimeoutMs <= 0 {
		cfg.TimeoutMs = 5000
	}
	if cfg.TimeoutMs > 60000 {
		return fmt.Errorf("timeout_ms must be at most 60000")
	}
	w.cfg.Store(&cfg)
	return nil
}

func (w *webhook) HandleEvents(ctx context.Context, events []pluginsdk.Event) error {
	cfg := w.cfg.Load()
	if cfg == nil {
		return nil
	}
	selected := events[:0:0]
	for _, ev := range events {
		if pluginsdk.MatchEvent(cfg.Events, ev.Type) {
			selected = append(selected, ev)
		}
	}
	if len(selected) == 0 {
		return nil
	}
	body, err := json.Marshal(struct {
		Source string            `json:"source"`
		Events []pluginsdk.Event `json:"events"`
	}{Source: "resin", Events: selected})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutMs)*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Resin-Webhook/1")
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
	}
	return nil
}
