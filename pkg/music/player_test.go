package music

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestPlayer(t *testing.T) (*Player, *[]string) {
	t.Helper()
	played := []string{}
	// 测试里不需要看门狗（会引入不确定的后台 goroutine 状态），显式关掉。
	p := NewPlayer(nil, nil, WithWatchdog(false))
	p.playURL = func(url string) error {
		played = append(played, url)
		p.lastPlayURLAt = time.Now().Add(-playGracePeriod)
		return nil
	}
	return p, &played
}

func testItems() []SongItem {
	return []SongItem{
		{Path: "a.mp3", URL: "http://music/a.mp3"},
		{Path: "b.mp3", URL: "http://music/b.mp3"},
		{Path: "c.mp3", URL: "http://music/c.mp3"},
	}
}

func TestPlayerNextAndPreviousUseHistory(t *testing.T) {
	p, played := newTestPlayer(t)
	p.SetQueue(testItems())

	if got := (*played)[0]; got != "http://music/a.mp3" {
		t.Fatalf("expected first song, got %s", got)
	}
	if !p.Next() {
		t.Fatal("expected next song")
	}
	if got := (*played)[1]; got != "http://music/b.mp3" {
		t.Fatalf("expected second song, got %s", got)
	}
	if !p.Previous() {
		t.Fatal("expected previous song")
	}
	if got := (*played)[2]; got != "http://music/a.mp3" {
		t.Fatalf("expected previous song to replay first song, got %s", got)
	}
}

func TestPlayerRepeatOneReplaysCurrentOnIdle(t *testing.T) {
	p, played := newTestPlayer(t)
	p.SetQueue(testItems())
	p.SetMode(PlaybackModeRepeatOne)
	p.OnPlayingStatus("Playing")
	p.OnPlayingStatus("Idle")

	if len(*played) != 2 {
		t.Fatalf("expected current song to replay, played=%v", *played)
	}
	if (*played)[1] != "http://music/a.mp3" {
		t.Fatalf("expected repeat one to replay current song, got %s", (*played)[1])
	}
}

// TestOnPlayingStatusIgnoresEarlyIdleAsManualPause 覆盖真实遇到的 bug：设备物理暂停/停止键
// 按下后，mute_stat 上报的不是 "Paused" 而是直接变成 "Idle"，从我们的角度跟"这首歌自然
// 播完了"完全没法区分。结果是用户按物理暂停键，却被我们当成"播完了"自动切到下一首——
// 物理暂停键形同虚设。用已经解析出的真实时长做兜底：只播放了一小部分就 Idle，
// 判定为手动打断，不应该自动切下一首。
func TestOnPlayingStatusIgnoresEarlyIdleAsManualPause(t *testing.T) {
	p, played := newTestPlayer(t)
	items := []SongItem{
		{Path: "a.mp3", URL: "http://music/a.mp3", DurationMs: 120_000}, // 真实时长 2 分钟
		{Path: "b.mp3", URL: "http://music/b.mp3", DurationMs: 120_000},
	}
	p.SetQueue(items)
	if len(*played) != 1 {
		t.Fatalf("expected first song to start, played=%v", *played)
	}

	// 模拟"只播了 10 秒（远小于 2 分钟真实时长的 70%）就收到 Idle"——典型的物理暂停/停止键场景。
	p.mu.Lock()
	p.lastPlayURLAt = time.Now().Add(-10 * time.Second)
	p.mu.Unlock()
	p.OnPlayingStatus("Playing")
	p.OnPlayingStatus("Idle")

	if len(*played) != 1 {
		t.Fatalf("expected no auto-advance on early idle (manual pause), played=%v", *played)
	}
	if p.CurrentState() != StateIdle {
		t.Fatalf("expected state to become idle after manual pause, got %d", p.CurrentState())
	}
}

