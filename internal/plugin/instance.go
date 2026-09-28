package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/Resinat/Resin/pkg/pluginsdk"
)

// instance is one running plugin, either in-process (builtin) or a child
// process. All methods must be safe for concurrent use.
type instance interface {
	// Capabilities returns the effective capabilities of this instance.
	Capabilities() pluginsdk.Capabilities
	Configure(ctx context.Context, config json.RawMessage) error
	Inspect(ctx context.Context, req *pluginsdk.RequestInfo) (*pluginsdk.RequestDecision, error)
	HandleEvents(ctx context.Context, events []pluginsdk.Event) error
	Close(ctx context.Context) error
}

// builtinInstance adapts an in-process pluginsdk.Plugin.
type builtinInstance struct {
	p         pluginsdk.Plugin
	inspector pluginsdk.RequestInspector
	events    pluginsdk.EventHandler
	caps      pluginsdk.Capabilities
}

func startBuiltin(ctx context.Context, b *Builtin, params pluginsdk.RegisterParams) (inst *builtinInstance, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("plugin panic: %v", r)
		}
	}()
	p := b.New()
	if p == nil {
		return nil, fmt.Errorf("builtin %s returned nil plugin", b.Manifest.ID)
	}
	if init, ok := p.(pluginsdk.Initializer); ok {
		if err := init.Init(ctx, params); err != nil {
			return nil, err
		}
	}
	if err := p.Configure(ctx, params.Config); err != nil {
		closePlugin(ctx, p)
		return nil, err
	}
	inst = &builtinInstance{p: p}
	reported := &pluginsdk.Capabilities{}
	if ri, ok := p.(pluginsdk.RequestInspector); ok {
		inst.inspector = ri
		reported.RequestHook = true
	}
	if eh, ok := p.(pluginsdk.EventHandler); ok {
		inst.events = eh
		reported.Events = []string{"*"}
	}
	inst.caps = effectiveCapabilities(b.Manifest.Capabilities, reported)
	return inst, nil
}

func (b *builtinInstance) Capabilities() pluginsdk.Capabilities { return b.caps }

func (b *builtinInstance) Configure(ctx context.Context, config json.RawMessage) (err error) {
	defer recoverInto(&err)
	return b.p.Configure(ctx, config)
}

func (b *builtinInstance) Inspect(ctx context.Context, req *pluginsdk.RequestInfo) (dec *pluginsdk.RequestDecision, err error) {
	if b.inspector == nil {
		return nil, nil
	}
	defer recoverInto(&err)
	return b.inspector.InspectRequest(ctx, req)
}

func (b *builtinInstance) HandleEvents(ctx context.Context, events []pluginsdk.Event) (err error) {
	if b.events == nil {
		return nil
	}
	defer recoverInto(&err)
	return b.events.HandleEvents(ctx, events)
}

func (b *builtinInstance) Close(ctx context.Context) (err error) {
	defer recoverInto(&err)
	closePlugin(ctx, b.p)
	return nil
}

func closePlugin(ctx context.Context, p pluginsdk.Plugin) {
	if sd, ok := p.(pluginsdk.Shutdowner); ok {
		_ = sd.Shutdown(ctx)
		return
	}
	if c, ok := p.(io.Closer); ok {
		_ = c.Close()
	}
}

func recoverInto(err *error) {
	if r := recover(); r != nil {
		*err = fmt.Errorf("plugin panic: %v", r)
	}
}

// effectiveCapabilities intersects the manifest declaration with what the
// plugin reported at registration. reported == nil means "same as manifest".
// Event subscriptions are expanded to concrete event types.
func effectiveCapabilities(declared pluginsdk.Capabilities, reported *pluginsdk.Capabilities) pluginsdk.Capabilities {
	out := pluginsdk.Capabilities{RequestHook: declared.RequestHook}
	if reported != nil && !reported.RequestHook {
		out.RequestHook = false
	}
	for _, t := range pluginsdk.KnownEventTypes {
		if !pluginsdk.MatchEvent(declared.Events, t) {
			continue
		}
		if reported != nil && !pluginsdk.MatchEvent(reported.Events, t) {
			continue
		}
		out.Events = append(out.Events, t)
	}
	return out
}
