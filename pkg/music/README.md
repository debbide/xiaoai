# pkg/music

可复用的本地音乐播放模块，目前由 [apps/chat](../../apps/chat/README.md) 集成。纯 Go 实现，无 ffmpeg 依赖，通过监听客户端上报的 `playing` 事件实现自动切歌。

> ℹ️ **apps/gemini 不集成本模块**：apps/gemini 是纯实时对话场景（半双工），不会消费 `instruction` 事件。如需本地音乐能力请使用 apps/chat。

## 功能

- **曲库索引**：递归扫描配置目录，使用 dhowden/tag 提取元数据（歌名/歌手/专辑）
- **关键词搜索**：按歌名、歌手、专辑、文件名模糊匹配，并按相关性排序
- **语音指令**：播放、下载、上一首、下一首、停止、随机播放、播放模式、刷新曲库
- **HTTP 文件服务**：`/file/{hex(path)}/{filename}`，白名单 + Range 支持
- **播放队列**：搜索/随机结果入队，支持顺序、单曲循环、全部循环、随机播放模式
- **自动切歌**：监听 `playing` 事件 Idle 状态，触发下一首
- **故事/有声书**：「播放故事」「播放有声书」为独立触发词，不与普通「播放音乐」混淆；支持「播放故事西游记11集」「播放故事三国第一季第48集」等，按集数排序、指定集播放、自动续播下一集
- **默认故事上下文**：可通过口令设置当前默认故事；设置后说「播放第4集」「播放故事第300集」会自动补全为默认故事对应集数
- **继续播放故事**：自动记录最近播放的故事系列名+集数（落盘持久化，跨重启有效），说「继续播放故事」即可自动从上次的位置接着播放

---

## 配置文件示例

完整配置示例（所有字段均可选，未配置时使用默认值）：

```yaml
music:
  enabled: true
  dirs:
    - /path/to/music1
    - /path/to/music2

  # 支持的音频格式，缺省为 [.mp3, .flac, .wav, .m4a, .aac, .ogg]
  extensions:
    - .mp3
    - .flac
    - .wav
    - .m4a
    - .aac
    - .ogg

  search:
    max_results: 20              # 搜索/随机返回的最大数量
    refresh_interval_sec: 0       # 定时刷新间隔（秒），0=禁用
    index_file: "cache/music_index.json"  # 索引缓存路径

  history:
    file: "cache/play_history.json"  # 播放历史文件（"继续播放故事"依赖它，只记录故事/有声书）

  commands:
     play_keywords: ["播放"]       # 前缀匹配，提取后缀为搜索关键词
     download_keywords: ["下载"]   # 明确下载在线歌曲，下载成功后播放
    stop_keywords:
      - "停止播放"
      - "暂停播放"
      - "暂停"
      - "停止"
      - "闭嘴"
      - "别放了"
      - "不要放了"
      - "关机"
    next_keywords: ["下一首", "下一个", "下一集", "下一章", "下一回", "下集", "换一集"]
    previous_keywords: ["上一首", "上一个", "上一集", "上一章", "上一回", "上集"]
    continue_story_keywords: ["继续播放故事", "接着播放故事", "继续听故事", "接着听故事", "继续讲故事", "接着讲故事", "继续故事", "接着故事"]
    refresh_keywords: ["刷新曲库"]
    random_play_keywords: ["随便听听"]       # 随机取一批歌曲播放
    repeat_one_keywords: ["单曲循环"]
    repeat_all_keywords: ["全部循环", "列表循环"]
    shuffle_mode_keywords: ["随机播放"]     # 切换为随机播放模式
    abort_xiaoai_on_play: true    # 播放本地歌曲前打断小爱云端 NLP，避免云端试听版 URL 覆盖本地播放
    abort_xiaoai_heartbeat_sec: 30 # 播放期间周期性打断小爱云端，0=关闭（默认 30）

  http:
    port: 18080
    base_url: ""                 # 空则自动检测 LAN IP，多网卡时建议显式配置

  # LX Sync Server 在线音乐（可选）
  # 本地曲库搜不到时，调用 LX Server 搜索并获取播放直链
  lx:
    enabled: false
    base_url: "http://localhost:9527"
    user_token: ""               # 推荐：LX 普通用户 API Token（x-user-token）
    username: ""                 # 可选：普通用户名；未配置 user_token 时自动登录获取 token
    password: ""                 # 可选：普通用户密码；日志不会打印此值
    frontend_auth: ""            # 可选：admin 管理口令；不建议日常播放依赖
     download: false              # 已废弃为自动下载开关；普通播放始终在线播放
    download_dir: ""             # 下载目录；空则使用 music.dirs[0]
    source: "kw"                 # kw / tx / wy / kg / mg
    quality: "128k"              # 128k / 320k / flac，取决于源和歌曲
    timeout_sec: 10

  # 播放器行为（可选）
  player:
    watchdog_enabled: false            # 主动续播机制，默认关闭
    preempt_margin_sec: 2              # 主动续播提前量（秒），默认 2
    diagnostic_interval_sec: 0         # 诊断快照间隔（秒），0=关闭（默认）
    announce_episode_before_play: true # 切到新一集前是否播报“现在播放第X集”，默认开启

  # 故事/有声书分类（可选），用于精确匹配与集数解析
  stories:
    - name: "西游记"
      aliases: ["西游"]
      dir: "/path/to/music/儿童/西游记"   # 可选，限定目录
      episode_pattern: "第?(\\d+)[集回]"  # 保留字段：当前版本尚未按单个 story 覆盖集数提取规则
    - name: "水浒传"
      aliases: ["水浒"]
      dir: "/path/to/music/儿童/水浒传"
```