// TestOnPlayingStatusStillAdvancesOnGenuineCompletion 确认这个兜底不会误伤正常的
// "歌曲真的播完了"场景：距上次 PlayURL 的时间已经接近/超过真实时长时，应该照常自动切歌。
func TestOnPlayingStatusStillAdvancesOnGenuineCompletion(t *testing.T) {
	p, played := newTestPlayer(t)
	items := []SongItem{
		{Path: "a.mp3", URL: "http://music/a.mp3", DurationMs: 120_000}, // 真实时长 2 分钟
		{Path: "b.mp3", URL: "http://music/b.mp3", DurationMs: 120_000},
	}
	p.SetQueue(items)

	// 模拟"播了 1 分 55 秒（已经超过 2 分钟的 70%）才收到 Idle"——正常播完的场景。
	p.mu.Lock()
	p.lastPlayURLAt = time.Now().Add(-115 * time.Second)
	p.mu.Unlock()
	p.OnPlayingStatus("Playing")
	p.OnPlayingStatus("Idle")

	if len(*played) != 2 {
		t.Fatalf("expected genuine completion to advance to next song, played=%v", *played)
	}
}

// TestOnPlayingStatusEarlyIdleStillAdvancesForEpisodeContent 覆盖线上回归：
// 故事/有声书（Episode>0）在设备偶发早期 Idle 时，不应该被"手动暂停兜底"拦住，
// 否则会卡在当前集，表现成"播了几集后不自动续播"。
func TestOnPlayingStatusEarlyIdleStillAdvancesForEpisodeContent(t *testing.T) {
	p, played := newTestPlayer(t)
	p.speak = func(text string) error { return nil }
	items := []SongItem{
		{Path: "ep93.mp3", URL: "http://music/ep93.mp3", DurationMs: 280_000, Episode: 93},
		{Path: "ep94.mp3", URL: "http://music/ep94.mp3", DurationMs: 280_000, Episode: 94},
	}
	p.SetQueue(items)
	if len(*played) != 1 {
		t.Fatalf("expected first episode to start, played=%v", *played)
	}

	// 模拟设备在 21 秒就上报 Idle（远小于 280s 的 70%）。对于 Episode>0，
	// 仍应按自然播完处理并切到下一集，而不是误判成手动暂停。
	p.mu.Lock()
	p.lastPlayURLAt = time.Now().Add(-21 * time.Second)
	p.mu.Unlock()
	p.OnPlayingStatus("Playing")
	p.OnPlayingStatus("Idle")

	if len(*played) != 2 {
		t.Fatalf("expected episode content to continue on early idle, played=%v", *played)
	}
	if (*played)[1] != "http://music/ep94.mp3" {
		t.Fatalf("expected to advance to next episode, got %s", (*played)[1])
	}
}

func TestPlayerRepeatOneManualNextStillAdvances(t *testing.T) {
	// 单曲循环只影响自动 Idle，用户手动 "下一首" 仍然要跳出当前曲，
	// 跟 iTunes / Spotify / Apple Music 一致。
	p, played := newTestPlayer(t)
	p.SetQueue(testItems())
	p.SetMode(PlaybackModeRepeatOne)

	if !p.Next() {
		t.Fatal("expected Next to advance under RepeatOne")
	}
	if got := (*played)[1]; got != "http://music/b.mp3" {
		t.Fatalf("expected manual Next to advance to b.mp3, got %s", got)
	}
	if !p.Previous() {
		t.Fatal("expected Previous to roll back under RepeatOne")
	}
	if got := (*played)[2]; got != "http://music/a.mp3" {
		t.Fatalf("expected manual Previous to roll back to a.mp3, got %s", got)
	}
}

func TestPlayerRepeatAllLoopsPlaylistOnIdle(t *testing.T) {
	p, played := newTestPlayer(t)
	p.SetQueue(testItems()[:2])
	p.SetMode(PlaybackModeRepeatAll)

	p.OnPlayingStatus("Playing")
	p.OnPlayingStatus("Idle")
	p.OnPlayingStatus("Playing")
	p.OnPlayingStatus("Idle")

	want := []string{"http://music/a.mp3", "http://music/b.mp3", "http://music/a.mp3"}
	if len(*played) != len(want) {
		t.Fatalf("expected %v, got %v", want, *played)
	}
	for i := range want {
		if (*played)[i] != want[i] {
			t.Fatalf("expected %v, got %v", want, *played)
		}
	}
}

