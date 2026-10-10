package lxgo

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
	"github.com/dop251/goja_nodejs/buffer"
	"github.com/dop251/goja_nodejs/console"
	"github.com/dop251/goja_nodejs/eventloop"
	"github.com/dop251/goja_nodejs/require"
)

// runOnLoopSafe 是 loop.RunOnLoop 的安全版本：JS 脚本质量参差不齐（很多脚本
// 大量 `catch (e) {}` 吞异常，一些脚本用字符串而不是 Error 对象 reject），
// goja 内部的类型断言/转换也可能因为脚本返回了意料之外的值而 panic。
// 事件循环回调是在专门的 goroutine 里跑的，一旦这里 panic 且没有 recover，
// 会直接崩掉整个进程，而不是"这一次请求失败"。所有 loop.RunOnLoop 调用都必须
// 经过这层包装。
func runOnLoopSafe(loop *eventloop.EventLoop, name string, fn func(vm *goja.Runtime)) {
	loop.RunOnLoop(func(vm *goja.Runtime) {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("❌ [lx-go] event loop panic recovered (%s): %v\n%s", name, r, debug.Stack())
			}
		}()
		fn(vm)
	})
}

// scriptVersion / scriptEnv 对齐真实 lx-music 客户端注入给自定义音源脚本的
// `lx.version` / `lx.env`，取一个较新的桌面端版本号，尽量让脚本走"全功能"分支
// （不少脚本会用 `env === 'mobile'` 之类的条件裁剪功能）。
const (
	scriptVersion = "2.5.0"
	scriptEnv     = "electron"
)

// SourceMeta 是脚本通过 `send(EVENT_NAMES.inited, { sources: {...} })` 上报的
// 单个平台（wy/tx/kw/kg/mg...）能力描述。
type SourceMeta struct {
	Name    string   `json:"name"`
	Actions []string `json:"actions"`
}

func (m SourceMeta) supports(action string) bool {
	for _, a := range m.Actions {
		if a == action {
			return true
		}
	}
	return false
}

type Engine struct {
	Name string // 音源名称，取自脚本头部注释里的 @name，取不到则回退为文件名

	loop     *eventloop.EventLoop
	ready    chan struct{}
	initErr  error
	initOnce sync.Once
	metaMu   sync.RWMutex
	events   map[string]any
	// 由于 handlers 在 loop 外部也会被访问/修改，将其改为 goja.Value 存储，
	// 并在涉及跨 Goroutine 读写时通过事件循环调度，防止 Data Race（数据竞争）
	handlers map[string]goja.Value
}

// NewEngine 创建一个独立的 goja 运行时并加载一段音源脚本。
// name 用于日志与音源优先级展示；rawScript 完整保留脚本源码，
// 通过 `lx.currentScriptInfo.rawScript` 暴露给脚本自己（部分脚本会在运行时
// 重新解析自己头部注释里的 @xxx 配置项，如 墨澜聚合音源 的 @tx_cookie）。
// 初始化失败时返回 error 而不是 panic，方便调用方在加载多个音源脚本时
// 跳过坏掉的脚本、继续加载其余的。
func NewEngine(name string, rawScript string) (*Engine, error) {
	loop := eventloop.NewEventLoop()
	loop.Start()

	e := &Engine{
		Name:     name,
		loop:     loop,
		ready:    make(chan struct{}),
		events:   make(map[string]any),
		handlers: make(map[string]goja.Value),
	}

	// 在常驻事件循环中完成一次 JS 环境初始化，后续请求通过 RunOnLoop 投递执行。
	loop.RunOnLoop(func(vm *goja.Runtime) {
		defer close(e.ready)
		// 单独处理（而不是走 runOnLoopSafe）是因为这里 panic 时除了要记日志，
		// 还必须把 e.initErr 设置成非 nil，否则 NewEngine 会把"初始化时 panic
		// 了"误判成"初始化成功"，返回一个半初始化的 Engine。
		defer func() {
			if r := recover(); r != nil {
				log.Printf("❌ [lx-go] 音源 %s 初始化 panic: %v\n%s", name, r, debug.Stack())
				e.initErr = fmt.Errorf("panic during init: %v", r)
			}
		}()

		// 启用 require + Buffer + console，尽量对齐真实 Node/lx-music 沙箱，
		// 兼容脚本里 `typeof Buffer !== 'undefined'`、`console.log(...)` 等写法。
		registry := new(require.Registry)
		registry.Enable(vm)
		buffer.Enable(vm)
		console.Enable(vm)

		lx := map[string]interface{}{
			// 修复点：包装为符合 async/await 的异步 Promise 请求
			"request": func(call goja.FunctionCall) goja.Value {
				return requestAsync(vm, loop, call)
			},
			"send": func(call goja.FunctionCall) goja.Value {
				if len(call.Arguments) >= 2 {
					e.metaMu.Lock()
					e.events[call.Argument(0).String()] = call.Argument(1).Export()
					e.metaMu.Unlock()
				}
				return nil
			},
			"on": func(call goja.FunctionCall) goja.Value {
				event := call.Argument(0).String()
				handler := call.Argument(1)
				e.handlers[event] = handler
				return nil
			},
			"EVENT_NAMES": map[string]string{
				"request":     "request",
				"inited":      "inited",
				"updateAlert": "updateAlert",
			},
			"env":     scriptEnv,
			"version": scriptVersion,
			"utils":   newJSUtils(),
			"currentScriptInfo": map[string]interface{}{
				"name":      name,
				"rawScript": rawScript,
			},
		}

		vm.Set("lx", lx)

		// 挂载全局环境，适配一些混淆脚本需要的 global 变量
		vm.Set("globalThis", vm.GlobalObject())

		_, err := vm.RunString(rawScript)
		if err != nil {
			e.initErr = err
		}
	})

	<-e.ready
	if e.initErr != nil {
		e.Close()
		return nil, e.initErr
	}

	return e, nil
}

