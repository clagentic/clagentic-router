// internal/backend/codex_model_discovery_test.go — table-driven tests for
// codex_cli's automatic model discovery via `codex debug models` (lr-82e68e).
//
// All slugs below are fabricated placeholders, not real codex model
// strings — see package doc on why slugs are never constructed/prefixed by
// this package (unconfirmed to be uniform across codex auth contexts).
package backend

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeCodexDebugModelsBin writes a fake "codex" binary that ignores its
// argv (works for both "debug models" and any other subcommand a caller
// might invoke) and always emits stdout on success, or exits non-zero with
// stderr when exitCode != 0.
func writeFakeCodexDebugModelsBin(t *testing.T, dir, stdout, stderr string, exitCode int) string {
	t.Helper()
	binPath := filepath.Join(dir, "codex")
	script := "#!/bin/sh\n"
	if stderr != "" {
		script += "printf '%s' " + shellQuote(stderr) + " >&2\n"
	}
	if stdout != "" {
		script += "printf '%s' " + shellQuote(stdout) + "\n"
	}
	if exitCode != 0 {
		script += "exit " + string(rune('0'+exitCode)) + "\n"
	}
	if err := os.WriteFile(binPath, []byte(script), 0755); err != nil {
		t.Fatalf("write fake codex bin: %v", err)
	}
	return binPath
}

// fakeCodexModelsJSON builds the wrapped-object catalog shape
// ({"models": [...]}) confirmed live on the ChatGPT-Plus path — never a bare
// array.
func fakeCodexModelsJSON(t *testing.T, entries []codexModelEntry) string {
	t.Helper()
	data, err := json.Marshal(codexModelsResponse{Models: entries})
	if err != nil {
		t.Fatalf("marshal fake catalog: %v", err)
	}
	return string(data)
}

// TestFilterAndSortCodexModels covers mixed visibility, supported_in_api
// false, both observed provider-ish priority orderings (sparse/non-contiguous
// and a dense range), proving selection always resolves by SORTED POSITION,
// never by matching a literal priority value.
func TestFilterAndSortCodexModels(t *testing.T) {
	t.Run("mixed visibility and supported_in_api excluded", func(t *testing.T) {
		entries := []codexModelEntry{
			{Slug: "fake-hidden-unsupported", Priority: 1, Visibility: "hide", SupportedInAPI: false},
			{Slug: "fake-best", Priority: 7, Visibility: "list", SupportedInAPI: true},
			{Slug: "fake-mid", Priority: 16, Visibility: "list", SupportedInAPI: true},
			{Slug: "fake-cheap", Priority: 23, Visibility: "list", SupportedInAPI: true},
			{Slug: "fake-internal", Priority: 43, Visibility: "hide", SupportedInAPI: true},
		}
		got := filterAndSortCodexModels(entries)
		wantSlugs := []string{"fake-best", "fake-mid", "fake-cheap"}
		assertSlugOrder(t, got, wantSlugs)
	})

	t.Run("sparse non-contiguous priority resolves by position not value", func(t *testing.T) {
		// Mirrors the confirmed live shape: priorities 1,7,16,23 are not
		// 0-based or contiguous. Sorted position (not the literal priority
		// number) must determine rank 0/1/2.
		entries := []codexModelEntry{
			{Slug: "fake-third", Priority: 23, Visibility: "list", SupportedInAPI: true},
			{Slug: "fake-first", Priority: 7, Visibility: "list", SupportedInAPI: true},
			{Slug: "fake-second", Priority: 16, Visibility: "list", SupportedInAPI: true},
		}
		got := filterAndSortCodexModels(entries)
		assertSlugOrder(t, got, []string{"fake-first", "fake-second", "fake-third"})
	})

	t.Run("dense zero-based priority resolves by position too", func(t *testing.T) {
		// A second, differently-shaped ordering (dense, 0-based) — proves
		// sorted-position resolution is not accidentally tied to any one
		// priority numbering scheme.
		entries := []codexModelEntry{
			{Slug: "fake-b", Priority: 1, Visibility: "list", SupportedInAPI: true},
			{Slug: "fake-a", Priority: 0, Visibility: "list", SupportedInAPI: true},
			{Slug: "fake-c", Priority: 2, Visibility: "list", SupportedInAPI: true},
		}
		got := filterAndSortCodexModels(entries)
		assertSlugOrder(t, got, []string{"fake-a", "fake-b", "fake-c"})
	})

	t.Run("empty catalog", func(t *testing.T) {
		got := filterAndSortCodexModels(nil)
		if len(got) != 0 {
			t.Errorf("expected empty result, got %d entries", len(got))
		}
	})

	t.Run("all entries filtered out", func(t *testing.T) {
		entries := []codexModelEntry{
			{Slug: "fake-hidden", Priority: 1, Visibility: "hide", SupportedInAPI: true},
			{Slug: "fake-unsupported", Priority: 2, Visibility: "list", SupportedInAPI: false},
		}
		got := filterAndSortCodexModels(entries)
		if len(got) != 0 {
			t.Errorf("expected empty result after filtering, got %d entries", len(got))
		}
	})
}