### 最简配置

仅启用并指定目录即可：

```yaml
music:
  enabled: true
  dirs:
    - /home/user/Music
```

### 故事/有声书目录建议

儿童故事可按「系列名/集数」组织，无需配置 `stories` 即可使用：

```
/music/
├── 流行/              # 普通音乐
└── 儿童/              # 故事
    ├── 西游记/
    │   ├── 第01集.mp3
    │   ├── 第02集.mp3
    │   └── ...
    ├── 水浒传/
    │   ├── 水浒传_01.mp3
    │   └── ...
```

文件名支持：`第11集`、`11集`、`第11回`、`01`（纯数字前缀，如 `048.48 xxx.mp3` 也能识别为第 48 集）等格式。配置 `stories` 可添加别名（如「西游」→「西游记」）或限定目录。

语音指令里的集数同时支持阿拉伯数字和中文数字：「第11集」「第六集」「第二十三集」「第一百集」都能正确解析（小爱 ASR 对个位数经常转写成中文数字，比如把"第6集"识别成"第六集"）。

### 故事/音乐触发词分离

默认情况下「播放」是唯一的播放前缀，「播放音乐 xxx」和「播放故事 xxx」都会命中同一个 `play_keywords`，
再由模块内部剥离资源类型词（`故事`、`有声书`、`音乐`、`歌曲`、`本地音乐`、`本地歌曲`）。
其中 **「故事」「有声书」是独立识别的前缀**：
只要说出「播放故事 xxx」或「播放有声书 xxx」，就会强制走"按集搜索、按集排序自动续播"的故事分支，
不再要求 `stories` 配置里系列名/别名精确匹配才能识别成故事——这样"播放小朋友的故事"和"播放音乐"不会再互相干扰。

如果不需要自定义关键词，默认已经支持：

```
播放故事 西游记          → 故事模式，从第 1 集开始
播放故事 西游记 第11集    → 故事模式，从第 11 集开始
播放有声书 三国演义       → 故事模式
播放音乐 晴天             → 普通音乐搜索
播放 晴天                 → 普通音乐搜索（未加资源词，走通用搜索，不下载）
下载 晴天                 → 下载在线歌曲，播报下载成功后播放本地文件
```

如果想自定义前缀词，可在 `commands` 里覆盖（会整体替换默认值，注意保留原有词）：

```yaml
music:
  commands:
    play_keywords: ["播放"]   # 触发词前缀不变，仍是"播放"
```

> 目前故事/有声书前缀词（`故事`、`有声书`）尚未做成独立的 yaml 配置项，如需增删同义词，
> 修改 `pkg/music/commands.go` 里的 `storyResourcePrefixes` 即可（如需要，欢迎提 issue 让它可配置化）。

