package music

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// PlayHistoryEntry 记录用户最近一次听故事听到的位置。
// 只精确到"集"（不是秒级断点续听——故事是整集 mp3 播放，没有播放位置概念），
// 但对"继续播放故事"这个场景已经够用：找到系列名+集数，接着往后放。
type PlayHistoryEntry struct {
	SeriesName string    `json:"series_name"`
	Episode    int       `json:"episode"`
	Path       string    `json:"path,omitempty"` // 当时命中的文件路径，仅供诊断参考
	UpdatedAt  time.Time `json:"updated_at"`
}

// PlayHistoryStore 播放历史的内存态 + 磁盘持久化。
// 目前只保存"最近一次"，不是完整历史列表——"继续播放故事"只需要知道
// 最后听的是哪个系列、第几集，不需要回放整个收听历史。
type PlayHistoryStore struct {
	mu    sync.RWMutex
	entry PlayHistoryEntry
	path  string
}

// NewPlayHistoryStore 创建播放历史存储。path 为空时 Load/Save 均为 no-op（相当于关闭该功能）。
func NewPlayHistoryStore(path string) *PlayHistoryStore {
	return &PlayHistoryStore{path: path}
}

// Load 从磁盘加载上次记录。文件不存在视为"还没有历史"，不是错误。
func (s *PlayHistoryStore) Load() error {
	if s.path == "" {
		return nil
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("📂 [music/history] 播放历史文件不存在: %s (还没有记录)", s.path)
			return nil
		}
		return fmt.Errorf("read play history: %w", err)
	}
	var entry PlayHistoryEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return fmt.Errorf("parse play history: %w", err)
	}
	s.mu.Lock()
	s.entry = entry
	s.mu.Unlock()
	log.Printf("📂 [music/history] 已加载播放历史: series=%q episode=%d (updated_at=%s)",
		entry.SeriesName, entry.Episode, entry.UpdatedAt.Format(time.RFC3339))
	return nil
}

// Save 原子写入磁盘：先写同目录 tmp 文件再 rename，避免进程被 kill 时留下半截 JSON
// 让下次 Load 失败（同 indexer.go 的 Save 手法）。
func (s *PlayHistoryStore) Save() error {
	if s.path == "" {
		return nil
	}
	s.mu.RLock()
	entry := s.entry
	s.mu.RUnlock()

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal play history: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("write play history tmp: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename play history: %w", err)
	}
	return nil
}

// Get 返回当前记录的副本。SeriesName == "" 表示还没有任何记录。
func (s *PlayHistoryStore) Get() PlayHistoryEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.entry
}

// Update 更新"最近播放"记录并落盘。seriesName 为空或 episode<=0 时忽略——
// 普通音乐/在线歌曲没有集数，不应该覆盖掉之前有效的故事进度记录。
//
// 落盘失败只打日志、不阻断播放流程：这是"体验加分项"，不该影响播放本身的可靠性。
func (s *PlayHistoryStore) Update(seriesName string, episode int, path string) {
	seriesName = strings.TrimSpace(seriesName)
	if seriesName == "" || episode <= 0 {
		return
	}
	s.mu.Lock()
	unchanged := s.entry.SeriesName == seriesName && s.entry.Episode == episode
	s.entry = PlayHistoryEntry{
		SeriesName: seriesName,
		Episode:    episode,
		Path:       path,
		UpdatedAt:  time.Now(),
	}
	s.mu.Unlock()
	if unchanged {
		// 同一集重复触发（比如单曲循环重播），避免无意义的重复磁盘写入。
		return
	}
	if err := s.Save(); err != nil {
		log.Printf("⚠️ [music/history] 保存播放历史失败: %v", err)
		return
	}
	log.Printf("💾 [music/history] 已记录播放进度: series=%q episode=%d", seriesName, episode)
}
