package music

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/cxjava/open-xiaoai/apps/client/services/connect"
)

// shellResult 镜像 client-go utils.CommandResult，用于解码 run_shell 的返回值
type shellResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// decodeShellResult 解析 run_shell 响应里的 CommandResult，
// 用于排查设备端 ubus/mphelper 是否返回错误。
func decodeShellResult(resp connect.Response) *shellResult {
	if resp.Data == nil {
		return nil
	}
	var r shellResult
	if err := json.Unmarshal(*resp.Data, &r); err != nil {
		return nil
	}
	return &r
}

// briefShellResult 把 stdout/stderr 截断便于打日志（避免一次倒几千字节）
func briefShellResult(r *shellResult) string {
	if r == nil {
		return "<nil>"
	}
	trim := func(s string) string {
		s = strings.TrimSpace(s)
		if len(s) > 200 {
			return s[:200] + "...(trunc)"
		}
		return s
	}
	return fmt.Sprintf("exit=%d stdout=%q stderr=%q", r.ExitCode, trim(r.Stdout), trim(r.Stderr))
}

// PlaybackState 播放状态
type PlaybackState int

const (
	StateIdle PlaybackState = iota
	StatePlaying
)

// PlaybackMode controls how the queue advances when a song ends.
type PlaybackMode int

const (
	PlaybackModeSequence PlaybackMode = iota
	PlaybackModeRepeatOne
	PlaybackModeRepeatAll
	PlaybackModeShuffle
)

// 状态过滤参数（防误切歌）
//
// 背景：PlayingMonitor 用 `mphelper mute_stat` 200ms 轮询设备状态，
// 但 mediaplayer 状态会被 tts_play.sh / mphelper pause / 切歌加载抖动等多种因素折腾，
// 不能简单把每次 Playing→Idle 都当成"歌曲播完"。否则会出现：
//  1. handlePlay 里 Speak 调 tts_play.sh，tts_play.sh 内部先 `mphelper pause` 再播 TTS，
//     mediaplayer 短暂 Idle/Paused → 上报 Idle → 触发"切歌" → 队列空就误停止；
//  2. player_play_url 切到新 URL，mediaplayer 在加载期间可能短暂 Idle → 同样误触发；
//  3. 网络/解码偶发的瞬时 Idle 也会被误判为"歌曲播完"。
const (
	// 距离上次主动 PlayURL 多久内的 Idle 事件，都视为"切歌抖动/加载延迟"忽略。
	// 5s 足够 cover：mediaplayer 切 URL 后下载缓冲 + 解码启动的时间（FLAC 25MB 一般 1-3s）；
	// 正常一首歌都远超 5s，所以不会影响真实的"播完→切下一首"判断。
	// 注：Speak/TTS 阶段由 suppressUntil 单独覆盖，不依赖这个 grace。
	playGracePeriod = 5 * time.Second

	// Speak 调用前置抑制窗口：Speak 本身 timeout 15s，整段过程内 mediaplayer 状态都不可信。
	// 给 18s 留一点 buffer。
	speakSuppressPrefix = 18 * time.Second

	// Speak 返回后再延一段时间继续抑制：mphelper play 把 mediaplayer 恢复 Playing 需要时间，
	// 这期间收到的 Idle 仍可能是恢复过程中的瞬态。
	speakSuppressTail = 3 * time.Second

	// 有些设备固件上的 tts_play.sh 会很快返回（甚至<100ms），但声音实际上还没真正播出来，
	// 紧接着下发 PlayURL 会把这句提示直接抢断，用户就听不到“现在播放第X集”。
	// 这里给一个最小阻塞时长兜底，保证“先播报，再播放正文”在时序上成立。
	minSpeakBlocking = 1500 * time.Millisecond

	// AbortXiaoAI 会重启 mico_aivs_lab，重启后的短窗口里 tts_play.sh 偶发失败
	// （如 Failed to parse message data / miplayer -f 缺参）。失败后稍等再重试一次，
	// 覆盖"服务刚起来还没就绪"的情况。
	speakRetryDelay = 1200 * time.Millisecond

	// earlyIdleThresholdRatio 用于区分"歌曲自然播完"和"用户手动暂停/停止"（比如按了设备
	// 物理暂停键）：如果距上次 PlayURL 的时间占这首歌真实时长的比例低于这个值，判定为
	// 手动打断，不自动切下一首。0.7 留了比较大的容错空间（应对 CBR 估算误差、VBR 文件等），
	// 同时足以覆盖"物理暂停键按下时track 通常远没播完"这种典型场景。
	// 只对能解析出真实时长的 .mp3 生效（IndexedSong.DurationMs>0）。
	earlyIdleThresholdRatio = 0.7
)

// SongItem 队列中的歌曲项
type SongItem struct {
	Path string
	URL  string
	// Size 文件字节数，来自 IndexedSong.Size，目前仅用于日志/诊断。
	Size int64
	// DurationMs 播放时长（毫秒），来自 IndexedSong.DurationMs（解析 mp3 帧头真实比特率算出的）。
	// 用于主动续播机制判断"这首歌是不是快播完了"；0 表示未知（比如 LX 在线直链、非 mp3、
	// 解析失败），此时主动续播对这首歌不生效，完全依赖真正的 Idle 事件。
	DurationMs int64
	// Episode 集数，来自 IndexedSong.Episode，0 表示非分集内容（普通音乐/在线歌曲）。
	// 用于播放前播报"现在播放第 X 集"，方便小朋友知道当前进度、中断后也能知道听到第几集了。
	Episode int
	// SeriesName 系列名（如「西游记」），来自 handlePlay 解析出的 PlayIntent.SeriesName。
	// 仅故事/有声书队列会填充；普通音乐/在线歌曲为空。用于 onEpisodeChange 回调记录
	// "最近播放到哪个系列第几集"，支撑"继续播放故事"语音指令，不参与播放本身的逻辑。
	SeriesName string
}