### 配置字段说明（实现对齐）

- `commands.abort_xiaoai_heartbeat_sec`：本地队列播放期间，周期性 AbortXiaoAI 的间隔（秒）；默认 `30`，显式设 `0` 关闭。
- `player.watchdog_enabled`：是否启用主动续播机制（基于时长提前切下一首）；默认 `false`。
- `player.preempt_margin_sec`：主动续播提前量（秒）；默认 `2`。
- `player.diagnostic_interval_sec`：播放期间周期性打印 `player_get_context` 的诊断间隔（秒）；默认 `0`（关闭）。
- `player.announce_episode_before_play`：每次切到新一集前是否播报“现在播放第X集”；默认 `true`。
- `stories[].episode_pattern`：当前版本是保留字段，暂未按单个故事生效；集数提取仍使用模块内置规则（支持 `第11集`、`11集`、`第11回`、`01` 等）。

### 默认故事上下文口令

当家里有很多故事时，可以先设置一个"当前默认故事"，后续只说集数即可，不用每次都重复系列名。

支持口令：

```text
默认故事是 三国演义第一季
默认故事设为 三国演义第一季
设置默认故事 三国演义第一季
把默认故事设为 三国演义第一季
```

设置成功后会播报：

```text
好的，当前默认故事是三国演义第一季
```

设置后可直接说：

```text
播放第4集
播放故事第300集
```

系统会自动按默认故事补全为：

- 「三国演义第一季第4集」
- 「三国演义第一季第300集」

查询与清除口令：

```text
当前默认故事是什么
默认故事是什么
清除默认故事
取消默认故事
```

如果还没设置默认故事就说「播放第4集」，会提示：`请告诉我想播放哪个故事或歌曲`。

### 继续播放故事（跨重启恢复播放进度）

系统会在每次切到新一集时，自动把「系列名 + 集数」记录到磁盘（`history.file`，默认
`cache/play_history.json`），只针对故事/有声书，不记录普通音乐/在线歌曲。

即使中途关机、服务重启，只要说：

```text
继续播放故事
接着播放故事
继续听故事
接着听故事
继续讲故事
接着讲故事
继续故事
接着故事
```

就会自动读取上次记录，从「上次听到的那一集」开始接着播放，不需要再报一遍系列名或集数。

反馈语示例：`好的，接着播放西游记，从第24集继续`。

如果还没有任何播放记录，会提示：`还没有听过故事，不知道该接着播放什么`。

注意：这个功能刻意只做"按集恢复"，不做秒级断点续听——故事是整集 mp3 播放，没有
"播放到第几秒"的概念，只精确到"听到第几集"这个粒度，对日常使用已经够用。

### 多季/嵌套目录的有声书（集数重新编号）

有些资源按「系列/第N季/子目录/文件」组织，且**每一季的集数编号都从 1 重新开始**，
比如：

```
/gushi/
└── 怀沙说《三国演义》爆笑解读国学名著第1季/
    ├── 001-499/
    │   ├── 001.01 发刊词：他们何以成为英雄？｜三国演义.mp3
    │   ├── ...
    │   └── 054.54 攻城的艰难（上）｜三国演义.mp3
    └── 499-729/
└── 怀沙说《三国演义》爆笑解读国学名著第2季/
    ├── 001-499/
    └── 500-/
```

这种情况下文件名本身不含"三国演义"字样，且两季的集数会重复（都有"048"），
**必须通过 `stories[].dir` 显式限定目录**才能区分季与季：

```yaml
music:
  dirs:
    - /root/open-xiaoai/gushi
  stories:
    - name: "三国演义第1季"
      aliases: ["三国第一季", "三国演义第一季", "三国 第一季"]
      dir: "/root/open-xiaoai/gushi/怀沙说《三国演义》爆笑解读国学名著第1季"
    - name: "三国演义第2季"
      aliases: ["三国第二季", "三国演义第二季", "三国 第二季"]
      dir: "/root/open-xiaoai/gushi/怀沙说《三国演义》爆笑解读国学名著第2季"
```

配置好后：

- 「播放故事 三国 第一季 第48集」→ 命中 `三国第一季` 别名，目录限定到第 1 季，
  从 `048.48 贪官与贿赂（上）｜三国演义.mp3` 开始播放。