// TestStateRemainsPlayingAfterAutoAdvance 覆盖一个关键状态一致性：
// OnPlayingStatus("Idle") 触发自动切歌且成功后，state 必须保持 Playing。
// 否则后续依赖 state==Playing 的心跳/看门狗会误判为空闲，导致停摆。
func TestStateRemainsPlayingAfterAutoAdvance(t *testing.T) {
	p, _ := newTestPlayer(t)
	p.SetQueue(testItems()[:2])
	p.OnPlayingStatus("Playing")
	p.OnPlayingStatus("Idle")

	if got := p.CurrentState(); got != StatePlaying {
		t.Fatalf("expected state playing after successful auto-advance, got %d", got)
	}
}

// TestStateBecomesIdleWhenQueueExhausted 自动切歌失败（队列耗尽）时才应该回到 Idle。
func TestStateBecomesIdleWhenQueueExhausted(t *testing.T) {
	p, _ := newTestPlayer(t)
	p.SetQueue(testItems()[:1])
	p.OnPlayingStatus("Playing")
	p.OnPlayingStatus("Idle")

	if got := p.CurrentState(); got != StateIdle {
		t.Fatalf("expected state idle when queue exhausted, got %d", got)
	}
}

func TestPlayerShuffleModeContinuesAfterQueueEnds(t *testing.T) {
	p, played := newTestPlayer(t)
	p.SetQueue(testItems()[:1])
	p.SetMode(PlaybackModeShuffle)
	p.OnPlayingStatus("Playing")
	p.OnPlayingStatus("Idle")

	if len(*played) != 2 {
		t.Fatalf("expected shuffle mode to pick another song, played=%v", *played)
	}
}

// TestAnnounceEpisodeBeforePlayback 覆盖"播放前先播报第几集"的核心需求：
//   - 第一集（由 SetQueue 播放）不重复播报（module.go 的 handlePlay 已经用反馈语报过了）；
//   - 自动切到下一集（Idle 触发）、用户手动 Next/Previous 都应该在真正 PlayURL 之前先
//     Speak "现在播放第 X 集"，且顺序必须是先说完集数、再播放音频。
func TestAnnounceEpisodeBeforePlayback(t *testing.T) {
	p, played := newTestPlayer(t)
	var spoken []string
	var order []string
	p.speak = func(text string) error {
		spoken = append(spoken, text)
		order = append(order, "speak:"+text)
		return nil
	}
	// 覆盖 playURL 记录顺序，验证"先说集数、再播放"的时序。
	p.playURL = func(url string) error {
		*played = append(*played, url)
		order = append(order, "play:"+url)
		p.lastPlayURLAt = time.Now().Add(-playGracePeriod)
		return nil
	}

	items := []SongItem{
		{Path: "ep1.mp3", URL: "http://music/ep1.mp3", Episode: 1},
		{Path: "ep2.mp3", URL: "http://music/ep2.mp3", Episode: 2},
	}
	p.SetQueue(items)
	if len(spoken) != 0 {
		t.Fatalf("expected no episode announcement for the first item played via SetQueue, got %v", spoken)
	}

	p.OnPlayingStatus("Playing")
	p.OnPlayingStatus("Idle") // 自动切到第 2 集

	if len(spoken) != 1 || spoken[0] != "现在播放第2集" {
		t.Fatalf("expected announcement for episode 2 on auto-advance, got %v", spoken)
	}
	wantOrder := []string{"play:http://music/ep1.mp3", "speak:现在播放第2集", "play:http://music/ep2.mp3"}
	if len(order) != len(wantOrder) {
		t.Fatalf("expected order %v, got %v", wantOrder, order)
	}
	for i := range wantOrder {
		if order[i] != wantOrder[i] {
			t.Fatalf("expected speak-before-play order %v, got %v", wantOrder, order)
		}
	}

	if !p.Previous() {
		t.Fatal("expected previous to succeed")
	}
	if len(spoken) != 2 || spoken[1] != "现在播放第1集" {
		t.Fatalf("expected announcement for episode 1 on manual Previous, got %v", spoken)
	}
}

// TestAnnounceEpisodeSkippedForNonEpisodeContent 普通音乐/在线歌曲 Episode=0，
// 不应该触发"现在播放第 X 集"播报。
func TestAnnounceEpisodeSkippedForNonEpisodeContent(t *testing.T) {
	p, _ := newTestPlayer(t)
	var spoken []string
	p.speak = func(text string) error {
		spoken = append(spoken, text)
		return nil
	}
	p.SetQueue(testItems()) // Episode 均为 0
	p.OnPlayingStatus("Playing")
	p.OnPlayingStatus("Idle")
	if len(spoken) != 0 {
		t.Fatalf("expected no episode announcement for non-episode content, got %v", spoken)
	}
}

