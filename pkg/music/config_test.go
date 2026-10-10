package music

import "testing"

func TestApplyDefaultsAddsPlaybackControlKeywords(t *testing.T) {
	var cfg MusicConfig
	cfg.ApplyDefaults()

	assertContains(t, cfg.Commands.NextKeywords, "下一首")
	assertContains(t, cfg.Commands.DownloadKeywords, "下载")
	assertContains(t, cfg.Commands.PreviousKeywords, "上一首")
	assertContains(t, cfg.Commands.RepeatOneKeywords, "单曲循环")
	assertContains(t, cfg.Commands.RepeatAllKeywords, "全部循环")
	assertContains(t, cfg.Commands.ShuffleModeKeywords, "随机播放")
}

// TestApplyDefaultsAddsEpisodeNextPreviousKeywords 覆盖真实遇到的 bug：故事/有声书场景下
// 用户说"下一集"/"上一集"（而不是"下一首"/"上一首"），matchExact 是精确匹配，如果默认
// 关键词只有"首/个"这一类说法，"下一集"会被当成非音乐指令直接忽略，导致喊了没反应。
func TestApplyDefaultsAddsEpisodeNextPreviousKeywords(t *testing.T) {
	var cfg MusicConfig
	cfg.ApplyDefaults()

	assertContains(t, cfg.Commands.NextKeywords, "下一集")
	assertContains(t, cfg.Commands.NextKeywords, "下一章")
	assertContains(t, cfg.Commands.NextKeywords, "下集")
	assertContains(t, cfg.Commands.PreviousKeywords, "上一集")
	assertContains(t, cfg.Commands.PreviousKeywords, "上一章")
	assertContains(t, cfg.Commands.PreviousKeywords, "上集")
}

func TestApplyDefaultsAddsLXDefaults(t *testing.T) {
	var cfg MusicConfig
	cfg.ApplyDefaults()

	if cfg.LX.Source != "kw" {
		t.Fatalf("expected default LX source kw, got %q", cfg.LX.Source)
	}
	if cfg.LX.Quality != "128k" {
		t.Fatalf("expected default LX quality 128k, got %q", cfg.LX.Quality)
	}
	if cfg.LX.TimeoutSec != 10 {
		t.Fatalf("expected default LX timeout 10, got %d", cfg.LX.TimeoutSec)
	}
	if cfg.LX.Download {
		t.Fatal("expected LX download to be disabled by default")
	}
}

func TestApplyDefaultsAddsPlayerDiagnosticDefaults(t *testing.T) {
	var cfg MusicConfig
	cfg.ApplyDefaults()

	if cfg.Player.DiagnosticIntervalSec == nil || *cfg.Player.DiagnosticIntervalSec != 0 {
		t.Fatalf("expected diagnostic interval disabled (0) by default, got %+v", cfg.Player.DiagnosticIntervalSec)
	}
	if cfg.Player.AnnounceEpisodeBeforePlay == nil || !*cfg.Player.AnnounceEpisodeBeforePlay {
		t.Fatalf("expected announce_episode_before_play enabled by default, got %+v", cfg.Player.AnnounceEpisodeBeforePlay)
	}
}

func TestApplyDefaultsAddsPlayerWatchdogDefaults(t *testing.T) {
	var cfg MusicConfig
	cfg.ApplyDefaults()

	if cfg.Player.WatchdogEnabled == nil || *cfg.Player.WatchdogEnabled {
		t.Fatalf("expected watchdog disabled by default (heartbeat is the recommended defense), got %+v", cfg.Player.WatchdogEnabled)
	}
	if cfg.Player.PreemptMarginSec == nil || *cfg.Player.PreemptMarginSec != 2 {
		t.Fatalf("expected default preempt margin 2s, got %+v", cfg.Player.PreemptMarginSec)
	}
}

func TestApplyDefaultsAddsAbortHeartbeatDefault(t *testing.T) {
	var cfg MusicConfig
	cfg.ApplyDefaults()

	if cfg.Commands.AbortHeartbeatIntervalSec == nil || *cfg.Commands.AbortHeartbeatIntervalSec != 30 {
		t.Fatalf("expected default abort heartbeat interval 30s, got %+v", cfg.Commands.AbortHeartbeatIntervalSec)
	}
}

func assertContains(t *testing.T, values []string, want string) {
	t.Helper()
	for _, v := range values {
		if v == want {
			return
		}
	}
	t.Fatalf("expected %q in %v", want, values)
}
