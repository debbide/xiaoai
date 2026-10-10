package lxgo

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	id3v2 "github.com/bogem/id3v2/v2"
)

const (
	searchCacheTTL = 30 * time.Second
	// urlCacheTTL 之前是 10 分钟，接入 pkg/music 联调时发现这个时间太长：
	// 不少免费音源后端返回的网易云 CDN 直链本身可能在一两分钟内就失效（服务端会
	// 返回 "auth failed - expired url"），缓存太久会导致 /api/music/url 明明返回
	// 200，实际链接却已经放不了。这里只保留很短的时间，主要用来去重"短时间内重复
	// 点了好几次同一首歌"这种情况，不再追求长时间复用。
	urlCacheTTL       = 20 * time.Second
	sourceRequestTime = 20 * time.Second
)

// Server 聚合了从 js/ 目录加载出来的所有音源脚本（Registry），对外暴露
// 兼容 pkg/music（LXClient）的 HTTP 接口。
type Server struct {
	registry *Registry
	progress *ProgressHub

	cacheMu sync.Mutex
	cache   map[string]cacheEntry
	closeCh chan struct{}

	// defaultSearchMu/defaultSearchName 是当前"默认搜索音源"，由
	// GET /api/music/switch-source 手动切换；GET /api/music/search 会把它
	// 排到 fallback 顺序的最前面优先尝试。
	defaultSearchMu   sync.Mutex
	defaultSearchName string
}

type cacheEntry struct {
	value     any
	expiresAt time.Time
}

// cacheSweepInterval 是清理过期缓存条目的后台扫描周期。cacheGet 本身在命中
// 同一个 key 时也会顺手清理过期项，但从未被再次访问的 key（比如攻击者/异常
// 客户端不断换关键词、每次都是新 key）不会走到那条路径，只能靠这个后台扫描
// 兜底，否则 cache map 会无限增长（内存泄漏 / 事实上的 DoS 面）。
const cacheSweepInterval = time.Minute

func NewServer(registry *Registry) *Server {
	s := &Server{
		registry: registry,
		progress: NewProgressHub(),
		cache:    make(map[string]cacheEntry),
		closeCh:  make(chan struct{}),
	}
	go s.sweepCacheLoop()
	return s
}

// Close 停止后台缓存清理协程。跟 registry.Close() 是分开的两件事：Server 只
// 负责 HTTP 层的缓存/进度状态，音源脚本自己的 goja 事件循环由 Registry 管理。
func (s *Server) Close() {
	if s == nil || s.closeCh == nil {
		return
	}
	select {
	case <-s.closeCh:
		// 已经关过了
	default:
		close(s.closeCh)
	}
}

func (s *Server) sweepCacheLoop() {
	ticker := time.NewTicker(cacheSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.sweepExpiredCache()
		case <-s.closeCh:
			return
		}
	}
}

func (s *Server) sweepExpiredCache() {
	now := time.Now()
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	for k, entry := range s.cache {
		if now.After(entry.expiresAt) {
			delete(s.cache, k)
		}
	}
}

func (s *Server) cacheGet(key string) (any, bool) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	entry, ok := s.cache[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(entry.expiresAt) {
		delete(s.cache, key)
		return nil, false
	}
	return entry.value, true
}

func (s *Server) cacheSet(key string, value any, ttl time.Duration) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	s.cache[key] = cacheEntry{value: value, expiresAt: time.Now().Add(ttl)}
}