func TestEpisodeAnnouncementCanBeDisabled(t *testing.T) {
	p, played := newTestPlayer(t)
	p.announceEpisodeBeforePlay = false
	var spoken []string
	p.speak = func(text string) error {
		spoken = append(spoken, text)
		return nil
	}
	items := []SongItem{
		{Path: "ep10.mp3", URL: "http://music/ep10.mp3", Episode: 10},
		{Path: "ep11.mp3", URL: "http://music/ep11.mp3", Episode: 11},
	}
	p.SetQueue(items)
	p.OnPlayingStatus("Playing")
	p.OnPlayingStatus("Idle")

	if len(*played) != 2 {
		t.Fatalf("expected queue to auto-advance, played=%v", *played)
	}
	if len(spoken) != 0 {
		t.Fatalf("expected no episode announcement when disabled, got %v", spoken)
	}
}

func TestPlayItemWaitsForSpeakBeforePlayURL(t *testing.T) {
	p, played := newTestPlayer(t)
	done := make(chan struct{})
	p.speak = func(text string) error {
		time.Sleep(40 * time.Millisecond)
		close(done)
		return nil
	}
	items := []SongItem{
		{Path: "ep1.mp3", URL: "http://music/ep1.mp3", Episode: 1},
		{Path: "ep2.mp3", URL: "http://music/ep2.mp3", Episode: 2},
	}
	p.SetQueue(items)
	p.OnPlayingStatus("Playing")
	p.OnPlayingStatus("Idle")

	select {
	case <-done:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("expected Speak to complete before advancing playback")
	}
	if len(*played) != 2 || (*played)[1] != "http://music/ep2.mp3" {
		t.Fatalf("expected playback to advance to episode 2, played=%v", *played)
	}
}

func TestPlayItemStillPlaysWhenSpeakFails(t *testing.T) {
	p, played := newTestPlayer(t)
	p.speak = func(text string) error {
		return errors.New("tts down")
	}
	items := []SongItem{
		{Path: "ep1.mp3", URL: "http://music/ep1.mp3", Episode: 1},
		{Path: "ep2.mp3", URL: "http://music/ep2.mp3", Episode: 2},
	}
	p.SetQueue(items)
	p.OnPlayingStatus("Playing")
	p.OnPlayingStatus("Idle")

	if len(*played) != 2 {
		t.Fatalf("expected playback to continue even when Speak fails, played=%v", *played)
	}
}

func TestTreatSpeakExitAsSuccessWhenMyLogMissingButPlaybackCompleted(t *testing.T) {
	stderr := "/usr/sbin/tts_play.sh: line 1: my_log: not found\n...\nevent(EndReached) is posted\n"
	if !treatSpeakExitAsSuccess("", stderr, 2*time.Second) {
		t.Fatal("expected non-zero speak to be treated as success when playback completed")
	}
}

// TestTreatSpeakExitAsSuccessWhenStdoutHasTTSPath 覆盖"my_log 桩脚本已部署"后的场景：
// stderr 不再有 my_log: not found，但 stdout 有 /tmp/tts/tts_ 路径且耗时足够，
// 说明 TTS 已播完，不应该重试（否则用户会听到两遍播报）。
func TestTreatSpeakExitAsSuccessWhenStdoutHasTTSPath(t *testing.T) {
	stdout := `{"code": 0}` + "\n" + `{ "path": "/tmp/tts/tts_abc123.mp3" }` + "\n/tmp/tts/tts_abc123.mp3"
	stderr := "libmiplayerlite: player_init: ...\nevent(EndReached) is posted\n"
	if !treatSpeakExitAsSuccess(stdout, stderr, 2*time.Second) {
		t.Fatal("expected success when stdout has TTS path and elapsed >= 1s")
	}
}

func TestTreatSpeakExitAsSuccessReturnsFalseWhenTooFast(t *testing.T) {
	// 不到 1 秒就返回 — TTS 根本没播，不能判成功
	stdout := `{ "path": "/tmp/tts/tts_abc123.mp3" }`
	if treatSpeakExitAsSuccess(stdout, "", 100*time.Millisecond) {
		t.Fatal("expected failure when elapsed < 1s even if stdout has TTS path")
	}
}

