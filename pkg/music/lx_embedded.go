package music

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	lxgo "github.com/cxjava/open-xiaoai/pkg/lx-go"
)

// EmbeddedLX 是 lxResolver 的另一种实现：直接在同一个进程里跑 pkg/lx-go 的引擎
// （goja 运行时 + js/ 目录下的音源脚本），不需要手动另起一个独立的 lx-go 服务
// 进程，也不走本地回环端口——搜索/取直链都是进程内的直接函数调用。
//
// 跟基于 HTTP 的 LXClient（见 lx.go）实现的是同一个 lxResolver 接口
// （Resolve/Download），对 Module 来说两者完全可以互换：New() 时根据
// cfg.LX.Embedded 选其中一种。
type EmbeddedLX struct {
	registry *lxgo.Registry
	source   string
	quality  string
}

// NewEmbeddedLX 加载 cfg.EmbeddedJSDir 目录下的音源脚本并启动内嵌 lx-go 引擎。
// 每个脚本在加载时都会做一次真实的自检（见 pkg/lx-go 的说明），自检不通过的
// 音源不会被加入，日志里会打印清楚原因。
func NewEmbeddedLX(cfg *LXConfig) (*EmbeddedLX, error) {
	if cfg == nil {
		return nil, fmt.Errorf("lx config is nil")
	}
	if strings.TrimSpace(cfg.EmbeddedJSDir) == "" {
		return nil, fmt.Errorf("music.lx.embedded_js_dir 未配置，内嵌模式需要指定音源脚本目录（一般是 pkg/lx-go/js）")
	}
	if info, err := os.Stat(cfg.EmbeddedJSDir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("music.lx.embedded_js_dir 不是有效目录: %s", cfg.EmbeddedJSDir)
	}

	log.Printf("🌐 [music/lx-embedded] 正在加载内嵌 lx-go 音源: %s", cfg.EmbeddedJSDir)
	registry, err := lxgo.LoadSources(cfg.EmbeddedJSDir)
	if err != nil {
		return nil, fmt.Errorf("加载内嵌 lx-go 音源失败: %w", err)
	}
	if len(registry.Sources) == 0 {
		log.Printf("⚠️ [music/lx-embedded] 没有任何音源自检通过，LX 在线兜底暂时不可用（本地曲库仍可正常使用）")
	} else {
		log.Printf("✅ [music/lx-embedded] 内嵌 lx-go 加载完成，共 %d 个音源可用", len(registry.Sources))
	}

	quality := cfg.Quality
	if quality == "" {
		quality = "128k"
	}
	return &EmbeddedLX{registry: registry, source: cfg.Source, quality: quality}, nil
}

// Close 关闭内嵌引擎（停止所有音源脚本各自的 goja 事件循环）。Module.Stop() 时调用。
func (e *EmbeddedLX) Close() error {
	if e == nil {
		return nil
	}
	e.registry.Close()
	return nil
}

// Resolve 实现 lxResolver：搜索关键词并返回第一首歌的播放直链。
// 全程是进程内函数调用（Registry.Search -> Registry.ResolveURL），
// 不经过任何网络请求或本地端口。
func (e *EmbeddedLX) Resolve(ctx context.Context, keyword string) (*LXTrack, error) {
	if e == nil || e.registry == nil {
		return nil, fmt.Errorf("embedded lx-go is not initialized")
	}
	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return nil, fmt.Errorf("lx-embedded keyword is empty")
	}

	log.Printf("🌐 [music/lx-embedded] search request: keyword=%q source=%s", keyword, e.source)
	songs, sourceName, err := e.registry.Search(ctx, e.source, keyword, 20)
	if err != nil {
		return nil, fmt.Errorf("lx-embedded search failed: %w", err)
	}
	if len(songs) == 0 {
		return nil, fmt.Errorf("lx-embedded no result for %q", keyword)
	}
	song := songs[0]
	log.Printf("🌐 [music/lx-embedded] search response: 音源=%s count=%d first=%s", sourceName, len(songs), briefJSON(song))

	log.Printf("🌐 [music/lx-embedded] url request: song=%s quality=%s", briefJSON(song), e.quality)
	remoteURL, urlSourceName, err := e.registry.ResolveURL(ctx, e.source, song, e.quality)
	if err != nil {
		return nil, fmt.Errorf("lx-embedded resolve url failed: %w", err)
	}
	log.Printf("🌐 [music/lx-embedded] url response: 音源=%s url=%s", urlSourceName, remoteURL)

	return &LXTrack{
		Name:    stringField(song, "name"),
		Singer:  stringField(song, "singer"),
		Source:  stringField(song, "source"),
		Quality: e.quality,
		URL:     remoteURL,
	}, nil
}

// Download 实现 lxResolver：把 track.URL 指向的直链下载保存到本地。
// 跟 HTTP 版 LXClient.Download 不同的是，这里不经过 lx-go 的
// /api/music/download 代理端点——那个端点是给"独立跑的外部服务"用的，
// 内嵌模式下直接用标准库发起下载即可，省掉一次没必要的回环网络往返。
func (e *EmbeddedLX) Download(ctx context.Context, track *LXTrack, targetPath string) error {
	if track == nil || track.URL == "" {
		return fmt.Errorf("lx-embedded download track url is empty")
	}
	if strings.TrimSpace(targetPath) == "" {
		return fmt.Errorf("lx-embedded download target path is empty")
	}

	log.Printf("🌐 [music/lx-embedded] download request: url=%s target=%s", track.URL, targetPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, track.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	client := http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("lx-embedded download failed: status=%d", resp.StatusCode)
	}

	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		return err
	}
	tmpPath := targetPath + ".tmp"
	out, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(out, resp.Body)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return closeErr
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	log.Printf("🌐 [music/lx-embedded] download response: saved=%s bytes=%d", targetPath, written)
	return nil
}