// Sources 解析脚本上报的 inited.sources，返回 {平台代码: 能力描述}。
// 未上报或格式不对时返回空 map，调用方应当把这种音源当作"不支持任何平台"处理。
func (e *Engine) Sources() map[string]SourceMeta {
	out := make(map[string]SourceMeta)
	payload, ok := e.EventPayload("inited")
	if !ok {
		return out
	}
	initedMap, ok := payload.(map[string]interface{})
	if !ok {
		return out
	}
	rawSources, ok := initedMap["sources"].(map[string]interface{})
	if !ok {
		return out
	}
	for platform, v := range rawSources {
		meta := SourceMeta{Name: platform}
		if m, ok := v.(map[string]interface{}); ok {
			if name, ok := m["name"].(string); ok {
				meta.Name = name
			}
			if actions, ok := m["actions"].([]interface{}); ok {
				for _, a := range actions {
					if s, ok := a.(string); ok {
						meta.Actions = append(meta.Actions, s)
					}
				}
			}
		}
		out[platform] = meta
	}
	return out
}

// Supports 判断该音源脚本是否为指定平台声明了指定 action（musicUrl/search 等）。
func (e *Engine) Supports(platform, action string) bool {
	meta, ok := e.Sources()[platform]
	if !ok {
		return false
	}
	return meta.supports(action)
}

func (e *Engine) Close() {
	if e == nil || e.loop == nil {
		return
	}
	e.initOnce.Do(func() {
		e.loop.Stop()
	})
}

func (e *Engine) EventPayload(name string) (any, bool) {
	if e == nil {
		return nil, false
	}
	e.metaMu.RLock()
	defer e.metaMu.RUnlock()
	payload, ok := e.events[name]
	return payload, ok
}

// 核心修复：实现支持 JS 内部 await 的异步网络请求
func requestAsync(vm *goja.Runtime, loop *eventloop.EventLoop, call goja.FunctionCall) goja.Value {
	// 洛雪源脚本里传参是 request(url, options, callback)
	var urlStr string
	arg0 := call.Argument(0).Export()
	options := call.Argument(1).Export()
	callback := call.Argument(2)
	cb, hasCallback := goja.AssertFunction(callback)

	if str, ok := arg0.(string); ok {
		urlStr = str
	} else if obj, ok := arg0.(map[string]interface{}); ok {
		if u, ok := obj["url"].(string); ok {
			urlStr = u
		}
	}

	method := http.MethodGet
	headers := map[string]string{}
	timeout := 15 * time.Second
	var bodyReader io.Reader
	if obj, ok := options.(map[string]interface{}); ok {
		if m, ok := obj["method"].(string); ok && m != "" {
			method = m
		}
		if t, ok := obj["timeout"].(int64); ok && t > 0 {
			timeout = time.Duration(t) * time.Millisecond
		} else if t, ok := obj["timeout"].(float64); ok && t > 0 {
			timeout = time.Duration(t) * time.Millisecond
		}
		if hs, ok := obj["headers"].(map[string]interface{}); ok {
			for k, v := range hs {
				headers[k] = fmt.Sprint(v)
			}
		}
		if body, ok := obj["body"].(string); ok {
			bodyReader = bytes.NewBufferString(body)
		}
	}

	// 同时兼容 callback 风格和 Promise 风格。
	promise, resolve, reject := vm.NewPromise()

	// 开启 Go 的异步协程去请求，绝对不能阻塞当前 JS 主线程
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("❌ [lx-go] request() 异步 goroutine panic recovered: %v\n%s", r, debug.Stack())
			}
		}()

		// 用 context 而不是只靠 http.Client.Timeout：这样请求真正超时/被取消时，
		// 底层连接会被立即中断，不会出现"Go 侧已经不等这个结果了，但 socket
		// 还在傻等到 Timeout 才断开"的悬空请求。
		reqCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		req, err := http.NewRequestWithContext(reqCtx, method, urlStr, bodyReader)
		if err != nil {
			runOnLoopSafe(loop, "request-error", func(vm *goja.Runtime) {
				if hasCallback {
					_, _ = cb(goja.Undefined(), vm.ToValue(map[string]any{"message": err.Error()}), goja.Undefined())
				}
				reject(vm.ToValue(err.Error()))
			})
			return
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		client := http.Client{Timeout: timeout}
		resp, err := client.Do(req)
		if err != nil {
			// 发生错误，必须通过 RunOnLoop 回传给 reject
			runOnLoopSafe(loop, "request-error", func(vm *goja.Runtime) {
				if hasCallback {
					_, _ = cb(goja.Undefined(), vm.ToValue(map[string]any{"message": err.Error()}), goja.Undefined())
				}
				reject(vm.ToValue(err.Error()))
			})
			return
		}
		defer resp.Body.Close()

		// 上游（脚本配置的域名，也可能是被劫持/伪造的域名）返回超大 body 时不能
		// 无限制读进内存，限制一个足够覆盖正常 JSON/歌曲信息响应的上限。
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes))

		// 成功拿到文本（通常是 JSON 字符串），安全回传给 JS 并唤醒其 await
		runOnLoopSafe(loop, "request-success", func(vm *goja.Runtime) {
			result := map[string]any{
				"statusCode": resp.StatusCode,
				"body":       string(bodyBytes),
			}
			if hasCallback {
				_, _ = cb(goja.Undefined(), goja.Null(), vm.ToValue(result))
			}
			resolve(vm.ToValue(result))
		})
	}()

	return vm.ToValue(promise)
}

