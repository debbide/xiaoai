package lxgo

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Source 是一个已加载好的音源脚本：独立的 goja 引擎 + 从脚本头部注释解析出来的名称。
type Source struct {
	Name   string // 脚本 @name，取不到则回退为文件名（不含扩展名）
	File   string // 文件名，仅用于日志/诊断
	Engine *Engine
}

// Registry 管理 js/ 目录下加载成功的所有音源脚本，并按加载顺序（即文件名排序）
// 作为默认优先级：同一平台/同一 action，排在前面的音源先尝试，失败或未命中再
// 自动切换到下一个。
type Registry struct {
	Sources []*Source
}

var nameCommentRe = regexp.MustCompile(`(?m)^\s*[*/]*\s*@name\s+(.+?)\s*$`)

// 启动自检：用固定的测试用例（周杰伦《稻香》）实际调用一次音源脚本，返回不了
// 结果的音源不会被加入音源列表，避免一个真正打不通的脚本混进去、白白拖慢/拖垮
// 每次请求的 fallback 链路。
//
// 优先用 search 自检（覆盖面广，脚本自己声明支持哪些平台就测哪些）；如果脚本
// 完全不支持 search（目前 js/ 目录下大部分第三方脚本都是这样，只做 musicUrl），
// 退化成用一个已知有效的网易云 songId 测一次 musicUrl。两者都不支持的脚本
// （既没有 search 也没有 wy 的 musicUrl）没法用这个测试用例验证，直接放行加入。
const (
	selfTestKeyword = "稻香"
	selfTestTimeout = 12 * time.Second
)

// selfTestPlatforms 是自检时尝试平台的优先顺序，wy 排最前是因为我们只准备了
// wy 平台的已知有效 musicUrl 测试用例（网易云 id 3357698666《稻香》/周杰伦）。
var selfTestPlatforms = []string{"wy", "tx", "kw", "kg", "mg"}

var selfTestMusicInfoWY = map[string]any{
	"id":     "3357698666",
	"name":   "稻香",
	"singer": "周杰伦",
}

func skipSelfTest() bool {
	return os.Getenv("LX_GO_SKIP_SELFTEST") == "1"
}

// selfTestSource 对刚加载好的音源脚本做一次真实的网络自检，返回
// (是否通过, 用于日志的说明文字)。
func selfTestSource(engine *Engine) (bool, string) {
	if skipSelfTest() {
		return true, "已跳过自检 (LX_GO_SKIP_SELFTEST=1)"
	}

	meta := engine.Sources()

	for _, platform := range selfTestPlatforms {
		m, ok := meta[platform]
		if !ok || !m.supports("search") {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), selfTestTimeout)
		result, err := engine.Call(ctx, "request", map[string]any{
			"action": "search",
			"source": platform,
			"info":   map[string]any{"keyword": selfTestKeyword, "limit": 3},
		})
		cancel()
		if err != nil {
			return false, fmt.Sprintf("自检失败: search(%s, %q) 报错: %v", platform, selfTestKeyword, err)
		}
		if isEmptyResult(result) {
			return false, fmt.Sprintf("自检失败: search(%s, %q) 返回空结果", platform, selfTestKeyword)
		}
		return true, fmt.Sprintf("自检通过: search(%s, %q) 有返回结果", platform, selfTestKeyword)
	}

	if m, ok := meta["wy"]; ok && m.supports("musicUrl") {
		ctx, cancel := context.WithTimeout(context.Background(), selfTestTimeout)
		result, err := engine.Call(ctx, "request", map[string]any{
			"action": "musicUrl",
			"source": "wy",
			"info":   map[string]any{"type": "128k", "musicInfo": selfTestMusicInfoWY},
		})
		cancel()
		if err != nil {
			return false, fmt.Sprintf("自检失败: musicUrl(wy, 稻香) 报错: %v", err)
		}
		if extractURL(result) == "" {
			return false, "自检失败: musicUrl(wy, 稻香) 未返回有效直链"
		}
		return true, "自检通过: musicUrl(wy, 稻香) 返回了有效直链"
	}

	return true, "既不支持 search 也不支持 wy 的 musicUrl，无法用测试用例自检，直接放行"
}