func makeCacheKey(parts ...string) string {
	h := sha1.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ---------------------------------------------------------------------
// GET /api/music/search?name=<keyword>&source=<wy|tx|kw|kg|mg>&page=&pages=&limit=
// 与 pkg/music/lx.go 里的 LXClient.search() 完全对齐：裸数组响应，
// 每一项至少带 name/singer/source 字段。
// ---------------------------------------------------------------------
func (s *Server) handleMusicSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	keyword := strings.TrimSpace(firstNonEmpty(query.Get("name"), query.Get("q")))
	if keyword == "" {
		http.Error(w, "missing name", http.StatusBadRequest)
		return
	}
	platform := strings.TrimSpace(query.Get("source"))
	if platform == "" {
		http.Error(w, "missing source", http.StatusBadRequest)
		return
	}
	limit := query.Get("limit")

	cacheKey := makeCacheKey("search", keyword, platform, limit)
	if cached, ok := s.cacheGet(cacheKey); ok {
		writeJSON(w, cached)
		return
	}

	candidates := s.registry.Candidates(platform, "search")
	if len(candidates) == 0 {
		http.Error(w, fmt.Sprintf("no loaded source supports search for platform %q", platform), http.StatusNotFound)
		return
	}
	candidates = s.reorderWithDefaultSearchFirst(candidates)

	ctx, cancel := context.WithTimeout(r.Context(), sourceRequestTime)
	defer cancel()

	payload := map[string]any{"keyword": keyword}
	if limit != "" {
		payload["limit"] = limit
	}

	var lastErr error
	for _, src := range candidates {
		log.Printf("🔍 [music/search] 当前使用音源: %s，platform=%s keyword=%q", src.Name, platform, keyword)
		result, err := src.Engine.Call(ctx, "request", map[string]any{
			"action": "search",
			"source": platform,
			"info":   payload,
		})
		if err != nil {
			log.Printf("⚠️  [music/search] 音源 %s 未找到结果 (%v)，自动切换下一个音源", src.Name, err)
			lastErr = err
			continue
		}
		if isEmptyResult(result) {
			log.Printf("⚠️  [music/search] 音源 %s 返回空结果，自动切换下一个音源", src.Name)
			lastErr = fmt.Errorf("%s: empty result", src.Name)
			continue
		}
		log.Printf("✅ [music/search] 音源 %s 命中，platform=%s keyword=%q", src.Name, platform, keyword)
		s.cacheSet(cacheKey, result, searchCacheTTL)
		writeJSON(w, result)
		return
	}

	msg := "all sources failed"
	if lastErr != nil {
		msg = fmt.Sprintf("all sources failed, last error: %v", lastErr)
	}
	http.Error(w, msg, http.StatusInternalServerError)
}