// Player 播放器：队列、RPC 调用、Idle 切歌
type Player struct {
	mu          sync.Mutex
	queue       []SongItem
	playlist    []SongItem
	history     []SongItem
	currentSong *SongItem
	state       PlaybackState
	mode        PlaybackMode
	fileServer  *FileServer
	indexer     *Indexer
	playURL     func(url string) error
	speak       func(text string) error
	abortXiaoAI func() error
	// runShell 可覆盖的 shell 执行入口（测试用），默认 nil 时走真实 RPC。
	runShell func(script string) (stdout string, err error)

	// lastPlayURLAt 上次主动 PlayURL 的时间戳，用于 grace period 内过滤 Idle 误报，
	// 也用于主动续播机制判断"这首歌是不是快播完了"。
	lastPlayURLAt time.Time
	// suppressUntil 显式抑制 OnPlayingStatus 处理的截止时间（Speak 期间会撑开此窗口）
	suppressUntil time.Time

	// watchdogEnabled：主动续播机制开关；preemptMargin：提前多久切下一首。见 PlayerConfig 注释。
	watchdogEnabled bool
	preemptMargin   time.Duration

	// onExhausted 队列和 playlist 都耗尽（PlaybackModeSequence 顺序播放模式）时的续播回调，
	// 主要给"故事/有声书按集播放"用：与其把 search.max_results 开得很大一次性入队整季
	// （那样会连带影响普通音乐搜索/随机播放的结果条数），不如在当前这一批放完时按需再取下一批。
	//
	// 返回非空 []SongItem 表示"还有更多，接着放"；返回 nil/空表示"真的没有了，正常停止"。
	//
	// 约束：这个回调在 nextLocked() 持有 p.mu 时被调用，不能反过来调用 Player 上任何需要
	// 获取 p.mu 的方法（会死锁）——只应该是纯查询（比如调 Indexer.SearchEpisode），不碰 Player 状态。
	onExhausted func() []SongItem

	// announceEpisodeBeforePlay 控制是否在播放分集内容（Episode>0）前播报
	// "现在播放第X集"。
	announceEpisodeBeforePlay bool

	// onEpisodeChange 每次切到一个新的分集内容（item.Episode>0）时触发，用于记录
	// "最近播放到哪个系列第几集"（供"继续播放故事"读取）。故意设计成"只通知，
	// 不参与播放决策"：回调里不能反过来调用 Player 上任何需要 p.mu 的方法。
	// nil 表示未启用（比如没有配置 history.file）。
	onEpisodeChange func(item SongItem)

	// initialStateCh 在收到第一个 playing 事件时关闭，用来给上层"等一下首个状态"的能力，
	// 取代之前注释里建议的 Sleep 推断（不可靠）。
	initialStateCh   chan struct{}
	initialStateOnce sync.Once
}

// NewPlayer 创建播放器
func NewPlayer(fs *FileServer, idx *Indexer, opts ...PlayerOption) *Player {
	p := &Player{
		fileServer:                fs,
		indexer:                   idx,
		initialStateCh:            make(chan struct{}),
		watchdogEnabled:           true,
		preemptMargin:             defaultPreemptMargin,
		announceEpisodeBeforePlay: true,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// PlayerOption NewPlayer 的可选配置项
type PlayerOption func(*Player)

// WithWatchdog 配置主动续播机制的开关（见 PlayerConfig 注释）。
func WithWatchdog(enabled bool) PlayerOption {
	return func(p *Player) {
		p.watchdogEnabled = enabled
	}
}

// WithPreemptMargin 配置提前多久（在预估时长结束前）主动切到下一首，见 PlayerConfig 注释。
// seconds<=0 时使用默认值 defaultPreemptMargin，不会被设成 0（那样就失去了"抢在设备原生
// 续播机制之前拿到控制权"的意义）。
func WithPreemptMargin(seconds int) PlayerOption {
	return func(p *Player) {
		if seconds > 0 {
			p.preemptMargin = time.Duration(seconds) * time.Second
		}
	}
}

// WithEpisodeAnnouncement 配置是否在每次播放分集内容前播报“现在播放第X集”。
func WithEpisodeAnnouncement(enabled bool) PlayerOption {
	return func(p *Player) {
		p.announceEpisodeBeforePlay = enabled
	}
}

// WithOnEpisodeChange 注册"切到新一集"的回调，用于记录播放历史（继续播放故事）。
// fn 会在独立 goroutine 里异步调用，不阻塞播放主流程，也不需要关心 p.mu。
func WithOnEpisodeChange(fn func(item SongItem)) PlayerOption {
	return func(p *Player) {
		p.onEpisodeChange = fn
	}
}

// WaitInitialState 阻塞直到收到第一次 playing 事件，或 ctx 截止。
// 返回 true 表示已经收到、CurrentState 的值现在是可信的；
// 返回 false 表示超时，调用方应自行决定如何兜底。
func (p *Player) WaitInitialState(ctx context.Context) bool {
	select {
	case <-p.initialStateCh:
		return true
	case <-ctx.Done():
		return false
	}
}

// PlayURL 播放 URL（通过 RPC 调用设备）
//
// 复合脚本，**先重置 mediaplayer 状态，再下发新 URL**（这个顺序由实机测试验证）：
//  1. player_wakeup action:stop          —— 解除可能存在的 wakeup 锁
//  2. player_play_operation action:play  —— 把 mediaplayer 唤醒到"准备播放"状态
//  3. sleep 0.1                          —— 给前两步生效的时间
//  4. player_play_url                    —— 切到新 URL，开始播
//
// 为什么这个顺序对，反过来不对？
//
// 通过 strings dump /usr/bin/mediaplayer 二进制找到的关键证据：
//
//	"play_url,in wakeup status"
//	"%s, pause last player in case hear last play."
//	"%s, Playing CPMedia, in wakeup period, end lock_start."
//	"%s, Playing CPMedia, stop player!"
//
// mediaplayer 内部状态机：
//   - 用户说唤醒词后，device 调 player_wakeup action:start，mediaplayer 进入 wakeup 状态
//   - 此时 player_play_url 不立即播，只缓存到 PlayList（等云端 NLP 最终决策）
//   - 正常路径：云端 NLP 决策完 → wakeup.sh ready 调 player_wakeup action:stop → 缓存的 URL 真正播
//   - 我们 AbortXiaoAI 把云端杀了 → 缓存永远不被消化 → 失声
//
// 已经踩过的坑：
//
//	✗ 单纯只 player_play_url：mediaplayer 缓存 URL 等云端，云端死了，永不播放（失声）。
//	  第一首歌"侥幸"能播是因为 Speak (tts_play.sh) 内部最后一步 mphelper play 隐式触发了
//	  player_play_operation action:play，正好把缓存的歌唤醒。handleNext 没有 tts_play.sh
//	  做这个唤醒，所以第二首失声。
//
//	✗ 先 player_play_url 再 wakeup_stop：wakeup_stop 的内部回调
//	  "Playing CPMedia, in wakeup period, end lock_start" → "stop player!" 会找到我们刚
//	  PlayURL 的歌，把它停掉。实机表现：GET 文件成功、Playing 短暂上报，1 秒后变 Idle 无声。
//
//	✓ **本方案**：player_reset（彻底清掉云端 push 来的 PlayList/track_list/wakeup 状态机
//	  等所有内部状态）+ player_play_operation play（把 mediaplayer 切到 "ready" 模式）→
//	  sleep 让状态稳定 → 再下发 player_play_url，URL 直接进入正常播放流程。
//
// 关于 player_reset 放在头：
//
//	这条是 SetQueue 路径专门用来打掉云端残留 PlayList 的，但放在 PlayURL 头部对所有调用路径
//	都安全：
//	  - SetQueue → playItemLocked → PlayURL：必须 reset，否则播完一首被云端 PlayList 接管
//	  - 自动续播 OnPlayingStatus("Idle") → playItemLocked → PlayURL：此时 mediaplayer 已经 Idle，
//	    reset 没影响。我们队列里 s[i]→s[i+1] 用同一个 PlayURL，不要再多分支
//	  - 用户 Next/Previous → PlayURL：reset 顺手打断 s[i]，正合心意
//
// 用 `;` 而非 `&&`，保证每一步都执行（前一步失败也不阻断后续）。
//
// 副作用：更新 lastPlayURLAt，用于 OnPlayingStatus 的 grace period 判断
// （刚 PlayURL 之后短时间内的 Idle 都视为切歌抖动/加载延迟，不触发"歌曲播完"逻辑）。
func (p *Player) PlayURL(url string) error {
	p.mu.Lock()
	p.lastPlayURLAt = time.Now()
	p.mu.Unlock()

	if p.playURL != nil {
		log.Printf("🌐 [music/player] PlayURL (override): %s", url)
		return p.playURL(url)
	}
	log.Printf("🌐 [music/player] PlayURL → 设备: %s", url)
	script := fmt.Sprintf(
		`ubus -t 2 call mediaplayer player_reset >/dev/null 2>&1 ; `+
			`ubus -t 2 call mediaplayer player_play_operation '{"action":"play","media":"common"}' >/dev/null 2>&1 ; `+
			`sleep 0.1 ; `+
			`ubus -t 5 call mediaplayer player_play_url '{"url":"%s","type":1}' || true`,
		url,
	)
	// 5s 给 100ms sleep 留足空间，ubus 调用本身毫秒级
	timeout := uint64(5000)
	resp, err := connect.GetRPC().CallRemote("run_shell", script, &timeout)
	if err != nil {
		log.Printf("❌ [music/player] PlayURL RPC 失败: %v", err)
		return err
	}
	r := decodeShellResult(resp)
	// stdout 一般是最后 player_play_url 的输出 {"code": 0}；其他几步 >/dev/null 抑制了。
	log.Printf("🌐 [music/player] PlayURL ubus 返回: %s", briefShellResult(r))
	// 设备端非零退出意味着 mediaplayer 没真正接管播放。必须上抛错误，否则
	// playItemLocked 会把 state 标成 Playing 但实际没声音，PlayingMonitor 永远不会
	// 上报 Playing→Idle 跃迁，整个队列就此卡死。
	if r != nil && r.ExitCode != 0 {
		stderr := strings.TrimSpace(r.Stderr)
		log.Printf("❌ [music/player] PlayURL 设备端非零退出 exit=%d stderr=%q", r.ExitCode, stderr)
		return fmt.Errorf("player_play_url exit=%d stderr=%s", r.ExitCode, stderr)
	}
	return nil
}

// Stop 停止播放
// mphelper pause 是简单命令，秒级返回。
func (p *Player) Stop() error {
	log.Printf("⏹️ [music/player] Stop (mphelper pause)")
	script := "mphelper pause"
	timeout := uint64(2000)
	_, err := connect.GetRPC().CallRemote("run_shell", script, &timeout)
	if err != nil {
		log.Printf("❌ [music/player] Stop RPC 失败: %v", err)
	}
	return err
}

// shellEscapeSingle 把单引号转义为 '\”（用于 shell 单引号包裹的字符串）
func shellEscapeSingle(s string) string {
	return strings.ReplaceAll(s, "'", `'\''`)
}

func summarizeShellText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) > 240 {
		return s[:240] + "...(trunc)"
	}
	return s
}