// parseScriptName 从脚本头部注释（/*! ... */ 或 /** ... */）里提取 @name 后面的值。
// 取不到时返回空字符串，由调用方决定回退策略（一般回退为文件名）。
func parseScriptName(source string) string {
	// 只在文件开头的注释块内查找，避免误命中脚本正文里的字符串
	end := strings.Index(source, "*/")
	head := source
	if end >= 0 && end < 4000 {
		head = source[:end]
	} else if len(source) > 4000 {
		head = source[:4000]
	}
	m := nameCommentRe.FindStringSubmatch(head)
	if len(m) < 2 {
		return ""
	}
	name := strings.TrimSpace(m[1])
	// 部分脚本头部注释里 @name 后面紧跟换行里的中文简介会被贪婪匹配进来，
	// 这里只取第一行，避免名字里混入换行符。
	if idx := strings.IndexAny(name, "\r\n"); idx >= 0 {
		name = strings.TrimSpace(name[:idx])
	}
	return name
}

// LoadSources 扫描 dir 目录下所有 *.js 文件并逐个加载：
//   - 按文件名排序，顺序即默认的音源优先级（先加载的先尝试）
//   - 每个脚本独立初始化，某个脚本加载失败（JS 语法错误等）只跳过它本身，
//     记录日志后继续加载其余脚本，不影响整个服务启动
//   - 若多个脚本解析出同名 @name，只加载第一个，后面同名的会被跳过并记录日志
//     （用户可以把不想用的脚本从 js/ 目录里移走，或改一下自己的 @name 来消除冲突）
func LoadSources(dir string) (*Registry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read js dir %q: %w", dir, err)
	}

	var files []string
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		if !strings.HasSuffix(strings.ToLower(ent.Name()), ".js") {
			continue
		}
		files = append(files, ent.Name())
	}
	sort.Strings(files)

	reg := &Registry{}
	seen := make(map[string]string) // name -> 已加载的文件名，用于检测冲突

	for _, file := range files {
		full := filepath.Join(dir, file)
		raw, err := os.ReadFile(full)
		if err != nil {
			log.Printf("⚠️  [lx-go] 读取音源文件失败，已跳过: %s (%v)", file, err)
			continue
		}

		name := parseScriptName(string(raw))
		if name == "" {
			name = strings.TrimSuffix(file, filepath.Ext(file))
			log.Printf("⚠️  [lx-go] 音源 %s 未找到 @name 注释，回退使用文件名作为音源名", file)
		}

		if existing, dup := seen[name]; dup {
			log.Printf("⚠️  [lx-go] 音源名称冲突: %s (文件: %s) 与已加载的 %s 同名，已跳过，只使用先加载的那个", name, file, existing)
			continue
		}

		engine, err := NewEngine(name, string(raw))
		if err != nil {
			log.Printf("❌ [lx-go] 音源加载失败，已跳过: %s (文件: %s) 错误: %v", name, file, err)
			continue
		}

		log.Printf("🧪 [lx-go] 正在自检音源: %s (文件: %s)，测试用例: 稻香/周杰伦 ...", name, file)
		if ok, info := selfTestSource(engine); !ok {
			log.Printf("❌ [lx-go] %s，未加入音源列表: %s (文件: %s)，请检查该脚本或其上游接口是否可用", info, name, file)
			engine.Close()
			continue
		} else {
			log.Printf("✅ [lx-go] %s: %s", name, info)
		}

		seen[name] = file
		reg.Sources = append(reg.Sources, &Source{Name: name, File: file, Engine: engine})
		log.Printf("✅ [lx-go] 已加载音源: %s (文件: %s)，支持平台: %s", name, file, summarizeSources(engine.Sources()))
	}

	// 大多数第三方音源脚本只实现 musicUrl（取直链），不实现 search（搜索）。
	// 补一个内置的搜索兜底源，保证 GET /api/music/search 至少对 wy/kw/mg 可用；
	// 优先级最低，用户自己放的脚本如果以后也支持了 search 会优先被使用。
	if _, dup := seen[builtinSearchSourceName]; !dup {
		if engine, err := NewEngine(builtinSearchSourceName, builtinSearchScript); err != nil {
			log.Printf("❌ [lx-go] 内置搜索源加载失败: %v", err)
		} else if ok, info := selfTestSource(engine); !ok {
			log.Printf("❌ [lx-go] 内置搜索源自检失败，未加入音源列表: %s", info)
			engine.Close()
		} else {
			reg.Sources = append(reg.Sources, &Source{Name: builtinSearchSourceName, File: "(内置)", Engine: engine})
			log.Printf("✅ [lx-go] 已加载音源: %s (内置)，%s，支持平台: %s", builtinSearchSourceName, info, summarizeSources(engine.Sources()))
		}
	}

	if len(reg.Sources) == 0 {
		log.Printf("⚠️  [lx-go] 没有任何音源脚本加载成功，search/url 接口将始终失败")
	}

	return reg, nil
}

