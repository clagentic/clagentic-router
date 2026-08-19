// internal/router/sticky_fallback_test.go — regression tests for
// sticky-through-fallback tool-capability enforcement inside Route (lr-add405).
//
// FilterChainForTools (tool_capability_test.go) screens out whole chain
// ENTRIES with no tool-capable candidate at all, but deliberately keeps a
// mixed tier-alias/role-chain entry as-is when at least one candidate in it
// is tool-capable (see FilterChainForTools doc). Route's own per-candidate
// walk is what must then refuse to fall back to an incapable candidate
// inside that entry — this file is the "mixed chain, capable backend fails,
// incapable one is next" scenario HOLDEN's design verdict called out as the
// single most important correctness requirement in lr-add405.
package router

import (
	"context"
	"errors"
	"testing"

	"github.com/clagentic/clagentic-router/internal/backend"
)

// scriptedAdapter is a capAdapter variant whose Invoke outcome is
// caller-controlled, so a test can force a tool-capable candidate to fail
// and observe whether Route falls back to an incapable one.
type scriptedAdapter struct {
	id      string
	caps    backend.Capabilities
	invoked bool
	invoke  func(ctx context.Context, req *backend.Request) (*backend.Response, error)
}

func (s *scriptedAdapter) ID() string { return s.id }
func (s *scriptedAdapter) Invoke(ctx context.Context, req *backend.Request) (*backend.Response, error) {
	s.invoked = true
	return s.invoke(ctx, req)
}
func (s *scriptedAdapter) Capabilities() backend.Capabilities { return s.caps }

// TestRoute_MixedTierEntry_ToolCapableFails_NeverFallsBackToIncapable is the
// core sticky-through-fallback regression test: a single tier-alias entry
// whose candidate set mixes a tool-capable backend (which fails) and a
// tool-incapable backend (which would otherwise be tried next). Route must
// NEVER invoke the incapable backend for a tools-bearing request — it must
// exhaust the chain and return ErrAllFailed instead of silently degrading
// to a backend that would drop the tools.
func TestRoute_MixedTierEntry_ToolCapableFails_NeverFallsBackToIncapable(t *testing.T) {
	capableFails := &scriptedAdapter{
		id:   "capable",
		caps: backend.Capabilities{SupportsTools: true},
		invoke: func(_ context.Context, _ *backend.Request) (*backend.Response, error) {
			return nil, &backend.InvokeError{Type: backend.ErrTypeNetwork, Raw: "boom"}
		},
	}
	incapableWouldSucceed := &scriptedAdapter{
		id:   "incapable",
		caps: backend.Capabilities{SupportsTools: false},
		invoke: func(_ context.Context, _ *backend.Request) (*backend.Response, error) {
			return &backend.Response{Content: "silently dropped the tools"}, nil
		},
	}

	adapters := map[string]backend.Adapter{
		"capable":   capableFails,
		"incapable": incapableWouldSucceed,
	}
	tiers := map[string][]string{
		// A single mixed tier-alias entry — FilterChainForTools keeps this
		// entry as-is (at least one candidate, "capable", is tool-capable).
		"mixed-tier": {"capable", "incapable"},
	}
	r := newCapTestRouter(adapters, tiers, nil)

	req := &backend.Request{
		Messages: []backend.Message{{Role: "user", Content: "use a tool"}},
		HasTools: true,
	}
	_, _, err := r.Route(context.Background(), req, []string{"mixed-tier"})

	if !errors.Is(err, ErrAllFailed) {
		t.Fatalf("expected ErrAllFailed (chain must exhaust rather than degrade to an incapable backend), got %v", err)
	}
	if !capableFails.invoked {
		t.Error("expected the tool-capable candidate to have been tried")
	}
	if incapableWouldSucceed.invoked {
		t.Error("tool-incapable candidate must NEVER be invoked for a tools-bearing request — this is the silent tool-drop lr-be9454 was built to kill, resurrected at the fallback boundary")
	}
}

// TestRoute_MixedChainAcrossEntries_SkipsIncapableEntry verifies the same
// invariant across two SEPARATE chain entries (not one mixed tier): a
// tools-bearing request must skip a later incapable direct-backend entry
// and keep going, never invoking it, even though a non-tools request would
// have used it.
func TestRoute_MixedChainAcrossEntries_SkipsIncapableEntry(t *testing.T) {
	capableFails := &scriptedAdapter{
		id:   "capable",
		caps: backend.Capabilities{SupportsTools: true},
		invoke: func(_ context.Context, _ *backend.Request) (*backend.Response, error) {
			return nil, &backend.InvokeError{Type: backend.ErrTypeNetwork, Raw: "boom"}
		},
	}
	incapableWouldSucceed := &scriptedAdapter{
		id:   "incapable",
		caps: backend.Capabilities{SupportsTools: false},
		invoke: func(_ context.Context, _ *backend.Request) (*backend.Response, error) {
			return &backend.Response{Content: "silently dropped the tools"}, nil
		},
	}

	adapters := map[string]backend.Adapter{
		"capable":   capableFails,
		"incapable": incapableWouldSucceed,
	}
	r := newCapTestRouter(adapters, nil, nil)

	req := &backend.Request{
		Messages: []backend.Message{{Role: "user", Content: "use a tool"}},
		HasTools: true,
	}
	_, _, err := r.Route(context.Background(), req, []string{"capable", "incapable"})

	if !errors.Is(err, ErrAllFailed) {
		t.Fatalf("expected ErrAllFailed, got %v", err)
	}
	if incapableWouldSucceed.invoked {
		t.Error("tool-incapable fallback entry must never be invoked for a tools-bearing request")
	}
}

// TestRoute_NoToolsRequest_StillFallsBackToIncapable is the control case:
// the sticky filter must be gated on req.HasTools, not applied
// unconditionally — a request with no tools must still fall back normally
// to an "incapable" backend (capability is irrelevant when no tools are
// being carried).
func TestRoute_NoToolsRequest_StillFallsBackToIncapable(t *testing.T) {
	capableFails := &scriptedAdapter{
		id:   "capable",
		caps: backend.Capabilities{SupportsTools: true},
		invoke: func(_ context.Context, _ *backend.Request) (*backend.Response, error) {
			return nil, &backend.InvokeError{Type: backend.ErrTypeNetwork, Raw: "boom"}
		},
	}
	incapableSucceeds := &scriptedAdapter{
		id:   "incapable",
		caps: backend.Capabilities{SupportsTools: false},
		invoke: func(_ context.Context, _ *backend.Request) (*backend.Response, error) {
			return &backend.Response{Content: "fine, no tools here"}, nil
		},
	}

	adapters := map[string]backend.Adapter{
		"capable":   capableFails,
		"incapable": incapableSucceeds,
	}
	r := newCapTestRouter(adapters, nil, nil)

	req := &backend.Request{
		Messages: []backend.Message{{Role: "user", Content: "no tools here"}},
		// HasTools intentionally omitted (false).
	}
	resp, _, err := r.Route(context.Background(), req, []string{"capable", "incapable"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Content != "fine, no tools here" {
		t.Errorf("expected fallback to the incapable backend to succeed, got %q", resp.Content)
	}
	if !incapableSucceeds.invoked {
		t.Error("expected the incapable backend to have been tried (no tools on this request)")
	}
}