// treatSpeakExitAsSuccess 判断 tts_play.sh 的非零退出是否可视为“已实际播报成功”。
// 某些机型上会出现 "my_log: not found" + exit=1，但实际已经把 TTS 播放完。
func treatSpeakExitAsSuccess(stdout, stderr string, elapsed time.Duration) bool {
	if elapsed < time.Second {
		// 不到 1 秒就返回，TTS 根本没机会播，按真失败处理
		return false
	}
	// 主要判据：stdout 有 TTS 文件路径，说明语音合成成功且已播放
	if strings.Contains(stdout, "/tmp/tts/tts_") {
		return true
	}
	// 次要判据：libmiplayerlite 播放完成事件
	if strings.Contains(stderr, "event(EndReached) is posted") {
		return true
	}
	return false
}

// Speak 播报反馈语
// 用 /usr/sbin/tts_play.sh：保证用户能听到（ubus mibrain 会被云端 TTS 路由，配置不当时静默不出声）。
//
// 关于阻塞：tts_play.sh 会同步等到音频播完（3-5 秒）才返回。
// 这意味着调用方（如 handlePlay：Speak → SetQueue → PlayURL）会被卡这段时间——
// 但这是用户感知上"听完反馈语再开始放歌"的自然体验，不是凭空延迟。
//
// 重要：tts_play.sh 内部会 `mphelper pause` 抢占 mediaplayer，TTS 播完再 `mphelper play` 恢复。
// 这段时间 PlayingMonitor 会上报 Idle/Paused/Playing 之间的颠簸，绝对不能被当成"歌曲播完"。
// 所以我们在 Speak 前后撑开 suppressUntil 窗口，OnPlayingStatus 在窗口内一律不切歌、不动 state。
//
// 重要前提（client 侧）：本调用现在不会再引发 client_go RPC 雪崩了，因为：
//  1. handler.go 已经把 OnEvent 解耦到 worker goroutine，不再阻塞 WS 读循环；
//  2. client-go 端 tts_play.sh 走 RunShellInterruptible，可被 StopTTS 打断。
//
// 在这次重构之前，tts_play.sh 阻塞会把 read loop 也一起卡住，导致后续 RPC 全部超时。
func (p *Player) Speak(text string) error {
	if p.speak != nil {
		return p.speak(text)
	}
	log.Printf("📝 [music/player] Speak: %q", text)
	p.extendSuppress(speakSuppressPrefix)

	script := fmt.Sprintf(`/usr/sbin/tts_play.sh '%s'`, shellEscapeSingle(text))
	// 超时给得宽裕一些：常见反馈语 1-5 秒；超过 15 秒说明设备端 tts 卡死，及时 bail 出
	timeout := uint64(15000)
	runSpeakOnce := func() (time.Duration, error) {
		start := time.Now()
		resp, err := connect.GetRPC().CallRemote("run_shell", script, &timeout)
		elapsed := time.Since(start)
		if err != nil {
			log.Printf("❌ [music/player] Speak RPC 失败: %v", err)
			return elapsed, err
		}
		r := decodeShellResult(resp)
		log.Printf("📝 [music/player] Speak ubus 返回: %s (耗时=%v)",
			briefShellResult(r), elapsed.Round(time.Millisecond))
		if r != nil && r.ExitCode != 0 {
			stderr := strings.TrimSpace(r.Stderr)
			if treatSpeakExitAsSuccess(r.Stdout, stderr, elapsed) {
				log.Printf("⚠️ [music/player] Speak 返回非零但判定为已成功播报: exit=%d stderr=%q (耗时=%v)",
					r.ExitCode, summarizeShellText(stderr), elapsed.Round(time.Millisecond))
				return elapsed, nil
			}
			err = fmt.Errorf("tts_play.sh exit=%d stderr=%s", r.ExitCode, summarizeShellText(stderr))
			log.Printf("❌ [music/player] Speak 设备端非零退出: exit=%d stderr=%q", r.ExitCode, summarizeShellText(stderr))
			return elapsed, err
		}
		return elapsed, nil
	}

	elapsed, err := runSpeakOnce()
	if err != nil {
		log.Printf("📝 [music/player] Speak 首次失败，%v 后重试一次", speakRetryDelay)
		time.Sleep(speakRetryDelay)
		elapsed, err = runSpeakOnce()
	}

	if err == nil && elapsed < minSpeakBlocking {
		time.Sleep(minSpeakBlocking - elapsed)
	}

	// Speak 返回后再延一会儿：mphelper play 恢复 mediaplayer 状态需要时间
	p.extendSuppress(speakSuppressTail)
	return err
}