func TestTreatSpeakExitAsSuccessReturnsFalseForHardFailure(t *testing.T) {
	stderr := "miplayer: option requires an argument -- 'f'"
	if treatSpeakExitAsSuccess("", stderr, 100*time.Millisecond) {
		t.Fatal("expected hard failure not to be treated as success")
	}
}

// TestExhaustedHandlerFetchesMoreWhenQueueEmpties 覆盖故事/按集播放的自动续播场景：
// 队列播完（顺序模式，没有循环）时，如果设置了 onExhausted 回调，应该调用它拉取下一批
// 并接着播放，而不是直接停止。
func TestExhaustedHandlerFetchesMoreWhenQueueEmpties(t *testing.T) {
	p, played := newTestPlayer(t)
	var calls int
	p.SetExhaustedHandler(func() []SongItem {
		calls++
		if calls == 1 {
			return []SongItem{{Path: "d.mp3", URL: "http://music/d.mp3"}}
		}
		return nil // 第二次续播时说"没有更多了"，避免测试无限循环
	})
	p.SetQueue(testItems()[:1]) // 只有 1 首，播完就该触发续播
	p.OnPlayingStatus("Playing")
	p.OnPlayingStatus("Idle")

	if len(*played) != 2 {
		t.Fatalf("expected exhausted handler to fetch and play one more song, played=%v", *played)
	}
	if (*played)[1] != "http://music/d.mp3" {
		t.Fatalf("expected continuation song d.mp3, got %s", (*played)[1])
	}
	if calls != 1 {
		t.Fatalf("expected onExhausted called exactly once, got %d", calls)
	}

	// 续播的这首播完之后，onExhausted 返回 nil，应该正常停止，不再调用第三次。
	p.OnPlayingStatus("Playing")
	p.OnPlayingStatus("Idle")
	if len(*played) != 2 {
		t.Fatalf("expected no further advance once onExhausted returns nil, played=%v", *played)
	}
}

// TestExhaustedHandlerNotCalledWhenNil 没设置续播回调（nil）时，队列耗尽应该正常停止，
// 不panic、不影响现有行为。
func TestExhaustedHandlerNotCalledWhenNil(t *testing.T) {
	p, played := newTestPlayer(t)
	p.SetQueue(testItems()[:1])
	p.OnPlayingStatus("Playing")
	p.OnPlayingStatus("Idle")

	if len(*played) != 1 {
		t.Fatalf("expected no advance without exhausted handler, played=%v", *played)
	}
}

// TestWatchdogPreemptivelyAdvancesNearEstimatedEnd 覆盖真实遇到的 bug：设备端 mediaplayer
// 自带一套跟 mico_aivs_lab 无关的"第三方内容源续播"机制，只要真正进入 idle 且没有排队的
// 下一个 URL，就会恢复你之前用原生小爱听到一半的别的内容（实测抓到
// `player_get_context` 返回 `audio_meta.cp.name = "ximalaya"`）。纯被动等 Idle 事件、
// 事后再反应，天生要跟设备本地这套机制赛跑，经常会输。
// 现在的策略是"主动"：在预估时长结束前 preemptMargin 就抢先切到下一首，而不是等真正播完。
func TestWatchdogPreemptivelyAdvancesNearEstimatedEnd(t *testing.T) {
	p, played := newTestPlayer(t)
	p.watchdogEnabled = true
	p.preemptMargin = 2 * time.Second
	items := []SongItem{
		// 真实时长 10s，提前量 2s，预计在 lastPlayURLAt+8s 左右就该主动切歌。
		{Path: "a.mp3", URL: "http://music/a.mp3", DurationMs: 10_000},
		{Path: "b.mp3", URL: "http://music/b.mp3", DurationMs: 10_000},
	}
	p.SetQueue(items)
	if len(*played) != 1 {
		t.Fatalf("expected first song to start, played=%v", *played)
	}

	// 模拟"已经接近预估时长结束"：把 lastPlayURLAt 往回拨到 duration-margin 之后，
	// 从来没有调用过 OnPlayingStatus("Idle")（模拟设备可能被 CP 续播抢先接管的场景）。
	p.mu.Lock()
	p.lastPlayURLAt = time.Now().Add(-9 * time.Second)
	p.mu.Unlock()

	p.checkWatchdog()

	if len(*played) != 2 {
		t.Fatalf("expected watchdog to preemptively advance to next song, played=%v", *played)
	}
	if (*played)[1] != "http://music/b.mp3" {
		t.Fatalf("expected watchdog to advance to b.mp3, got %s", (*played)[1])
	}
}