// maxResponseBodyBytes 是 request() 读取上游响应体的硬上限，防止被恶意/异常
// 的上游拖爆内存。音源脚本请求的都是歌曲搜索/直链这类小体积 JSON 接口，
// 10MB 绰绰有余。
const maxResponseBodyBytes = 10 << 20

// Call 调用 JS 导出的事件处理器
func (e *Engine) Call(ctx context.Context, event string, payload interface{}) (interface{}, error) {
	// 定义一个包装返回结果的结构体
	type resultTuple struct {
		res interface{}
		err error
	}
	ch := make(chan resultTuple, 1)

	// 必须把执行代码的逻辑推入事件循环线程中运行，以防并发死锁
	runOnLoopSafe(e.loop, "call:"+event, func(vm *goja.Runtime) {
		handlerVal, exists := e.handlers[event]
		if !exists || handlerVal == nil {
			ch <- resultTuple{nil, fmt.Errorf("事件监听器 [%s] 未注册", event)}
			return
		}

		// 核心修复点：将 goja.Value 转换为 goja.Callable 接口进行调用
		fn, ok := goja.AssertFunction(handlerVal)
		if !ok {
			ch <- resultTuple{nil, fmt.Errorf("[%s] 对应的绑定项不是一个标准的 JS 函数", event)}
			return
		}

		// 执行 JS 函数
		res, err := fn(goja.Undefined(), vm.ToValue(payload))
		if err != nil {
			ch <- resultTuple{nil, err}
			return
		}

		// 如果调用的 JS 函数是 async 函数，它返回的是一个 Promise
		if promise, ok := res.Export().(*goja.Promise); ok {
			// 已经完成的 Promise 直接返回；Pending 时挂上 then/catch，等异步请求回调继续驱动事件循环。
			switch promise.State() {
			case goja.PromiseStateFulfilled:
				ch <- resultTuple{promise.Result().Export(), nil}
			case goja.PromiseStateRejected:
				ch <- resultTuple{nil, fmt.Errorf("JS Promise 被拒绝: %s", formatJSValue(promise.Result()))}
			case goja.PromiseStatePending:
				promiseObj := res.ToObject(vm)
				thenVal := promiseObj.Get("then")
				then, ok := goja.AssertFunction(thenVal)
				if !ok {
					ch <- resultTuple{nil, fmt.Errorf("函数返回了无法等待的 Promise")}
					return
				}

				onFulfilled := func(call goja.FunctionCall) goja.Value {
					ch <- resultTuple{call.Argument(0).Export(), nil}
					return goja.Undefined()
				}
				onRejected := func(call goja.FunctionCall) goja.Value {
					ch <- resultTuple{nil, fmt.Errorf("JS Promise 被拒绝: %s", formatJSValue(call.Argument(0)))}
					return goja.Undefined()
				}
				if _, err := then(promiseObj, vm.ToValue(onFulfilled), vm.ToValue(onRejected)); err != nil {
					ch <- resultTuple{nil, err}
				}
			}
			return
		}

		ch <- resultTuple{res.Export(), nil}
	})

	// 等待结果或超时控制
	select {
	case r := <-ch:
		return r.res, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func formatJSValue(v goja.Value) string {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return "unknown error"
	}
	exported := v.Export()
	if m, ok := exported.(map[string]any); ok {
		if msg, ok := m["message"]; ok {
			if text := strings.TrimSpace(fmt.Sprint(msg)); text != "" {
				return text
			}
		}
	}
	if s, ok := exported.(string); ok && s != "" {
		return s
	}
	return v.String()
}