// extendSuppress 把 suppressUntil 至少延后到 now+d。
// 不会缩短现有窗口（多次叠加只取最远的那个）。
func (p *Player) extendSuppress(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	until := time.Now().Add(d)
	if until.After(p.suppressUntil) {
		p.suppressUntil = until
	}
}

// stopTTSTimeoutMs 与 chat-go/speaker.go 保持一致：1.5 秒
// stop_tts 是轻打断指令，正常 client 端处理很快返回，过长 timeout 没意义
var stopTTSTimeoutMs uint64 = 1500

// AbortXiaoAI 重启 mico_aivs_lab，杀掉小爱本身的云端 NLP/TTS 流水线。
//
// 解决的问题：
//
//	用户说"播放等一分钟" → instruction.log 写入 → 我们 player_play_url 本地文件
//	同时（！）：本地 ASR 上传云端 → 云端 NLP 识别为播放指令 → 1-2 秒后云端推送试听版 URL
//	→ 小爱设备自动 player_play_url(试听版)，覆盖我们刚才的本地 URL
//	→ mediaplayer 切换，HTTP server 只 GET 一次本地文件就再也没人来读了
//
// 通过重启 mico_aivs_lab，把云端流水线打断掉，我们的 PlayURL 不会被覆盖。
//
// 副作用：mibrain 重启 1-2 秒，期间 tts_play.sh / ubus call mibrain text_to_speech 不可用。
// mediaplayer 是独立服务，不受影响（继续播放我们的 URL）。
//
// fire-and-forget：调用方不等待结果，失败也无所谓。
func (p *Player) AbortXiaoAI() error {
	if p.abortXiaoAI != nil {
		return p.abortXiaoAI()
	}
	log.Printf("🔇 [music/player] AbortXiaoAI: 重启 mico_aivs_lab 杀云端 NLP")
	timeout := uint64(3000)
	_, err := connect.GetRPC().CallRemote(
		"run_shell",
		"/etc/init.d/mico_aivs_lab restart >/dev/null 2>&1",
		&timeout,
	)
	if err != nil {
		log.Printf("⚠️ [music/player] AbortXiaoAI 失败（可忽略）: %v", err)
	}
	return err
}

// ResetMediaPlayer 调 `ubus call mediaplayer player_reset` 把设备端整个
// mediaplayer 状态机翻底重置：
//
//	mibrain_list_reset      —— 清掉云端 NLP 之前推过来的 album_playlist / track_list
//	track_list_reset        —— 清掉当前 track 列表
//	mi_player_reset         —— 重置内部播放器
//	clear_tts_file          —— 清缓存 tts 文件
//	reset last_dialog_id    —— 防止后续被识别成"上次对话"
//
// 这些字段是从 mediaplayer 二进制 strings 出来的内部实现细节，
// 不在 ubus 文档里，但 player_reset 是 mediaplayer 暴露的合法 method。
//
// 解决的问题：
//
//	即使我们用 AbortXiaoAI() 把云端 NLP 杀掉，**之前** 云端已经 push 到
//	mediaplayer 的 PlayList 还活着。当我们的本地歌播完一首、mediaplayer
//	内部进入 Idle，它会按这张残留 PlayList 自动 next 到云端的下一项 →
//	用户观感"随便听听之后被系统的随机插入了"。
//
// 因此每次 SetQueue（用户主动设新队列）前都要清一次。但 PlayURL 路径下不能
// 调，否则我们队列内部切歌也会清掉自己。
//
// 副作用：会打断当前正在播放的内容。在 SetQueue 场景下这正是我们要的，
// 因为下一行就是播自己队列的第一首。
func (p *Player) ResetMediaPlayer() error {
	log.Printf("🧹 [music/player] ResetMediaPlayer: 清 mediaplayer 内部 PlayList/track_list")
	script := `ubus -t 2 call mediaplayer player_reset >/dev/null 2>&1 || true`
	timeout := uint64(3000)
	_, err := connect.GetRPC().CallRemote("run_shell", script, &timeout)
	if err != nil {
		log.Printf("⚠️ [music/player] ResetMediaPlayer 失败（可忽略）: %v", err)
	}
	return err
}

// StopTTS 停止 TTS 播报
func (p *Player) StopTTS() error {
	log.Printf("📝 [music/player] StopTTS (timeout=%dms)", stopTTSTimeoutMs)
	_, err := connect.GetRPC().CallRemote("stop_tts", nil, &stopTTSTimeoutMs)
	if err != nil {
		log.Printf("❌ [music/player] StopTTS RPC 失败: %v", err)
	}
	return err
}

// Queue 返回当前队列（副本）
func (p *Player) Queue() []SongItem {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]SongItem, len(p.queue))
	copy(out, p.queue)
	return out
}