- 该集播完（设备上报 `playing=Idle`）后，播放器会自动从队列取出下一项，即 `049.49 ...`，无需额外配置。
- 单批入队数量受 `search.max_results` 限制（默认 20），但**这一批放完之后会自动拉取下一批**
  （21~40 集……），不需要手动调大 `search.max_results` 去一次性入队整季——这样也不会
  影响普通音乐搜索/随机播放的结果条数（它们共用同一个 `search.max_results`）。

> ⚠️ **`stories[].dir` 必须落在 `music.dirs` 的扫描范围内**（是其中某个 dir 本身或子目录）。
> `stories[].dir` 只用来做"目录限定过滤"，本身**不会**触发额外扫描——如果只写了
> `stories[].dir` 却忘了把 `/root/open-xiaoai/gushi` 这个上级目录加进 `music.dirs`，
> 那个目录下的文件根本不会被索引，`SearchEpisode` 只会一直 0 命中。模块启动时会对每个
> `stories[].dir` 做一次检查，如果没被 `music.dirs` 覆盖到，会打印 `⚠️ [music] stories[...].dir=... 不在 music.dirs=... 扫描范围内` 的警告日志，出问题时先看日志里有没有这条。

### 按集播放的自动续播（跨批次，不用调大 max_results）

`search.max_results` 是一次搜索/入队的上限，同时也影响普通音乐搜索、随机播放的结果条数。
如果为了播放一整季几百集的故事就把它调到几百上千，会连带让"播放周杰伦"这种普通搜索也
返回一大堆低相关度的结果，通常不是你想要的。

所以按集播放（`useEpisode=true`，即命中"故事/有声书"或带了"第N集"的指令）走的是另一套
机制：每次只入队 `search.max_results` 条（默认 20 集），但这一批**播完之后会自动从上次
最后一集接着拉下一批**（比如第 1~20 集放完，自动接上第 21~40 集……），一直到某一批
拉不到更多内容为止，全程不需要用户再说一遍"播放"，也不用把 `search.max_results` 开大。

这套自动续播只在按集播放时启用；普通搜索结果播完、随机播放列表播完、在线 (LX) 单曲播完，
都还是走"正常停止"，不会被上一次按集播放的续播逻辑影响到。



### 物理暂停/停止键按下后仍然自动切下一首

已知的一个坑：设备物理暂停/停止键按下后，`mphelper mute_stat` 上报的不是 `"Paused"`（2）
而是直接变成 `"Idle"`——从我们的角度这跟"这首歌自然播完了"完全没法区分。结果就是用户按了
物理暂停键，却被当成"播完了"自动切到下一首，物理暂停键形同虚设（甚至因为连续误判，
出现几秒到几十秒就飞速跳过好几集的现象）。

修复：用索引时已经解析出的每首 mp3 真实播放时长（`IndexedSong.DurationMs`）做兜底判断——
如果距上次 `PlayURL` 的时间远小于这首歌真实应该播放的时长（低于 70%），大概率不是"自然
播完"，而是用户手动打断了播放，这种情况下不自动切下一首，老老实实停在这里。这个判断
只对能解析出真实时长的 `.mp3` 生效；其他格式没有这层保护，物理暂停键在这些格式上可能
还是会遇到这个问题。

日志里 `⏸️ [music/player] 忽略 Idle: 只播放了 .../... (<70%)，判定为设备被手动暂停/停止...`
就是这个兜底在生效。

### 云端抢占本地播放

`commands.abort_xiaoai_on_play` 默认开启。用户说「播放某首歌」时，小爱原生云端 NLP 也可能同时识别这句话，并在 1-2 秒后下发试听版 URL，覆盖本地音乐模块刚播放的文件。开启后，音乐模块会在本地播放前后按场景打断小爱云端流水线，避免这个竞态。

时序说明（实现细节）：

- 故事/按集播放（命中故事前缀或带第N集）：`AbortXiaoAI -> Speak -> SetQueue`
  - 这样可以优先压住云端并发语音反馈，避免和本地“好的，找到X集...”提示语重叠。