// TestWatchdogDoesNotFireBeforePreemptWindow 确认看门狗不会在提前量窗口之前就误触发：
// 刚播放不久（远早于"预估时长-提前量"）时不应该切歌。
func TestWatchdogDoesNotFireBeforePreemptWindow(t *testing.T) {
	p, played := newTestPlayer(t)
	p.watchdogEnabled = true
	p.preemptMargin = 2 * time.Second
	items := []SongItem{
		// 真实时长 10 分钟，刚播放几秒离"提前量窗口"还远得很，不该触发。
		{Path: "a.mp3", URL: "http://music/a.mp3", DurationMs: 10 * 60 * 1000},
		{Path: "b.mp3", URL: "http://music/b.mp3", DurationMs: 10 * 60 * 1000},
	}
	p.SetQueue(items)

	p.checkWatchdog()

	if len(*played) != 1 {
		t.Fatalf("expected watchdog not to fire yet, played=%v", *played)
	}
}

// TestWatchdogPreemptMarginCappedByRatio 提前量不应该超过真实时长的 20%——
// 即便配置了一个很大的 preemptMargin，对一首很短的歌也不能砍掉太大一截内容。
func TestWatchdogPreemptMarginCappedByRatio(t *testing.T) {
	p, played := newTestPlayer(t)
	p.watchdogEnabled = true
	p.preemptMargin = 30 * time.Second // 故意配置一个很大的提前量
	items := []SongItem{
		// 真实时长 10s，20% 上限 = 2s，实际生效的提前量应该是 2s 而不是 30s。
		{Path: "a.mp3", URL: "http://music/a.mp3", DurationMs: 10_000},
		{Path: "b.mp3", URL: "http://music/b.mp3", DurationMs: 10_000},
	}
	p.SetQueue(items)

	// 只过去了 5 秒（还没到 10s-2s=8s 这个被 20% 上限限制后的触发点），不应该触发。
	p.mu.Lock()
	p.lastPlayURLAt = time.Now().Add(-5 * time.Second)
	p.mu.Unlock()
	p.checkWatchdog()
	if len(*played) != 1 {
		t.Fatalf("expected watchdog not to fire before capped margin window, played=%v", *played)
	}

	// 过去了 9 秒（超过 8s 这个触发点），应该已经触发。
	p.mu.Lock()
	p.lastPlayURLAt = time.Now().Add(-9 * time.Second)
	p.mu.Unlock()
	p.checkWatchdog()
	if len(*played) != 2 {
		t.Fatalf("expected watchdog to fire once past the capped margin window, played=%v", *played)
	}
}

// TestWatchdogSkipsUnknownDuration 时长未知（DurationMs=0，比如 LX 在线直链/非 mp3/解析失败）
// 时看门狗不应生效，避免瞎猜误切；这种情况下完全依赖 Idle 事件本身。
func TestWatchdogSkipsUnknownDuration(t *testing.T) {
	p, played := newTestPlayer(t)
	p.watchdogEnabled = true
	items := []SongItem{
		{Path: "online", URL: "http://music/online.mp3", DurationMs: 0},
		{Path: "b.mp3", URL: "http://music/b.mp3", DurationMs: 0},
	}
	p.SetQueue(items)

	p.mu.Lock()
	p.lastPlayURLAt = time.Now().Add(-1 * time.Hour)
	p.mu.Unlock()

	p.checkWatchdog()

	if len(*played) != 1 {
		t.Fatalf("expected watchdog to skip unknown-duration song, played=%v", *played)
	}
}