// ClearQueue 清空队列
func (p *Player) ClearQueue() {
	p.mu.Lock()
	hadSong := p.currentSong != nil
	oldLen := len(p.queue)
	p.queue = nil
	p.playlist = nil
	p.history = nil
	p.currentSong = nil
	p.mu.Unlock()
	if hadSong || oldLen > 0 {
		log.Printf("🧹 [music/player] ClearQueue (剩余队列=%d, 有当前曲=%v)", oldLen, hadSong)
	}
}

// SetQueue 设置队列并播放第一首
//
// 不需要在这里单独 ResetMediaPlayer——PlayURL 的 shell pipeline 头部已经带了
// player_reset，会把云端 push 给 mediaplayer 的 PlayList/track_list 一并清掉。
// 留作独立公共方法是为了"显式 reset 但不立即播放"的场景（比如停止后清后台）。
func (p *Player) SetQueue(items []SongItem) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queue = copySongItems(items)
	p.playlist = copySongItems(items)
	p.history = nil
	p.currentSong = nil
	log.Printf("🎵 [music/player] SetQueue: %d 首", len(p.queue))
	if len(p.queue) == 0 {
		return false
	}
	// announce=false：这一批的第一首如果是按集播放，调用方（module.go handlePlay）已经
	// 用 "好的，找到 X 集，从第 X 集开始播放" 报过起始集数了，这里不用再重复播报。
	return p.playNextLocked(false, false)
}

// playNextLocked 播放下一首（调用方需已持锁）
//
// announce 控制是否在播放前先播报"现在播放第 X 集"（仅对 Episode>0 的分集内容生效）。
// 见 playItemLocked 的 announce 参数注释。
func (p *Player) playNextLocked(recordHistory, announce bool) bool {
	if len(p.queue) == 0 {
		p.currentSong = nil
		return false
	}
	item := p.queue[0]
	p.queue = p.queue[1:]
	return p.playItemLocked(item, recordHistory, announce)
}

// playItemLocked 播放指定项（调用方需已持锁）。
//
// announce：是否在真正下发 PlayURL 之前先用 Speak 播报"现在播放第 X 集"（仅当
// item.Episode>0 才会真的说话，普通音乐/在线歌曲 Episode=0 不受影响）。
//
// 为什么需要这个播报、为什么调用方要按场景传不同的值：
//   - 故事/有声书按集播放时，如果不播报集数，小朋友没法知道当前听到第几集，
//     一旦播放中断（比如设备重启、被别的指令打断），也无从得知该从哪一集继续听。
//   - handlePlay 用户主动点播的第一首，已经在 module.go 里用
//     "好的，找到 X 集，从第 X 集开始播放" 报过起始集数了，这里再报一遍会显得啰嗦，
//     所以调用方传 announce=false 跳过。
//   - 后续每次切到新的一集（自动播完切下一集、用户说"下一集"/"上一集"、队列耗尽后
//     自动续播下一批），调用方都传 announce=true，确保每一集开始前都能听到"现在播放
//     第 X 集"。
//   - 单曲循环重播当前这一集时，因为是同一集重复播放，不需要再报一次，传 announce=false。
//
// Speak 是阻塞调用（同步等 tts_play.sh 播完），必须在 PlayURL 之前完成，这样才能保证
// "先说第几集，再播放音频"的顺序，不会被后续的歌声打断或抢跑。
func (p *Player) playItemLocked(item SongItem, recordHistory, announce bool) bool {
	if recordHistory && p.currentSong != nil {
		p.history = append(p.history, *p.currentSong)
	}
	p.currentSong = &item
	// 主动设 Playing：
	// 1) 切歌时 PlayingMonitor 可能不会再上报"Playing"（如果之前已经是 Playing 且 mute_stat 没变化）。
	//    如果这里不主动设，state 会停在 Idle / 旧值，下次真的播完上报 Idle 时 state != Playing，
	//    OnPlayingStatus 不会触发切歌 → 队列死在这里。
	// 2) 加载期间的短暂 Idle 由 playGracePeriod 兜底过滤，不再需要"先设 Idle 防抖"。
	p.state = StatePlaying
	queueLen := len(p.queue)
	histLen := len(p.history)
	p.mu.Unlock()
	if announce && p.announceEpisodeBeforePlay && item.Episode > 0 {
		// 播报必须先于 PlayURL 且同步等待播完，保证小朋友先听到"第几集"，再听到正文内容。
		_ = p.Speak(fmt.Sprintf("现在播放第%d集", item.Episode))
	}
	err := p.PlayURL(item.URL)
	p.mu.Lock()
	if err != nil {
		log.Printf("❌ [music/player] 播放失败: %v (path=%s)", err, item.Path)
		p.currentSong = nil
		p.state = StateIdle
		return false
	}
	log.Printf("🎵 [music/player] 正在播放: %s (剩余队列=%d 历史=%d)", item.Path, queueLen, histLen)
	if p.onEpisodeChange != nil && item.Episode > 0 {
		// 异步触发：磁盘 IO 不应该占用 p.mu、也不应该拖慢播放主流程。
		cb := p.onEpisodeChange
		go cb(item)
	}
	return true
}