- 普通音乐播放：`Speak -> AbortXiaoAI -> SetQueue`
  - 这样可以避开部分设备上“刚重启 mico_aivs_lab 后，tts_play.sh 短窗口不稳定”的问题。

如果你只想观察原生小爱的处理结果，或确认设备固件不存在这类覆盖问题，可以显式关闭：

```yaml
music:
  commands:
    abort_xiaoai_on_play: false
```

### CP 续播抢占问题：默认防护 + 可选的主动续播机制

**根因（已通过诊断快照实测确认）**：设备 mediaplayer 自带一套跟 `mico_aivs_lab`（小爱云端
NLP）完全无关的"第三方内容提供商（CP）续播"机制——只要 mediaplayer 真正进入 idle 且没有
排队的下一个 URL，它就会去恢复你之前用原生小爱听到一半的内容。播放期间周期性 dump
`ubus call mediaplayer player_get_context`，在本地曲目自然播完的那个时间点附近，返回结果
里会突然多出一个字段：

```json
"audio_meta": {
  "audio_id": "1273513942314913097",
  "cp": { "name": "ximalaya", "album_id": "53712041", "episode_index": 22 },
  "audio_type": "BOOKS"
}
```

`cp.name` 是 `ximalaya`（喜马拉雅）——这是设备内置的第三方内容源续播功能。

**默认防护：AbortXiaoAI 播放心跳（`commands.abort_xiaoai_heartbeat_sec`，默认 30 秒）**

会在本地队列播放期间周期性重启 `mico_aivs_lab`。虽然理论上 CP 续播跟 `mico_aivs_lab` 是
两套不同机制，重启它并不能直接清掉 CP 那边的状态，但**实测发现把间隔调到 30 秒后，"自动
切一次下一集、再往后被切到喜马拉雅内容"这个问题就没再复现过**，效果比预期的好。目前这是
默认启用、开箱即用的防护，不需要额外配置：

```yaml
music:
  commands:
    abort_xiaoai_heartbeat_sec: 30   # 默认 30 秒；设为 0 可关闭这个心跳
```

**可选的更强力手段：主动续播机制（默认关闭）**

如果心跳方案在你的设备上还是不够用，可以打开这套更"硬"的机制：利用索引时解析 mp3 帧头
拿到的每首歌真实播放时长（`IndexedSong.DurationMs`，见 `pkg/music/mp3duration.go`），在
预估时长结束前 `preempt_margin_sec`（默认 2 秒）就主动切到下一首，从根上不给 CP 续播留
可乘之机——这样设备端根本没有机会进入"idle 且没有下一个 URL"的状态。真正的 `Idle` 事件
依然正常处理，两者不冲突。

```yaml
music:
  player:
    watchdog_enabled: true      # 默认关闭；心跳方案不够用时可以打开
    preempt_margin_sec: 2       # 默认 2 秒；下限 1 秒，上限不超过真实时长的 20%
```

日志里 `🐕 [music/player] 主动续播: ... 抢在设备原生续播机制（如第三方内容源 CP 恢复）接管前
主动切到下一首` 就是这个机制在工作。只对能解析出真实时长的 `.mp3` 生效
（`IndexedSong.DurationMs > 0`）；其他格式（flac/wav/m4a/aac/ogg）或解析失败的文件没有这个
保护，完全依赖 `Idle` 事件本身。

如果你之前已经有一份 `cache/music_index.json`（早期版本生成的，没有 `duration_ms` 字段），
不需要手动删除或"刷新曲库"——下次启动时会自动检测到 mp3 文件缺失这个字段并补一次（只针对
size/mtime 没变但缺时长信息的文件），之后就走正常的增量刷新，不会每次都重新解析。

