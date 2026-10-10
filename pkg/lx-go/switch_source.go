package lxgo

import (
	"fmt"
	"log"
	"net/http"
	"runtime/debug"
)

// recoverMiddleware 给 handler 包一层 panic 恢复：goja/JS 脚本这条链路本来就
// 有类型断言失败、脚本抛出非预期形状异常等风险，一旦某个请求触发 panic 而没有
// recover，Go 默认行为是直接崩掉整个进程（http.Server 本身只保证按 goroutine
// 隔离，但不 recover 业务 panic）。这里统一兜底，panic 时记日志、返回 500，
// 不让一次请求的异常影响其它正在处理的请求。
func recoverMiddleware(name string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("❌ [lx-go] handler panic recovered (%s %s): %v\n%s", name, r.URL.Path, rec, debug.Stack())
				http.Error(w, fmt.Sprintf("internal error: %v", rec), http.StatusInternalServerError)
			}
		}()
		h(w, r)
	}
}

// searchPool 返回所有为任意平台声明了 search 能力的音源，顺序与 Registry 的
// 加载顺序（默认优先级）一致。这是 "默认搜索音源" 轮换的候选池。
func (s *Server) searchPool() []*Source {
	out := make([]*Source, 0, len(s.registry.Sources))
	for _, src := range s.registry.Sources {
		for _, meta := range src.Engine.Sources() {
			if meta.supports("search") {
				out = append(out, src)
				break
			}
		}
	}
	return out
}

// currentDefaultSearch 返回当前的默认搜索音源；如果还没手动切换过，或者上次
// 选中的音源已经不在池子里了（比如重启后音源列表变了），回退成池子里的第一个。
func (s *Server) currentDefaultSearch(pool []*Source) *Source {
	if len(pool) == 0 {
		return nil
	}
	s.defaultSearchMu.Lock()
	name := s.defaultSearchName
	s.defaultSearchMu.Unlock()

	for _, src := range pool {
		if src.Name == name {
			return src
		}
	}
	// 还没设置过，或者设置的那个已经不在池子里了，默认用第一个。
	s.defaultSearchMu.Lock()
	s.defaultSearchName = pool[0].Name
	s.defaultSearchMu.Unlock()
	return pool[0]
}

// reorderWithDefaultSearchFirst 把当前默认搜索音源挪到 candidates 列表最前面，
// 其余保持原有优先级顺序不变。默认音源如果本来就不支持这次请求的 platform
// （不在 candidates 里），则不做任何调整。
func (s *Server) reorderWithDefaultSearchFirst(candidates []*Source) []*Source {
	pool := s.searchPool()
	def := s.currentDefaultSearch(pool)
	if def == nil {
		return candidates
	}
	idx := -1
	for i, c := range candidates {
		if c == def {
			idx = i
			break
		}
	}
	if idx <= 0 {
		return candidates
	}
	reordered := make([]*Source, 0, len(candidates))
	reordered = append(reordered, def)
	reordered = append(reordered, candidates[:idx]...)
	reordered = append(reordered, candidates[idx+1:]...)
	return reordered
}

// SwitchToNextSearchSource 把默认搜索音源切换成池子里的下一个（按加载顺序循环），
// 返回 (切换前, 切换后)。池子为空时两个返回值都是 nil。
func (s *Server) SwitchToNextSearchSource() (previous, current *Source) {
	pool := s.searchPool()
	if len(pool) == 0 {
		return nil, nil
	}
	previous = s.currentDefaultSearch(pool)

	idx := 0
	for i, src := range pool {
		if src == previous {
			idx = i
			break
		}
	}
	next := pool[(idx+1)%len(pool)]

	s.defaultSearchMu.Lock()
	s.defaultSearchName = next.Name
	s.defaultSearchMu.Unlock()

	return previous, next
}

// RegisterRoutes 把这个 Server 的所有 HTTP 路由注册到给定的 mux 上。
// 独立跑（cmd/lxgo-server）和被别的服务内嵌（比如 pkg/music 把它挂到自己的
// mux 上）都可以用这个方法，不用关心具体 handler 叫什么名字。
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	// 兼容 pkg/music（LXClient）期望的 "LX Sync Server" 接口
	mux.HandleFunc("/api/music/search", recoverMiddleware("search", s.handleMusicSearch))
	mux.HandleFunc("/api/music/url", recoverMiddleware("url", s.handleMusicURL))
	mux.HandleFunc("/api/music/progress", recoverMiddleware("progress", s.handleMusicProgress))
	mux.HandleFunc("/api/music/download", recoverMiddleware("download", s.handleMusicDownload))
	mux.HandleFunc("/api/music/switch-source", recoverMiddleware("switch-source", s.handleSwitchSearchSource))

	// 诊断用
	mux.HandleFunc("/health", recoverMiddleware("health", s.handleHealth))
	mux.HandleFunc("/sources", recoverMiddleware("sources", s.handleSources))
}

// ---------------------------------------------------------------------
// GET /api/music/switch-source
// 手动把"默认搜索音源"切换到下一个（按音源加载顺序循环），并把切换结果打到
// 日志和响应里。GET /api/music/search 之后会优先用这个默认音源。
// ---------------------------------------------------------------------
func (s *Server) handleSwitchSearchSource(w http.ResponseWriter, r *http.Request) {
	previous, current := s.SwitchToNextSearchSource()
	if current == nil {
		http.Error(w, "no source supports search, nothing to switch", http.StatusNotFound)
		return
	}

	prevName := "(未设置)"
	if previous != nil {
		prevName = previous.Name
	}
	log.Printf("🔀 [music/switch-source] 已切换默认搜索音源: %s -> %s", prevName, current.Name)

	pool := s.searchPool()
	poolNames := make([]string, 0, len(pool))
	for _, src := range pool {
		poolNames = append(poolNames, src.Name)
	}

	writeJSON(w, map[string]any{
		"previous": prevName,
		"current":  current.Name,
		"pool":     poolNames,
	})
}