// OnPlayingStatus 处理 playing 事件状态变化
// status 为 "Playing" / "Paused" / "Idle"
//
// 三道护栏（按顺序拒绝伪 Idle）：
//  1. suppressUntil：Speak/TTS 期间，所有状态变化全部忽略（不动 state、不切歌）；
//  2. currentSong == nil：我们没有主动播任何东西，Idle 跟我们无关（避免 Speak 阶段
//     把 state 改成 Playing 后又被自身 mphelper pause 触发"切歌"）；
//  3. playGracePeriod：距离上次 PlayURL 不到 playGracePeriod 的 Idle 视为切歌抖动/加载延迟。
//
// 正常 case（歌真的播完）：currentSong != nil、过了 grace、不在 suppress 中、state == Playing
// → 触发 nextLocked（按 mode 走顺序/循环/随机或重播当前）。
func (p *Player) OnPlayingStatus(status string) {
	// 收到第一次 playing 事件就算"初始状态可信"，不区分具体值。
	// 放在锁外避免阻塞 close。
	p.initialStateOnce.Do(func() {
		close(p.initialStateCh)
	})

	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	if !p.suppressUntil.IsZero() && now.Before(p.suppressUntil) {
		log.Printf("⏸️ [music/player] 忽略 playing=%s (suppress 中, 还剩 %v)",
			status, p.suppressUntil.Sub(now).Round(time.Millisecond))
		return
	}

	prev := p.state
	switch status {
	case "Idle":
		if p.state != StatePlaying {
			p.state = StateIdle
			return
		}
		if p.currentSong == nil {
			log.Printf("🎚️ [music/player] 忽略 Idle: currentSong 为空（非我方触发的播放结束）")
			p.state = StateIdle
			return
		}
		if !p.lastPlayURLAt.IsZero() {
			if since := now.Sub(p.lastPlayURLAt); since < playGracePeriod {
				log.Printf("🎚️ [music/player] 忽略 Idle: 距上次 PlayURL %v < grace=%v (切歌抖动/TTS 抢占/加载延迟)",
					since.Round(time.Millisecond), playGracePeriod)
				// 注意：state 保持 Playing，让后续真正稳定的状态决定走向
				return
			}
			// 真实遇到的 bug：设备物理暂停/停止键按下后，mute_stat 上报的不是 "Paused"(2)
			// 而是直接变成 "Idle"（跟"这首歌自然播完了"从 mute_stat 的角度完全无法区分）。
			// 结果就是用户按物理暂停键，我们却当成"播完了"自动切到下一首——物理暂停键
			// 形同虚设。用已经解析出的真实时长（IndexedSong.DurationMs）做个兜底判断：
			// 如果距上次 PlayURL 的时间远小于这首歌真实应该播放的时长，大概率不是"自然播完"，
			// 而是用户手动打断了播放，这种情况下不应该自动切下一首，老老实实停在这里。
			//
			// 但这个保护只对普通音乐（Episode=0）启用：
			// 故事/有声书按集播放（Episode>0）里，设备偶发的早期 Idle（并非用户真的要暂停）
			// 更常见，若在这里直接判定"手动暂停"会导致队列卡死在某一集，表现为"播了几集后不
			// 自动续播"。对分集内容优先保证连续播放，早期 Idle 仍然按自然播完去续下一集。
			if p.currentSong.DurationMs > 0 && p.currentSong.Episode <= 0 {
				expected := time.Duration(p.currentSong.DurationMs) * time.Millisecond
				since := now.Sub(p.lastPlayURLAt)
				if since < time.Duration(float64(expected)*earlyIdleThresholdRatio) {
					log.Printf("⏸️ [music/player] 忽略 Idle: 只播放了 %v/%v (<%.0f%%)，判定为设备被手动暂停/停止"+
						"（比如按了物理暂停键），不自动切下一首",
						since.Round(time.Second), expected.Round(time.Second), earlyIdleThresholdRatio*100)
					p.state = StateIdle
					return
				}
			}
		}
		log.Printf("🎚️ [music/player] 状态转换 Playing→Idle, 触发自动切歌 (mode=%d)", p.mode)
		advanced := false
		if p.mode == PlaybackModeRepeatOne {
			advanced = p.replayCurrentLocked()
		} else {
			advanced = p.nextLocked(true)
		}
		if advanced {
			// 如果已经成功切到下一首，状态应该保持 Playing。
			// 之前这里无条件写回 Idle，会和上面的切歌结果打架：
			//   1) nextLocked/playItemLocked 已经把 state 设成 Playing 并开始播下一首；
			//   2) 但这里又立刻覆盖成 Idle；
			//   3) 接下来设备上报的 playing=Playing 常常还在 suppress 窗口里，被忽略，
			//      于是 state 会长期卡在 Idle，导致依赖 state==Playing 的心跳/看门狗停摆。
			p.state = StatePlaying
		} else {
			p.state = StateIdle
		}
	case "Playing":
		if prev != StatePlaying {
			log.Printf("🎚️ [music/player] 状态转换 %d→Playing", prev)
		}
		p.state = StatePlaying
	case "Paused":
		// 不切歌，但记录一下（仅在状态变化时）
		if prev != StateIdle {
			log.Printf("🎚️ [music/player] 状态转换 %d→Paused (不切歌)", prev)
		}
	default:
		log.Printf("⚠️ [music/player] 未知 playing 状态: %q (保守处理)", status)
	}
}

// Next 用户主动"下一首"。
//
// 跟 OnPlayingStatus 的自动切歌不同：在 RepeatOne 模式下，用户手动 Next 也会
// 跳到下一首（跟主流播放器一致——单曲循环只影响自动行为，用户操作永远走下一项）。
// 自动 Idle 的重播逻辑放在 OnPlayingStatus 里。
func (p *Player) Next() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.nextLocked(true)
}

func (p *Player) nextLocked(recordHistory bool) bool {
	if len(p.queue) > 0 {
		// announce=true：切到队列里的下一项，如果它是分集内容就播报"现在播放第 X 集"。
		return p.playNextLocked(recordHistory, true)
	}
	switch p.mode {
	case PlaybackModeRepeatAll, PlaybackModeShuffle:
		if len(p.playlist) == 0 {
			log.Printf("➡️ [music/player] 队列空且 playlist 也空，无法循环 (mode=%d)", p.mode)
			return false
		}
		p.queue = copySongItems(p.playlist)
		if p.mode == PlaybackModeShuffle {
			log.Printf("➡️ [music/player] 循环+乱序: 重新填充 %d 首", len(p.queue))
			rand.Shuffle(len(p.queue), func(a, b int) {
				p.queue[a], p.queue[b] = p.queue[b], p.queue[a]
			})
		} else {
			log.Printf("➡️ [music/player] 列表循环: 重新填充 %d 首", len(p.queue))
		}
		return p.playNextLocked(recordHistory, true)
	default:
		if more := p.tryFetchMoreLocked(); len(more) > 0 {
			p.queue = more
			p.playlist = copySongItems(more)
			return p.playNextLocked(recordHistory, true)
		}
		log.Printf("➡️ [music/player] 队列耗尽 (mode=sequence)，停止")
		return false
	}
}

// tryFetchMoreLocked 调用方需已持锁。队列/playlist 都耗尽时尝试通过 onExhausted 拉取
// 下一批（典型场景：故事按集播放，当前这 20 集放完了，接着拉 21~40 集）。
// 没有设置回调、或回调返回空，都视为"真的没有更多了"。
func (p *Player) tryFetchMoreLocked() []SongItem {
	if p.onExhausted == nil {
		return nil
	}
	more := p.onExhausted()
	if len(more) == 0 {
		return nil
	}
	log.Printf("➡️ [music/player] 队列耗尽，自动续播下一批: %d 首", len(more))
	return more
}

// Previous 用户主动"上一首"。同 Next，RepeatOne 也跳出当前曲。
func (p *Player) Previous() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.history) == 0 {
		return false
	}
	prev := p.history[len(p.history)-1]
	p.history = p.history[:len(p.history)-1]
	if p.currentSong != nil {
		p.queue = append([]SongItem{*p.currentSong}, p.queue...)
	}
	// announce=true：Previous 切到的是不同的一集，需要播报。
	return p.playItemLocked(prev, false, true)
}