> ⚠️ 提前量是有代价的：会牺牲每首歌最后 `preempt_margin_sec` 秒左右的内容（通常是尾奏/
> 静音，影响不大）。如果你发现内容被切得太早（比如某些 CBR 假设误差较大的文件），可以调小
> `preempt_margin_sec`；如果 CP 续播偶尔还是抢到了控制权，可以适当调大留更多余量。
>
> **实测记录（2026-07-31）**：默认 3 秒的提前量下，连续 4 首歌都成功抢在 CP 续播之前完成
> 切歌，全程诊断快照再没出现过 `audio_meta.cp` 字段；后续把默认值调小到 2 秒同样稳定有效。
> 但更进一步的测试发现，单独把 `abort_xiaoai_heartbeat_sec` 调到 30 秒（不开主动续播、
> 不开诊断）也能稳定避免问题复现，而且没有"牺牲每首歌尾部内容"的代价，所以最终把心跳
> 设为默认的、开箱即用的防护，主动续播机制降级为可选的、更强力的备用手段。
>
> 这套机制中途一度因为体验上感知到延迟被整体移除过，但那次测试实际跑的是移除前的旧版本
> （反应式看门狗，播完后几分钟才补救），跟这版"提前几秒主动切歌"的实现完全不是一回事——
> 如果你也遇到过"看起来没用"的情况，先确认部署的是不是最新代码。

### 诊断快照（排查工具，默认关闭）

播放期间周期性打印 `ubus call mediaplayer player_get_context` 的原始输出，就是靠这个功能
抓到了上面 `cp.name=ximalaya` 的证据。日常运行不需要一直开着（默认关闭，避免刷日志）；
如果想继续排查、或想确认主动续播机制是不是还在稳定抢到控制权（正常情况下不应该再看到
`audio_meta.cp` 字段出现），可以随时打开：

```yaml
music:
  player:
    diagnostic_interval_sec: 15   # 默认关闭（0）；需要排查时可以设成 15 秒左右
```

日志里 `🔬 [music/player] player_get_context 诊断快照 (periodic): ...` 就是这个诊断在工作。

---

## 语音指令示例

| 用户说 | 动作 |
|--------|------|
| 播放许嵩 | 搜索「许嵩」，入队并播放 |
| 播放歌曲周杰伦晴天 | 去掉「歌曲」资源词，搜索「周杰伦晴天」，优先播放最高相关结果 |
| 播放西游记 | 搜索「西游记」，按集数排序从第 1 集开始 |
| 播放西游记11集 | 搜索「西游记」，从第 11 集开始播放 |
| 播放水浒传第5集 | 搜索「水浒传」，从第 5 集开始播放 |
| 默认故事是三国演义第一季 | 设置默认故事上下文，并播报当前默认故事 |
| 播放第4集 | 若已设置默认故事，则按默认故事从第 4 集开始播放 |
| 播放故事第300集 | 若已设置默认故事，则按默认故事从第 300 集开始播放 |
| 继续播放故事 / 接着听故事 | 读取播放历史，自动从上次听到的系列名+集数接着播放 |
| 下一首 / 上一首 | 切换队列中的下一首或上一首 |
| 随便听听 | 随机取 N 首歌曲播放（优先排除 `stories[].dir` 下的故事文件） |
| 单曲循环 / 全部循环 / 随机播放 | 切换播放模式 |
| 停止 / 暂停 / 闭嘴 | 清空队列并停止 |
| 刷新曲库 | 重新扫描目录并更新索引 |

### LX Sync Server 在线兜底

> `随便听听` 默认按"听歌"语义处理：若配置了 `stories[].dir`，会优先从非故事目录随机取歌，避免随机到故事集。
> 若未配置 `stories[].dir`，模块无法区分故事与歌曲，随机结果会来自整个曲库。

启用 `music.lx.enabled` 后，`播放某首歌` 会先搜索本地曲库；如果本地没有命中且不是故事/有声书集数播放，就会走在线兜底搜索/取直链。支持三种接法，看你怎么部署：

