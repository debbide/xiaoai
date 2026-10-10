package lxgo

import (
	"context"
	"testing"
	"time"

	"github.com/dop251/goja"
)

// TestFormatJSValueDoesNotPanicOnNonMapExport 复现 review 里提到的第 1 个问题：
// v.Export() 返回的不是 map[string]any（比如脚本用 `Promise.reject('a string')`
// 或者 `throw` 一个字符串/数字）时，formatJSValue 之前会直接 panic。
func TestFormatJSValueDoesNotPanicOnNonMapExport(t *testing.T) {
	vm := goja.New()

	cases := []struct {
		name string
		js   string
	}{
		{"string reject", `"action not support: foo"`},
		{"number reject", `42`},
		{"array reject", `[1,2,3]`},
		{"null-ish object without message field", `({code: 500})`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, err := vm.RunString(c.js)
			if err != nil {
				t.Fatalf("eval failed: %v", err)
			}
			// 之前的实现在这里会 panic；现在应该安全返回某个字符串。
			got := formatJSValue(v)
			if got == "" {
				t.Fatal("expected non-empty fallback string")
			}
		})
	}
}

// TestEngineRejectWithNonErrorValueDoesNotCrash 端到端复现 HYWmusic 风格脚本的
// `return Promise.reject('action not support: ' + action)` 写法：直接用字符串
// reject，而不是 `throw new Error(...)`。这条路径之前会因为 formatJSValue 里的
// 不安全类型断言直接 panic 崩掉整个 event loop goroutine（进而崩进程）。
func TestEngineRejectWithNonErrorValueDoesNotCrash(t *testing.T) {
	engine, err := NewEngine("reject-string-source", `
		const { on, EVENT_NAMES } = globalThis.lx;
		on(EVENT_NAMES.request, async ({ action }) => {
			if (action !== 'known') return Promise.reject('action not support: ' + action);
			return 'ok';
		});
	`)
	if err != nil {
		t.Fatalf("NewEngine failed: %v", err)
	}
	defer engine.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// 这一行在修复前会让整个测试进程 panic 崩溃，而不是拿到一个正常的 error。
	_, err = engine.Call(ctx, "request", map[string]any{"action": "unknown"})
	if err == nil {
		t.Fatal("expected an error")
	}

	// 引擎必须还活着，能正常处理下一个请求（证明没有把 event loop 拖挂）。
	result, err := engine.Call(ctx, "request", map[string]any{"action": "known"})
	if err != nil {
		t.Fatalf("engine should still be alive after previous rejection: %v", err)
	}
	if result != "ok" {
		t.Fatalf("unexpected result: %v", result)
	}
}

// TestRequestAsyncURLObjectWithNonStringURLDoesNotCrash 复现 review 第 3 个问题：
// request({url: 123}, ...) 这种 url 字段不是字符串的畸形调用，之前会在
// `u.(string)` 处直接 panic。
func TestRequestAsyncURLObjectWithNonStringURLDoesNotCrash(t *testing.T) {
	engine, err := NewEngine("bad-url-source", `
		const { on, request, EVENT_NAMES } = globalThis.lx;
		on(EVENT_NAMES.request, async () => {
			return await new Promise((resolve, reject) => {
				request({ url: 12345 }, { method: 'GET', timeout: 500 }, (err) => {
					if (err) return reject(new Error(err.message));
					resolve('unexpected success');
				});
			});
		});
	`)
	if err != nil {
		t.Fatalf("NewEngine failed: %v", err)
	}
	defer engine.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// 之前这里会直接 panic；现在应该是一个正常的（大概率是"URL 为空导致请求失败"）error。
	_, err = engine.Call(ctx, "request", map[string]any{})
	if err == nil {
		t.Fatal("expected an error for malformed url, not a crash")
	}
}

// TestPanicInHandlerDoesNotCrashProcess 验证事件处理器执行过程中的 panic
// （比如脚本触发了 goja 内部某个类型转换异常）会被 runOnLoopSafe 兜住，
// 变成一个普通的 error 返回，而不是让整个 event loop / 进程崩掉。
func TestPanicInHandlerDoesNotCrashProcess(t *testing.T) {
	engine, err := NewEngine("panic-source", `
		const { on, EVENT_NAMES } = globalThis.lx;
		let calls = 0;
		on(EVENT_NAMES.request, ({ action }) => {
			calls++;
			if (action === 'boom') {
				// 制造一个 goja 侧的运行时错误
				return undefined.someProperty;
			}
			return 'ok-' + calls;
		});
	`)
	if err != nil {
		t.Fatalf("NewEngine failed: %v", err)
	}
	defer engine.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err = engine.Call(ctx, "request", map[string]any{"action": "boom"})
	if err == nil {
		t.Fatal("expected an error")
	}

	// 进程/引擎必须还活着。
	result, err := engine.Call(ctx, "request", map[string]any{"action": "fine"})
	if err != nil {
		t.Fatalf("engine should still be alive: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
}