func (p *Player) SetMode(mode PlaybackMode) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mode != mode {
		log.Printf("🎚️ [music/player] 播放模式: %d → %d", p.mode, mode)
	}
	p.mode = mode
}

// SetExhaustedHandler 设置/清空队列耗尽时的续播回调，见 onExhausted 字段注释。
// 传 nil 表示禁用续播（队列耗尽就正常停止）——每次 SetQueue 一批新内容时，调用方应该
// 显式设置（故事/按集播放场景）或清空（普通搜索/随机播放场景）它，避免残留上一次的续播
// 逻辑错误地接到这一次不相关的播放上。
func (p *Player) SetExhaustedHandler(fn func() []SongItem) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onExhausted = fn
}

func (p *Player) Mode() PlaybackMode {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mode
}

func (p *Player) replayCurrentLocked() bool {
	if p.currentSong == nil {
		return false
	}
	// announce=false：单曲循环重播的是同一集，不需要重复播报。
	return p.playItemLocked(*p.currentSong, false, false)
}

// CurrentState 返回当前播放状态
func (p *Player) CurrentState() PlaybackState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

func copySongItems(items []SongItem) []SongItem {
	out := make([]SongItem, len(items))
	copy(out, items)
	return out
}

// BuildQueueFromSongs 从 IndexedSong 列表构建队列，并授权 HTTP 服务访问这些文件。
// seriesName 非空时会写入每个 SongItem.SeriesName，供 onEpisodeChange 回调记录播放历史
// （"继续播放故事"依赖它）；普通音乐/随机播放/在线歌曲场景传空字符串即可。
func (p *Player) BuildQueueFromSongs(songs []IndexedSong, seriesName string) []SongItem {
	items := make([]SongItem, 0, len(songs))
	skipped := 0
	for _, s := range songs {
		p.fileServer.AllowFile(s.Path)
		url := p.fileServer.CreateFileURL(s.Path)
		if url != "" {
			items = append(items, SongItem{Path: s.Path, URL: url, Size: s.Size, DurationMs: s.DurationMs, Episode: s.Episode, SeriesName: seriesName})
		} else {
			skipped++
			log.Printf("⚠️ [music/player] BuildQueue 跳过 (URL 生成失败): %s", s.Path)
		}
	}
	if skipped > 0 {
		log.Printf("⚠️ [music/player] BuildQueue 完成: %d 首 (跳过 %d)", len(items), skipped)
	}
	return items
}

// advanceTickInterval 主动续播检查间隔。必须比 preemptMargin 小得多，否则会因为
// tick 粒度太粗而错过"提前一点点"这个窗口，退化成事后才发现。
const advanceTickInterval = 1 * time.Second

// preemptMinMargin 提前量下限：即使配置成 0 或很小的值，也至少提前这么久发起下一首，
// 保留一点点缓冲应对 RPC 往返延迟本身。
const preemptMinMargin = 1 * time.Second

// preemptMaxMarginRatio 提前量上限占真实时长的比例：不管配置多大，最多不超过时长的 20%，
// 避免因为 CBR 估算误差或配置不当，把一首歌很大一截内容都提前切掉。
const preemptMaxMarginRatio = 0.2

// defaultPreemptMargin 默认提前量：在预估时长结束前这么久，就主动切到下一首。
// 实测验证过 3s 能稳定抢在设备原生 CP 续播机制之前拿到控制权；2s 在后续实测里同样稳定有效，
// 且能进一步缩小对每首歌尾部内容的影响，所以把默认值调小到 2s。
const defaultPreemptMargin = 2 * time.Second

// WatchdogLoop 主动续播机制：趁着我们已经能精确算出每首 mp3 真实播放时长（见
// mp3duration.go），在预估时长结束前一点点就主动切到下一首——从根上避免设备进入
// "真正 idle、且我们还没喂下一个 URL"的空窗期。
//
// 为什么必须是"提前"而不是"事后"：实测抓到了真正的根因——播放期间周期性 dump
// `ubus call mediaplayer player_get_context`，在我们本地曲目自然播完的那个时间点附近，
// 返回结果里突然多出一个 `audio_meta.cp.name = "ximalaya"` 字段，说明设备本身有一套
// **跟 mico_aivs_lab 完全无关**的"第三方内容提供商（CP）续播"机制：只要 mediaplayer
// 真正进入 idle 且没有新内容排队，它就会去恢复你之前用原生小爱在喜马拉雅上听到一半的
// 内容。这个空窗期发生在"track 自然播完"和"我们轮询 mute_stat 检测到 Idle 再回传 RPC
// 播下一首"这段往返延迟之内——纯被动等 Idle 事件、事后再反应，天生就会跟设备自己的这套
// 内部机制赛跑，而设备本地触发比我们"轮询 + 网络往返"更快，我们经常会输掉这场比赛。
// AbortXiaoAI 心跳杀的是 mico_aivs_lab，跟这套 CP 续播机制完全不搭边，所以怎么调都没用。
//
// 现在的策略：既然知道真实时长，就不用等它真的播完——提前 preemptMargin（默认 2 秒，
// 留了 1 秒下限和"最多不超过时长 20%"的上限做保护）主动调用下一首的 PlayURL。
// 这样设备端根本没有机会进入"idle 且没有下一个 URL"的状态，CP 续播自然没有可乘之机。
// 真正的 Idle 事件依然正常处理（OnPlayingStatus），两者不冲突：谁先触发，"下一首"的
// 队列弹出就已经完成，后到的那个只是操作在空队列/已变化的 currentSong 上，不会重复播放。
//
// 实测记录（2026-07-31）：4 首歌连续通过主动续播成功切歌，全程诊断快照从未再出现
// `audio_meta.cp` 字段，证明这个策略确实能稳定抢在 CP 续播之前拿到控制权。
//
// 只对能解析出真实时长的 .mp3 生效（DurationMs>0）；其他格式/在线直链没有这个保护，
// 完全依赖 Idle 事件本身。
//
// 阻塞直到 ctx 被取消，调用方应该在独立 goroutine 里跑。
func (p *Player) WatchdogLoop(ctx context.Context) {
	if !p.watchdogEnabled {
		log.Printf("🐕 [music/player] 主动续播机制已禁用 (player.watchdog_enabled=false)")
		return
	}
	log.Printf("🐕 [music/player] 主动续播机制已启用: 检查间隔=%v, 提前量=%v (只对能解析出真实时长的 mp3 生效)",
		advanceTickInterval, p.preemptMargin)
	ticker := time.NewTicker(advanceTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.checkWatchdog()
		}
	}
}