// TestAbortXiaoAIHeartbeatFiresWhileActivelyPlaying 覆盖真实遇到的 bug：小爱云端在
// mico_aivs_lab 重连一段时间后会主动把"之前暂停的故事"resume 到 mediaplayer，抢占本地队列，
// 且这个过程完全不经过 Idle 事件。心跳应该在本地队列仍在播放时周期性重复 AbortXiaoAI，
// 抢在云端触发 resume 之前把它重新打断。
func TestAbortXiaoAIHeartbeatFiresWhileActivelyPlaying(t *testing.T) {
	p, _ := newTestPlayer(t)
	var calls int32
	p.abortXiaoAI = func() error {
		atomic.AddInt32(&calls, 1)
		return nil
	}
	p.SetQueue(testItems())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		p.AbortXiaoAIHeartbeatLoop(ctx, 10*time.Millisecond)
		close(done)
	}()

	// 给心跳几个 tick 的时间触发
	time.Sleep(60 * time.Millisecond)
	cancel()
	<-done

	if atomic.LoadInt32(&calls) == 0 {
		t.Fatal("expected heartbeat to call AbortXiaoAI at least once while actively playing")
	}
}

// TestAbortXiaoAIHeartbeatSkipsWhenNotPlaying 队列播完/空闲时心跳不应该继续重启 mico_aivs_lab
// （没有必要，也避免无意义地打断用户跟原生小爱的正常交互）。
func TestAbortXiaoAIHeartbeatSkipsWhenNotPlaying(t *testing.T) {
	p, _ := newTestPlayer(t)
	var calls int32
	p.abortXiaoAI = func() error {
		atomic.AddInt32(&calls, 1)
		return nil
	}
	// 不设置队列，currentSong 保持 nil。

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		p.AbortXiaoAIHeartbeatLoop(ctx, 10*time.Millisecond)
		close(done)
	}()

	time.Sleep(60 * time.Millisecond)
	cancel()
	<-done

	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("expected heartbeat to skip when idle, got %d calls", calls)
	}
}

// TestDiagnosticContextLoopFiresWhileActivelyPlaying 覆盖诊断快照循环：播放期间应该周期性
// 调用 runShellCommand 拉取 player_get_context，用于排查"自动切到别的内容"问题的根因。
func TestDiagnosticContextLoopFiresWhileActivelyPlaying(t *testing.T) {
	p, _ := newTestPlayer(t)
	var calls int32
	var lastScript string
	p.runShell = func(script string) (string, error) {
		atomic.AddInt32(&calls, 1)
		lastScript = script
		return `{"volume":50}`, nil
	}
	p.SetQueue(testItems())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		p.DiagnosticContextLoop(ctx, 10*time.Millisecond)
		close(done)
	}()

	time.Sleep(60 * time.Millisecond)
	cancel()
	<-done

	if atomic.LoadInt32(&calls) == 0 {
		t.Fatal("expected diagnostic loop to call runShell at least once while actively playing")
	}
	if !strings.Contains(lastScript, "player_get_context") {
		t.Fatalf("expected script to query player_get_context, got %q", lastScript)
	}
}

// TestDiagnosticContextLoopSkipsWhenNotPlaying 队列空闲时不应该继续拉取诊断信息。
func TestDiagnosticContextLoopSkipsWhenNotPlaying(t *testing.T) {
	p, _ := newTestPlayer(t)
	var calls int32
	p.runShell = func(script string) (string, error) {
		atomic.AddInt32(&calls, 1)
		return "", nil
	}
	// 不设置队列，currentSong 保持 nil。

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		p.DiagnosticContextLoop(ctx, 10*time.Millisecond)
		close(done)
	}()

	time.Sleep(60 * time.Millisecond)
	cancel()
	<-done

	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("expected diagnostic loop to skip when idle, got %d calls", calls)
	}
}

// TestDiagnosticContextLoopDisabledWithZeroInterval interval<=0 应该直接返回，不启动任何 ticker。
func TestDiagnosticContextLoopDisabledWithZeroInterval(t *testing.T) {
	p, _ := newTestPlayer(t)
	var calls int32
	p.runShell = func(script string) (string, error) {
		atomic.AddInt32(&calls, 1)
		return "", nil
	}
	p.SetQueue(testItems())

	done := make(chan struct{})
	go func() {
		p.DiagnosticContextLoop(context.Background(), 0)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected DiagnosticContextLoop to return immediately when interval<=0")
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("expected no calls when disabled, got %d", calls)
	}
}
