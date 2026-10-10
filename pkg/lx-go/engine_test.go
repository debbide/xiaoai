package lxgo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEngineCallSupportsCallbackRequest(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"value":"jay"}`))
	}))
	defer ts.Close()

	engine, err := NewEngine("test", `
		const { on, request, EVENT_NAMES } = globalThis.lx;
		on(EVENT_NAMES.request, async ({ info }) => {
			return await new Promise((resolve, reject) => {
				request(info.url, { method: 'GET', timeout: 1000 }, (err, resp) => {
					if (err) return reject(new Error(err.message));
					resolve(JSON.parse(resp.body).value);
				});
			});
		});
	`)
	if err != nil {
		t.Fatalf("unexpected init error: %v", err)
	}
	defer engine.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	result, err := engine.Call(ctx, "request", map[string]any{
		"info": map[string]any{"url": ts.URL},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := result.(string); got != "jay" {
		t.Fatalf("unexpected result: %q", got)
	}
}

func TestEngineCallReturnsJSMessage(t *testing.T) {
	engine, err := NewEngine("test", `
		const { on, EVENT_NAMES } = globalThis.lx;
		on(EVENT_NAMES.request, async () => {
			throw new Error('boom');
		});
	`)
	if err != nil {
		t.Fatalf("unexpected init error: %v", err)
	}
	defer engine.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err = engine.Call(ctx, "request", map[string]any{})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := err.Error(); got != "JS Promise 被拒绝: Error: boom" {
		t.Fatalf("unexpected error: %q", got)
	}
}