- **内嵌模式（推荐）**：`pkg/lx-go` 直接以 Go 库的形式 import 进 `pkg/music` 进程里，不用你手动另起进程/端口，搜索/取直链全是进程内函数调用。
- **独立服务模式**：单独跑一个 `pkg/lx-go` 进程，`pkg/music` 通过 HTTP（`base_url`）对接，两边可以分开部署在不同机器上。
- **第三方 LX Server**：接第三方项目 [XCQ0607/lxserver](https://github.com/XCQ0607/lxserver)，同样走 HTTP，需要账号/Token。

#### 内嵌模式（推荐）：不用手动起进程

```yaml
music:
  enabled: true
  lx:
    enabled: true
    embedded: true
    embedded_js_dir: "../lx-go/js"   # 指向音源脚本（可以多个）目录，相对/绝对路径都行
    download: false            # 普通播放不下载；说“下载歌曲”时才保存
    download_dir: ""  # 空则下载到 music.dirs[0]
    source: "wy"
    quality: "128k"
```

`pkg/music/go.mod` 用 `replace` 把 `pkg/lx-go` 当本地模块引进来，`lx_embedded.go` 里的 `EmbeddedLX` 实现了跟 `LXClient` 一样的接口（`Resolve`/`Download`），`Module.New()` 看到 `embedded: true` 就会自动用这个实现，不再走任何 HTTP/端口。这套接法已经实测跑通过：进程内直接调用，加载 6 个音源脚本、自检、搜索「周杰伦 稻香」、取直链、下载到本地文件全流程一次跑通。详见 [pkg/lx-go/README.md](../lx-go/README.md#接入-pkgmusic)。

#### 独立服务模式：单独起一个 lx-go 进程，走 HTTP

```bash
cd pkg/lx-go
go run ./cmd/lxgo-server   # 默认监听 :8080
```

```yaml
music:
  enabled: true
  lx:
    enabled: true
    base_url: "http://127.0.0.1:8080"   # pkg/lx-go 默认端口，和 music 自己的文件服务端口 18080 不冲突
    download: false            # 普通播放不下载；说“下载歌曲”时才保存
    download_dir: ""  # 空则下载到 music.dirs[0]
    source: "wy"
    quality: "128k"
    # username/password/user_token/frontend_auth 都不用填：
    # pkg/lx-go 本身不做鉴权，留空的话这边也不会去调用 /api/user/login
```

这个模式下，`pkg/music` 通过 `LXClient`（`pkg/music/lx.go`）走标准 HTTP 协议对接：

1. `GET /api/music/search?name={keyword}&source={source}&type=song&page=1&pages=1`
2. `POST /api/music/url`，请求体为 `{"songInfo": <第一条搜索结果>, "quality": "128k"}`
3. 将返回的 `url` 交给小爱设备播放

这套配置已经用未经修改的 `LXClient` 实测跑通过：搜索 → 取直链 → 代理下载到本地，全流程可用。

#### 用第三方 LX Server（需要账号/Token）

示例：

```yaml
music:
  enabled: true
  lx:
    enabled: true
    base_url: "http://localhost:9527"
    username: "your_lx_user"
    password: "your_lx_password"
    download: false            # 普通播放不下载；说“下载歌曲”时才保存
    download_dir: ""  # 空则下载到 music.dirs[0]
    source: "kw"
    quality: "128k"
```

如果已经在 LX Server 面板里生成了普通用户 API Token，优先使用：

```yaml
music:
  lx:
    enabled: true
    base_url: "http://localhost:9527"
    user_token: "lx_tk_xxx"
```

运行时会打印 LX 搜索请求、搜索返回摘要、取直链请求、取直链返回摘要以及最终远程播放 URL；日志会隐藏登录 token，也不会打印密码。

说“下载稻香”或“下载周杰伦的稻香”时，在线歌曲会通过 LX 的 `/api/music/download` 代理下载到本地，默认保存为 `歌名 - 歌手.mp3`，下载成功后自动播放。如果文件已存在会直接复用并播放本地文件，下载后会刷新曲库索引，后续本地搜索可以直接命中。普通“播放”只播放在线歌曲，不会自动下载。

#### 本地优先与远程触发规则

`pkg/music` 永远先搜本地曲库。只要本地有任何命中，就直接播放本地结果，不会触发 LX 远程搜索或下载。

例如本地还没有下载《稻香》时，可以说：

```text
播放周杰伦的稻香
```

本地找不到后，会用「周杰伦的稻香」去 LX 远程搜索并在线播放；如果希望保存，请说“下载周杰伦的稻香”。

如果已经下载过《稻香》，就需要用本地能精确命中的歌名或歌手名来播放：

```text
播放稻香
```

这时本地曲库能找到《稻香》，会直接播放本地文件，不会再访问 LX。

如果你说：

```text
播放周杰伦
```

只要本地曲库里有周杰伦的歌曲，就会播放本地搜索到的周杰伦歌曲列表，也不会触发远程下载。只有本地完全没有命中时，才会进入 LX 远程兜底。

---

## 播报文案

| 场景 | 播报内容 |
|------|----------|
| 目录未配置 | 本地音乐目录还没有配置 |
| 搜索无结果 | 没有找到包含{keyword}的歌曲 |
| 曲库为空（随机） | 曲库为空，无法随机播放 |
| 搜索命中 | 好的，找到{N}首歌曲 |
| 故事指定集数 | 好的，找到{N}集，从第{X}集开始播放 |
| 故事未指定集数 | 好的，找到{N}集 |
| 设置默认故事成功 | 好的，当前默认故事是{系列名} |
| 查询默认故事 | 当前默认故事是{系列名} |
| 查询默认故事（未设置） | 当前还没有默认故事 |
| 清除默认故事 | 好的，已清除默认故事 |
| 仅说集数但无默认故事 | 请告诉我想播放哪个故事或歌曲 |
| 继续播放故事成功 | 好的，接着播放{系列名}，从第{X}集继续 |
| 继续播放故事（无历史） | 还没有听过故事，不知道该接着播放什么 |
| 继续播放故事（文件缺失） | 没有找到{系列名}第{X}集，可能文件被移动或删除了 |
| 随机命中 | 好的，随机播放{N}首歌曲 |
| 没有下一首 | 没有下一首 |
| 没有上一首 | 已经是第一首 |
| 切换单曲循环 | 已切换到单曲循环 |
| 切换全部循环 | 已切换到全部循环 |
| 切换随机播放 | 已切换到随机播放 |
| 刷新中（已有任务） | 曲库正在刷新，请稍候 |
| 刷新开始 | 正在刷新曲库，请稍候 |
| 刷新完成 | 曲库刷新完成，共{N}首，耗时{X}秒 |
| 刷新失败 | 曲库刷新失败，请稍后重试 |

---

## 集成方式

在父模块（apps/chat）中：

```go
// 1. 在 config 结构体中增加 Music 字段
type AppConfig struct {
    // ...
    Music music.MusicConfig `yaml:"music"`
}

// 2. 启动时创建并启动音乐模块
var musicModule *music.Module

if cfg.Music.Enabled {
    musicModule = music.New(&cfg.Music)
    if err := musicModule.Start(ctx); err != nil {
        log.Fatal(err)
    }
    defer musicModule.Stop()
}

// 3. 事件处理：先调 music.OnEvent，返回 true 则跳过 AI
connect.GetHandlers().SetEventHandler(func(event connect.Event) error {
    if musicModule != nil && musicModule.OnEvent(event) {
        return nil  // 音乐模块已处理，不交给 AI
    }
    engine.OnEvent(event)
    return nil
})

// 4. 连接感知 base_url（支持 LAN + Tailscale）：传入 OnConnectionHost 回调
onConnectionHost := func(host string) {
    if musicModule != nil {
        musicModule.SetBaseURLForConnection(host)
    }
}
startServer(ctx, cfg, onConnectionHost)  // 或 startServer(ctx, engine, onConnectionHost)
```

---

## base_url 说明

- **空**：通过 UDP 探测（`net.Dial("udp", "8.8.8.8:80")`）获取本机 LAN IP，音箱通过该地址拉取音频文件
- **显式配置**：多网卡或复杂网络时，建议在配置中指定完整 URL，如 `http://192.168.1.100:18080`
- **连接感知**：集成时传入 `OnConnectionHost` 回调，会根据客户端连接方式（LAN 或 Tailscale）自动使用对应 host 拼音乐 URL，详见 [connection-aware-base-url-design](../../docs/connection-aware-base-url-design.md)

---

## playing 事件

apps/client 的 `SendEvent("playing", status)` 传入 `PlayingStatus` 字符串，故 `event.Data` 为 JSON 字符串：

- `"Playing"`：正在播放
- `"Paused"`：暂停（不触发切歌）
- `"Idle"`：空闲，若此前为播放状态且队列非空，则自动播下一首

---

## 依赖

- `github.com/dhowden/tag`：音频元数据提取
- `github.com/cxjava/open-xiaoai/apps/client`：connect 包（RPC、Event）