func summarizeSources(sources map[string]SourceMeta) string {
	if len(sources) == 0 {
		return "(无，脚本未上报 inited.sources)"
	}
	keys := make([]string, 0, len(sources))
	for k := range sources {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s%v", k, sources[k].Actions))
	}
	return strings.Join(parts, ", ")
}

// Close 关闭所有已加载音源的 goja 引擎（停止各自的事件循环）。
func (r *Registry) Close() {
	if r == nil {
		return
	}
	for _, s := range r.Sources {
		s.Engine.Close()
	}
}

// Candidates 返回按优先级排好序、且为 platform 声明了 action 能力的音源列表。
func (r *Registry) Candidates(platform, action string) []*Source {
	if r == nil {
		return nil
	}
	out := make([]*Source, 0, len(r.Sources))
	for _, s := range r.Sources {
		if s.Engine.Supports(platform, action) {
			out = append(out, s)
		}
	}
	return out
}

// Search 在已加载的、为 platform 声明了 search 能力的音源里按加载优先级依次尝试，
// 返回第一个成功且非空的结果，以及实际命中的音源名称。
//
// 这是给非 HTTP 场景（比如把 pkg/lx-go 当库直接 import 内嵌到别的 Go 程序里）用的
// 底层能力；HTTP 层的 GET /api/music/search（见 api.go）在这个基础上还加了一层
// 响应缓存和"默认搜索音源优先"的调整，不直接复用这个方法。
func (r *Registry) Search(ctx context.Context, platform, keyword string, limit int) ([]map[string]any, string, error) {
	candidates := r.Candidates(platform, "search")
	if len(candidates) == 0 {
		return nil, "", fmt.Errorf("no loaded source supports search for platform %q", platform)
	}

	payload := map[string]any{"keyword": keyword}
	if limit > 0 {
		payload["limit"] = limit
	}

	var lastErr error
	for _, src := range candidates {
		result, err := src.Engine.Call(ctx, "request", map[string]any{
			"action": "search",
			"source": platform,
			"info":   payload,
		})
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", src.Name, err)
			continue
		}
		if isEmptyResult(result) {
			lastErr = fmt.Errorf("%s: empty result", src.Name)
			continue
		}
		items, convErr := toSongList(result)
		if convErr != nil {
			lastErr = fmt.Errorf("%s: %w", src.Name, convErr)
			continue
		}
		return items, src.Name, nil
	}
	if lastErr != nil {
		return nil, "", lastErr
	}
	return nil, "", fmt.Errorf("all sources failed")
}

// ResolveURL 在已加载的、为 platform 声明了 musicUrl 能力的音源里按加载优先级
// 依次尝试，返回第一个成功解析出的直链，以及实际命中的音源名称。
//
// songInfo 一般就是 Search() 返回结果里的某一项（或者外部搜索来源，比如
// pkg/music 本地曲库/网易云搜索接口返回的等价结构）。
func (r *Registry) ResolveURL(ctx context.Context, platform string, songInfo map[string]any, quality string) (string, string, error) {
	candidates := r.Candidates(platform, "musicUrl")
	if len(candidates) == 0 {
		return "", "", fmt.Errorf("no loaded source supports musicUrl for platform %q", platform)
	}

	var lastErr error
	for _, src := range candidates {
		result, err := src.Engine.Call(ctx, "request", map[string]any{
			"action": "musicUrl",
			"source": platform,
			"info": map[string]any{
				"type":      quality,
				"musicInfo": songInfo,
			},
		})
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", src.Name, err)
			continue
		}
		u := extractURL(result)
		if u == "" {
			lastErr = fmt.Errorf("%s: empty url", src.Name)
			continue
		}
		return u, src.Name, nil
	}
	if lastErr != nil {
		return "", "", lastErr
	}
	return "", "", fmt.Errorf("all sources failed")
}

func toSongList(result any) ([]map[string]any, error) {
	arr, ok := result.([]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected search result shape: %T", result)
	}
	out := make([]map[string]any, 0, len(arr))
	for _, item := range arr {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// AllSourcesSnapshot 汇总所有已加载音源的 inited.sources 元信息，用于 /health 诊断。
func (r *Registry) AllSourcesSnapshot() map[string]any {
	out := make(map[string]any, len(r.Sources))
	for _, s := range r.Sources {
		out[s.Name] = map[string]any{
			"file":    s.File,
			"sources": s.Engine.Sources(),
		}
	}
	return out
}
