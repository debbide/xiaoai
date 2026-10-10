package lxgo

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newSearchableEngine(t *testing.T, name string) *Engine {
	return mustEngine(t, name, `
		const { on, send, EVENT_NAMES } = globalThis.lx;
		on(EVENT_NAMES.request, async ({ info }) => [{ name: info.keyword, source: 'wy' }]);
		send(EVENT_NAMES.inited, { sources: { wy: { actions: ['search'] } } });
	`)
}

func TestSwitchToNextSearchSourceCyclesThroughPool(t *testing.T) {
	a := &Source{Name: "A", Engine: newSearchableEngine(t, "A")}
	b := &Source{Name: "B", Engine: newSearchableEngine(t, "B")}
	c := &Source{Name: "C", Engine: newSearchableEngine(t, "C")}
	srv := NewServer(&Registry{Sources: []*Source{a, b, c}})

	// 默认应该是第一个（A）。
	if def := srv.currentDefaultSearch(srv.searchPool()); def.Name != "A" {
		t.Fatalf("expected default to be A, got %s", def.Name)
	}

	prev, cur := srv.SwitchToNextSearchSource()
	if prev.Name != "A" || cur.Name != "B" {
		t.Fatalf("expected A -> B, got %s -> %s", prev.Name, cur.Name)
	}

	prev, cur = srv.SwitchToNextSearchSource()
	if prev.Name != "B" || cur.Name != "C" {
		t.Fatalf("expected B -> C, got %s -> %s", prev.Name, cur.Name)
	}

	// 应该循环回到 A。
	prev, cur = srv.SwitchToNextSearchSource()
	if prev.Name != "C" || cur.Name != "A" {
		t.Fatalf("expected C -> A (wrap around), got %s -> %s", prev.Name, cur.Name)
	}
}

func TestHandleSwitchSearchSourceHTTP(t *testing.T) {
	a := &Source{Name: "A", Engine: newSearchableEngine(t, "A")}
	b := &Source{Name: "B", Engine: newSearchableEngine(t, "B")}
	srv := NewServer(&Registry{Sources: []*Source{a, b}})

	req := httptest.NewRequest(http.MethodGet, "/api/music/switch-source", nil)
	w := httptest.NewRecorder()
	srv.handleSwitchSearchSource(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", w.Code, w.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if result["previous"] != "A" || result["current"] != "B" {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestHandleMusicSearchPrefersDefaultSource(t *testing.T) {
	a := &Source{Name: "A", Engine: mustEngine(t, "A", `
		const { on, send, EVENT_NAMES } = globalThis.lx;
		on(EVENT_NAMES.request, async ({ info }) => [{ name: info.keyword, source: 'wy', from: 'A' }]);
		send(EVENT_NAMES.inited, { sources: { wy: { actions: ['search'] } } });
	`)}
	b := &Source{Name: "B", Engine: mustEngine(t, "B", `
		const { on, send, EVENT_NAMES } = globalThis.lx;
		on(EVENT_NAMES.request, async ({ info }) => [{ name: info.keyword, source: 'wy', from: 'B' }]);
		send(EVENT_NAMES.inited, { sources: { wy: { actions: ['search'] } } });
	`)}
	srv := NewServer(&Registry{Sources: []*Source{a, b}})

	// 切一次默认音源到 B。
	srv.SwitchToNextSearchSource()

	req := httptest.NewRequest(http.MethodGet, "/api/music/search?name=x&source=wy", nil)
	w := httptest.NewRecorder()
	srv.handleMusicSearch(w, req)

	var result []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(result) != 1 || result[0]["from"] != "B" {
		t.Fatalf("expected result from B (default source), got: %#v", result)
	}
}