// checkWatchdog 单次检查：如果当前曲目已经接近（或超过）真实时长，就提前/及时推进到下一首，
// 抢在设备端 CP 续播机制之前拿到"下一个要播的 URL 是什么"的控制权。
func (p *Player) checkWatchdog() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.state != StatePlaying || p.currentSong == nil {
		return
	}
	now := time.Now()
	// Speak/新指令处理期间的状态不可信，一并让路，避免跟正常流程打架。
	if !p.suppressUntil.IsZero() && now.Before(p.suppressUntil) {
		return
	}
	if p.lastPlayURLAt.IsZero() {
		return
	}
	if p.currentSong.DurationMs <= 0 {
		// 时长未知（非 mp3、解析失败、LX 在线直链等），不瞎猜，完全依赖 Idle 事件本身。
		return
	}
	dur := time.Duration(p.currentSong.DurationMs) * time.Millisecond
	margin := p.preemptMargin
	if margin < preemptMinMargin {
		margin = preemptMinMargin
	}
	if maxMargin := time.Duration(float64(dur) * preemptMaxMarginRatio); margin > maxMargin {
		margin = maxMargin
	}
	preemptAt := p.lastPlayURLAt.Add(dur - margin)
	if now.Before(preemptAt) {
		return
	}
	log.Printf("🐕 [music/player] 主动续播: %s 预估时长=%v, 提前量=%v，距上次 PlayURL 已过 %v，"+
		"抢在设备原生续播机制（如第三方内容源 CP 恢复）接管前主动切到下一首",
		p.currentSong.Path, dur.Round(time.Second), margin.Round(time.Second),
		now.Sub(p.lastPlayURLAt).Round(time.Second))
	if p.mode == PlaybackModeRepeatOne {
		p.replayCurrentLocked()
	} else {
		p.nextLocked(true)
	}
}

// AbortXiaoAIHeartbeatLoop 在本地队列活跃播放期间，周期性重复调用 AbortXiaoAI
// （重启 mico_aivs_lab），防止小爱云端在后台重新连接、重新同步账号状态后，把
// "之前暂停的故事/新闻"resume 到 mediaplayer，抢占我们正在播放的本地队列。
//
// 背景（详见 CommandsConfig.AbortHeartbeatIntervalSec 的注释）：AbortXiaoAI 只在下达
// 播放指令那一刻重启一次 mico_aivs_lab，但它是持续运行的本地代理，重启后几秒内就会自动
// 重连云端、重新同步账号在云端记录的"上次暂停内容"。实测表现：本地队列能正常自动切
// 一次下一集，再往后突然就换成了很久以前听到一半的别的内容，中间完全没有 Idle 事件——
// 说明 mediaplayer 内部是被云端直接接管切换的，不是我们队列耗尽。
//
// 通过在本地队列仍在播放时周期性重复 AbortXiaoAI，在云端每次重新连接、还没攒够触发
// resume 的时间窗口内就把它再打断一次，从源头上不给它机会。
//
// interval<=0 时直接返回（表示已禁用）。阻塞直到 ctx 被取消，调用方应该在独立 goroutine 里跑。
func (p *Player) AbortXiaoAIHeartbeatLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		log.Printf("🔇 [music/player] AbortXiaoAI 播放心跳已禁用 (interval<=0)")
		return
	}
	log.Printf("🔇 [music/player] AbortXiaoAI 播放心跳已启用: 间隔=%v", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.mu.Lock()
			active := p.state == StatePlaying && p.currentSong != nil
			// Speak/新指令处理期间自己就会做 AbortXiaoAI，心跳这次跳过，避免重复重启。
			suppressed := !p.suppressUntil.IsZero() && time.Now().Before(p.suppressUntil)
			p.mu.Unlock()
			if !active || suppressed {
				continue
			}
			log.Printf("🔇 [music/player] AbortXiaoAI 播放心跳: 重启 mico_aivs_lab，防止云端恢复后台暂停内容抢占")
			_ = p.AbortXiaoAI()
		}
	}
}

// runShellCommand 执行一段 shell 脚本并返回 stdout，测试可通过 p.runShell 覆盖。
func (p *Player) runShellCommand(script string, timeoutMs uint64) (string, error) {
	if p.runShell != nil {
		return p.runShell(script)
	}
	resp, err := connect.GetRPC().CallRemote("run_shell", script, &timeoutMs)
	if err != nil {
		return "", err
	}
	r := decodeShellResult(resp)
	if r == nil {
		return "", nil
	}
	return r.Stdout, nil
}

// snapshotDeviceContext 诊断用：拉取设备端 mediaplayer 的完整上下文（`player_get_context`），
// 原样打印 stdout。
//
// 背景：连续两轮修复（看门狗时长估算、AbortXiaoAI 播放心跳）都没能解决"自动切一次下一集后，
// 后面就被切到很久以前用原生小爱听到一半的别的内容"这个问题——心跳已经确认按预期每 60 秒
// 稳定重启一次 mico_aivs_lab，但问题依旧复现，说明"mico_aivs_lab 重连后同步云端暂停内容"
// 这个理论被证伪了，真正的触发源目前还不确定（有可能是另一个跟 mico_aivs_lab 无关的
// 设备内部服务/定时任务）。
//
// 在确认真正机制之前继续瞎猜着修没有意义，所以先加这个纯诊断快照：周期性把
// `player_get_context` 的原始输出打到日志里，下次问题复现时，对比"劫持前"和"劫持后"
// 的快照，看看这个字段里有没有透露出真正在起作用的是谁（比如某个 album/track id 字段）。
//
// 目前不解析具体字段（没有官方文档，schema 未知），先把原始 JSON 打出来人工看。
func (p *Player) snapshotDeviceContext(reason string) {
	out, err := p.runShellCommand("ubus -t 2 call mediaplayer player_get_context 2>&1", 3000)
	if err != nil {
		log.Printf("🔬 [music/player] player_get_context 诊断快照失败 (%s): %v", reason, err)
		return
	}
	log.Printf("🔬 [music/player] player_get_context 诊断快照 (%s): %s", reason, strings.TrimSpace(out))
}

// DiagnosticContextLoop 播放期间周期性打印 player_get_context 原始输出，纯诊断用途。
// interval<=0 时直接返回（禁用）。阻塞直到 ctx 被取消，调用方应该在独立 goroutine 里跑。
func (p *Player) DiagnosticContextLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	log.Printf("🔬 [music/player] 诊断快照已启用: 间隔=%v (排查'自动切到别的内容'问题用，确认根因后会移除)", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.mu.Lock()
			active := p.state == StatePlaying && p.currentSong != nil
			p.mu.Unlock()
			if !active {
				continue
			}
			p.snapshotDeviceContext("periodic")
		}
	}
}