// ---------------------------------------------------------------------
// POST /api/music/url
// body: {"songInfo": {...,"source":"wy"}, "quality": "320k"}
// 可选 header: x-req-id，用于 GET /api/music/progress?reqId=xxx 订阅 SSE 解析进度。
// 响应: {"url": "...", "type": "320k"}，与 pkg/music/lx.go 的 lxURLResponse 对齐。
// ---------------------------------------------------------------------
func (s *Server) handleMusicURL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	reqID := strings.TrimSpace(r.Header.Get(progressHeader))

	var body struct {
		SongInfo map[string]any `json:"songInfo"`
		Quality  string         `json:"quality"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.SongInfo == nil {
		http.Error(w, "missing songInfo", http.StatusBadRequest)
		return
	}
	platform := stringField(body.SongInfo, "source")
	if platform == "" {
		platform = strings.TrimSpace(r.URL.Query().Get("source"))
	}
	if platform == "" {
		http.Error(w, "missing songInfo.source", http.StatusBadRequest)
		return
	}
	quality := firstNonEmpty(body.Quality, "128k")

	cacheKey := makeCacheKey("url", platform, quality, canonicalJSON(body.SongInfo))
	if cached, ok := s.cacheGet(cacheKey); ok {
		s.progress.Publish(reqID, ProgressEvent{Stage: "success", Platform: platform, Message: "命中缓存", Done: true})
		writeJSON(w, cached)
		return
	}

	candidates := s.registry.Candidates(platform, "musicUrl")
	if len(candidates) == 0 {
		msg := fmt.Sprintf("no loaded source supports musicUrl for platform %q", platform)
		s.progress.Publish(reqID, ProgressEvent{Stage: "error", Platform: platform, Message: msg, Done: true})
		http.Error(w, msg, http.StatusNotFound)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), sourceRequestTime)
	defer cancel()

	var lastErr error
	for _, src := range candidates {
		log.Printf("🌐 [music/url] 当前使用音源: %s，platform=%s quality=%s", src.Name, platform, quality)
		s.progress.Publish(reqID, ProgressEvent{Stage: "trying", Source: src.Name, Platform: platform, Message: "正在尝试音源 " + src.Name})

		result, err := src.Engine.Call(ctx, "request", map[string]any{
			"action": "musicUrl",
			"source": platform,
			"info": map[string]any{
				"type":      quality,
				"musicInfo": body.SongInfo,
			},
		})
		if err != nil {
			log.Printf("⚠️  [music/url] 音源 %s 解析失败 (%v)，自动切换下一个音源", src.Name, err)
			s.progress.Publish(reqID, ProgressEvent{Stage: "failed", Source: src.Name, Platform: platform, Message: err.Error()})
			lastErr = err
			continue
		}

		remoteURL := extractURL(result)
		if remoteURL == "" {
			log.Printf("⚠️  [music/url] 音源 %s 返回了空链接，自动切换下一个音源", src.Name)
			s.progress.Publish(reqID, ProgressEvent{Stage: "failed", Source: src.Name, Platform: platform, Message: "empty url"})
			lastErr = fmt.Errorf("%s: empty url", src.Name)
			continue
		}

		log.Printf("✅ [music/url] 音源 %s 解析成功: %s", src.Name, remoteURL)
		s.progress.Publish(reqID, ProgressEvent{Stage: "success", Source: src.Name, Platform: platform, Message: remoteURL, Done: true})

		resp := map[string]any{"url": remoteURL, "type": quality}
		s.cacheSet(cacheKey, resp, urlCacheTTL)
		writeJSON(w, resp)
		return
	}

	msg := "all sources failed"
	if lastErr != nil {
		msg = fmt.Sprintf("all sources failed, last error: %v", lastErr)
	}
	s.progress.Publish(reqID, ProgressEvent{Stage: "error", Platform: platform, Message: msg, Done: true})
	http.Error(w, msg, http.StatusInternalServerError)
}

// ---------------------------------------------------------------------
// GET /api/music/progress?reqId=xxx
// SSE：订阅 POST /api/music/url（同一个 x-req-id）解析过程中的实时进度。
// ---------------------------------------------------------------------
func (s *Server) handleMusicProgress(w http.ResponseWriter, r *http.Request) {
	reqID := strings.TrimSpace(r.URL.Query().Get("reqId"))
	if reqID == "" {
		http.Error(w, "missing reqId", http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, cancel := s.progress.Subscribe(reqID)
	defer cancel()

	timeout := time.NewTimer(progressSubscribeTimeout)
	defer timeout.Stop()

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", ev.marshal())
			flusher.Flush()
			if ev.Done {
				return
			}
			if !timeout.Stop() {
				<-timeout.C
			}
			timeout.Reset(progressSubscribeTimeout)
		case <-timeout.C:
			return
		case <-r.Context().Done():
			return
		}
	}
}

// ---------------------------------------------------------------------
// GET /api/music/download?url=&filename=&tag=1&name=&singer=&album=&pic=
// 代理下载 url 指向的音频文件；tag!=0（默认 1）时会在转发前注入 ID3 标签
// （标题/歌手/专辑/封面）。
// ---------------------------------------------------------------------
func (s *Server) handleMusicDownload(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	remoteURL := strings.TrimSpace(query.Get("url"))
	if remoteURL == "" {
		http.Error(w, "missing url", http.StatusBadRequest)
		return
	}
	filename := strings.TrimSpace(query.Get("filename"))
	if filename == "" {
		filename = filepath.Base(remoteURL)
	}
	if filename == "" || filename == "." || filename == "/" {
		filename = "download.mp3"
	}
	injectTag := query.Get("tag") != "0" // 默认注入标签，tag=0 才关闭

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, remoteURL, nil)
	if err != nil {
		http.Error(w, "invalid url: "+err.Error(), http.StatusBadRequest)
		return
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	client := http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "download failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		http.Error(w, fmt.Sprintf("download failed: upstream status=%d", resp.StatusCode), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", firstNonEmpty(resp.Header.Get("Content-Type"), "audio/mpeg"))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))

	name := query.Get("name")
	singer := query.Get("singer")
	album := query.Get("album")
	pic := strings.TrimSpace(query.Get("pic"))

	if !injectTag || (name == "" && singer == "" && album == "" && pic == "") {
		log.Printf("📥 [music/download] 直接转发（不注入标签）: %s -> %s", remoteURL, filename)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	log.Printf("📥 [music/download] 代理下载并注入 ID3 标签: %s -> %s (name=%q singer=%q album=%q pic=%v)",
		remoteURL, filename, name, singer, album, pic != "")

	tmpPath, err := saveToTempFile(resp.Body)
	if err != nil {
		http.Error(w, "buffer audio failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.Remove(tmpPath)

	if err := injectID3Tag(r.Context(), tmpPath, name, singer, album, pic); err != nil {
		// 注入标签失败不应该让整次下载失败，退化为直接转发原始文件。
		log.Printf("⚠️  [music/download] 注入 ID3 标签失败，退化为原始文件: %v", err)
	}

	f, err := os.Open(tmpPath)
	if err != nil {
		http.Error(w, "read tagged audio failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	_, _ = io.Copy(w, f)
}

func saveToTempFile(r io.Reader) (string, error) {
	f, err := os.CreateTemp("", "lx-go-download-*.mp3")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// injectID3Tag 就地给 tmpPath 指向的 mp3 文件写入 ID3v2 标签（标题/歌手/专辑/封面）。
// 使用基于文件的 Open+Save，而不是手动拼字节，是因为 id3v2 库在 Save() 内部会正确处理
// "跳过原有标签、只重写标签头部分" 的偏移量计算，比自己按 tag.Size() 手动切片更可靠
// （tag.Size() 反映的是修改后新标签的大小，不是原始文件里旧标签的大小）。
func injectID3Tag(ctx context.Context, tmpPath, name, singer, album, pic string) error {
	tag, err := id3v2.Open(tmpPath, id3v2.Options{Parse: true})
	if err != nil {
		return fmt.Errorf("open id3 tag: %w", err)
	}
	defer tag.Close()

	if name != "" {
		tag.SetTitle(name)
	}
	if singer != "" {
		tag.SetArtist(singer)
	}
	if album != "" {
		tag.SetAlbum(album)
	}
	if pic != "" {
		if picBytes, mimeType, err := fetchPicture(ctx, pic); err != nil {
			log.Printf("⚠️  [music/download] 下载封面失败，跳过封面注入: %v", err)
		} else {
			tag.AddAttachedPicture(id3v2.PictureFrame{
				Encoding:    id3v2.EncodingUTF8,
				MimeType:    mimeType,
				PictureType: id3v2.PTFrontCover,
				Description: "Cover",
				Picture:     picBytes,
			})
		}
	}

	return tag.Save()
}

func fetchPicture(ctx context.Context, picURL string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, picURL, nil)
	if err != nil {
		return nil, "", err
	}
	client := http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("status=%d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 封面最多读 8MB，避免异常大文件
	if err != nil {
		return nil, "", err
	}
	mimeType := resp.Header.Get("Content-Type")
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	if idx := strings.Index(mimeType, ";"); idx >= 0 {
		mimeType = mimeType[:idx]
	}
	return data, mimeType, nil
}

// ---------------------------------------------------------------------
// GET /health、GET /sources：诊断用，汇总所有已加载音源的状态。
// ---------------------------------------------------------------------
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	names := make([]string, 0, len(s.registry.Sources))
	for _, src := range s.registry.Sources {
		names = append(names, src.Name)
	}
	writeJSON(w, map[string]any{
		"ok":      true,
		"sources": names,
	})
}

func (s *Server) handleSources(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.registry.AllSourcesSnapshot())
}

// ---------------------------------------------------------------------
// 小工具函数
// ---------------------------------------------------------------------

func isEmptyResult(v any) bool {
	if v == nil {
		return true
	}
	if arr, ok := v.([]interface{}); ok {
		return len(arr) == 0
	}
	return false
}

// extractURL 兼容两种脚本返回形态：直接返回字符串直链，或返回
// { url: "..." } 这样的对象（部分脚本还会带上 lyric/cover 等附加信息）。
func extractURL(v any) string {
	switch val := v.(type) {
	case string:
		return val
	case map[string]interface{}:
		if u, ok := val["url"].(string); ok {
			return u
		}
	}
	return ""
}

func stringField(data map[string]any, key string) string {
	if v, ok := data[key].(string); ok {
		return v
	}
	return ""
}

func firstNonEmpty(value string, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func canonicalJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(data)
}

func writeJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		// 这时候响应头已经发出去了，没法再改成 500 了，只能记日志方便排查
		// "客户端收到截断 JSON" 这类问题。
		log.Printf("⚠️  [lx-go] writeJSON encode failed: %v", err)
	}
}