func assertSlugOrder(t *testing.T, got []codexModelEntry, wantSlugs []string) {
	t.Helper()
	if len(got) != len(wantSlugs) {
		t.Fatalf("got %d entries, want %d (%v)", len(got), len(wantSlugs), wantSlugs)
	}
	for i, w := range wantSlugs {
		if got[i].Slug != w {
			t.Errorf("position %d: slug = %q, want %q", i, got[i].Slug, w)
		}
	}
}

// TestSelectCodexModelByRank covers rank selection, out-of-range rank
// (a realistic case per lr-82e68e ground truth — a filtered catalog can be
// as small as three usable entries), and empty catalog.
func TestSelectCodexModelByRank(t *testing.T) {
	usable := []codexModelEntry{
		{Slug: "fake-best", Priority: 7},
		{Slug: "fake-mid", Priority: 16},
		{Slug: "fake-cheap", Priority: 23},
	}

	t.Run("rank 0 selects best", func(t *testing.T) {
		got, err := selectCodexModelByRank(usable, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "fake-best" {
			t.Errorf("slug = %q, want fake-best", got)
		}
	})

	t.Run("rank 2 selects last usable", func(t *testing.T) {
		got, err := selectCodexModelByRank(usable, 2)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "fake-cheap" {
			t.Errorf("slug = %q, want fake-cheap", got)
		}
	})

	t.Run("rank out of range is a construction-time error, not a clamp", func(t *testing.T) {
		_, err := selectCodexModelByRank(usable, 3)
		if err == nil {
			t.Fatal("expected error for out-of-range rank, got nil")
		}
	})

	t.Run("negative rank is an error", func(t *testing.T) {
		_, err := selectCodexModelByRank(usable, -1)
		if err == nil {
			t.Fatal("expected error for negative rank, got nil")
		}
	})

	t.Run("empty catalog is an error even for rank 0", func(t *testing.T) {
		_, err := selectCodexModelByRank(nil, 0)
		if err == nil {
			t.Fatal("expected error for empty catalog, got nil")
		}
	})
}

// TestResolveCodexModel_ViaFakeBinary covers the full ResolveCodexModel path
// against a fake codex binary: catalog shape (wrapped object, not bare
// array), malformed JSON, and command failure.
func TestResolveCodexModel_ViaFakeBinary(t *testing.T) {
	t.Run("wrapped object shape resolves rank 0", func(t *testing.T) {
		dir := t.TempDir()
		catalog := fakeCodexModelsJSON(t, []codexModelEntry{
			{Slug: "fake-hidden-unsupported", Priority: 1, Visibility: "hide", SupportedInAPI: false},
			{Slug: "fake-best", Priority: 7, Visibility: "list", SupportedInAPI: true},
			{Slug: "fake-mid", Priority: 16, Visibility: "list", SupportedInAPI: true},
			{Slug: "fake-cheap", Priority: 23, Visibility: "list", SupportedInAPI: true},
			{Slug: "fake-internal", Priority: 43, Visibility: "hide", SupportedInAPI: true},
		})
		bin := writeFakeCodexDebugModelsBin(t, dir, catalog, "", 0)

		got, err := ResolveCodexModel(context.Background(), bin, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "fake-best" {
			t.Errorf("slug = %q, want fake-best", got)
		}
	})

	t.Run("rank 1 selects second-best", func(t *testing.T) {
		dir := t.TempDir()
		catalog := fakeCodexModelsJSON(t, []codexModelEntry{
			{Slug: "fake-best", Priority: 7, Visibility: "list", SupportedInAPI: true},
			{Slug: "fake-mid", Priority: 16, Visibility: "list", SupportedInAPI: true},
			{Slug: "fake-cheap", Priority: 23, Visibility: "list", SupportedInAPI: true},
		})
		bin := writeFakeCodexDebugModelsBin(t, dir, catalog, "", 0)

		got, err := ResolveCodexModel(context.Background(), bin, 1)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "fake-mid" {
			t.Errorf("slug = %q, want fake-mid", got)
		}
	})

	t.Run("bare array shape (wrong shape) fails to resolve any usable model", func(t *testing.T) {
		// Proves the parser requires the "models"-wrapped object shape: a
		// bare array must not silently be accepted.
		dir := t.TempDir()
		bareArray := `[{"slug":"fake-best","priority":7,"visibility":"list","supported_in_api":true}]`
		bin := writeFakeCodexDebugModelsBin(t, dir, bareArray, "", 0)

		_, err := ResolveCodexModel(context.Background(), bin, 0)
		if err == nil {
			t.Fatal("expected error: bare-array shape must not silently resolve a model")
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		dir := t.TempDir()
		bin := writeFakeCodexDebugModelsBin(t, dir, "{not valid json", "", 0)

		_, err := ResolveCodexModel(context.Background(), bin, 0)
		if err == nil {
			t.Fatal("expected error for malformed JSON, got nil")
		}
	})

	t.Run("command failure", func(t *testing.T) {
		dir := t.TempDir()
		bin := writeFakeCodexDebugModelsBin(t, dir, "", "codex: internal error", 1)

		_, err := ResolveCodexModel(context.Background(), bin, 0)
		if err == nil {
			t.Fatal("expected error for command failure, got nil")
		}
		if !strings.Contains(err.Error(), "codex_model_discovery") {
			t.Errorf("error should be namespaced for diagnosability, got: %v", err)
		}
	})

	t.Run("empty catalog after filtering", func(t *testing.T) {
		dir := t.TempDir()
		catalog := fakeCodexModelsJSON(t, []codexModelEntry{
			{Slug: "fake-hidden", Priority: 1, Visibility: "hide", SupportedInAPI: true},
			{Slug: "fake-unsupported", Priority: 2, Visibility: "list", SupportedInAPI: false},
		})
		bin := writeFakeCodexDebugModelsBin(t, dir, catalog, "", 0)

		_, err := ResolveCodexModel(context.Background(), bin, 0)
		if err == nil {
			t.Fatal("expected error for empty usable catalog, got nil")
		}
	})

	t.Run("rank out of range against real catalog size", func(t *testing.T) {
		// Mirrors the live ground truth: filtering a 5-entry raw catalog can
		// yield as few as 3 usable entries, making an out-of-range rank a
		// realistic operator mistake, not a theoretical case.
		dir := t.TempDir()
		catalog := fakeCodexModelsJSON(t, []codexModelEntry{
			{Slug: "fake-hidden-unsupported", Priority: 1, Visibility: "hide", SupportedInAPI: false},
			{Slug: "fake-best", Priority: 7, Visibility: "list", SupportedInAPI: true},
			{Slug: "fake-mid", Priority: 16, Visibility: "list", SupportedInAPI: true},
			{Slug: "fake-cheap", Priority: 23, Visibility: "list", SupportedInAPI: true},
			{Slug: "fake-internal", Priority: 43, Visibility: "hide", SupportedInAPI: true},
		})
		bin := writeFakeCodexDebugModelsBin(t, dir, catalog, "", 0)

		_, err := ResolveCodexModel(context.Background(), bin, 5)
		if err == nil {
			t.Fatal("expected error for out-of-range rank, got nil")
		}
	})
}
