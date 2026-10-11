import { clamp, firstOf, lastOf, sleep } from "../../utils/base";
import { fastRetry } from "../../utils/retry";
import { kAreYouOK } from "../../utils/string";
import { BaseSpeaker, BaseSpeakerConfig } from "./base";
import { StreamResponse } from "./stream";
import { Status } from "../status";

export interface QueryMessage {
  text: string;
  answer?: string;
  /**
   * 毫秒
   */
  timestamp: number;
}

export interface SpeakerAnswer {
  text?: string;
  url?: string;
  stream?: StreamResponse;
}

export interface SpeakerCommand {
  match: (msg: QueryMessage) => boolean;
  /**
   * 命中后执行的操作，返回值非空时会自动回复给用户
   */
  run: (msg: QueryMessage) => Promise<SpeakerAnswer | undefined | void>;
  /**
   * 命中该指令时，若小爱正在播放音乐/媒体，是否也把它暂停掉。
   *
   * - 未设置 / true（默认）：无条件暂停。用于唤醒、退出、换音色等
   *   MiGPT 自己的指令 —— 目的是打断小爱正在播报的 TTS，避免两个声音重叠。
   * - false：检测到正在播放音乐时跳过暂停，避免一句闲聊就把歌掐断。
   *   小爱自己的 TTS 播报不受影响，仍会被打断。
   *
   * 与 songloft / xiaomusic 等外部音乐服务共存时，建议给「聊天」指令设为 false。
   */
  pauseOnMusic?: boolean;
}

export type SpeakerConfig = BaseSpeakerConfig & {
  /**
   * 拉取消息心跳间隔（单位毫秒，最低 500 毫秒，默认 1 秒）
   */
  heartbeat?: number;
  /**
   * 自定义的消息指令
   */
  commands?: SpeakerCommand[];
  /**
   * 无响应一段时间后，多久自动退出唤醒模式（单位秒，默认30秒）
   */
  exitKeepAliveAfter?: number;
  /**
   * 静音音频链接
   */
  audioSilent?: string;
};

export class Speaker extends BaseSpeaker {
  heartbeat: number;
  exitKeepAliveAfter: number;
  currentQueryMsg?: QueryMessage;

  constructor(config: SpeakerConfig) {
    super(config);
    const {
      heartbeat = 1000,
      exitKeepAliveAfter = 30,
      audioSilent = process.env.AUDIO_SILENT,
    } = config;
    this.audioSilent = audioSilent;
    this._commands = config.commands ?? [];
    this.heartbeat = clamp(heartbeat, 500, Infinity);
    this.exitKeepAliveAfter = exitKeepAliveAfter;
  }

  status: "running" | "stopped" = "running";

  stop() {
    this.status = "stopped";
  }

  async run() {
    const ok = await this.initMiServices();
    if (!ok || !this.MiNA) {
      // 登录失败：只打日志，不抛错、不退出进程。
      // 原因（验证链接等）已写入 Status，Web 管理页展示并支持手动重试。
      this.logger.error("小米登录失败，语音服务未启动。请打开 Web 管理页查看原因。");
      Status.service = "stopped";
      return;
    }
    Status.service = "running";
    this.logger.success("服务已启动...");
    this.activeKeepAliveMode();
    const retry = fastRetry(this, "消息列表");
    while (this.status === "running") {
      const nextMsg = await this.fetchNextMessage();
      const isOk = retry.onResponse(this._lastConversation);
      if (isOk === "break") {
        process.exit(1); // 退出应用
      }
      if (nextMsg) {
        this.responding = false;
        this.logger.log("🔥 " + nextMsg.text);
        // 异步处理消息，不阻塞正常消息拉取
        this.onMessage(nextMsg);
      }
      await sleep(this.heartbeat);
    }
  }

  audioSilent?: string;
  async activeKeepAliveMode() {
    while (this.status === "running") {
      if (this.keepAlive) {
        // 唤醒中
        if (!this.responding) {
          // 没有回复时，一直播放静音音频使小爱闭嘴
          if (this.audioSilent) {
            await this.MiNA?.play({ url: this.audioSilent });
          } else {
            await this.MiIOT!.doAction(...this.ttsCommand, kAreYouOK);
          }
        }
      }
      await sleep(this.checkInterval);
    }
  }

  _commands: SpeakerCommand[] = [];
  get commands() {
    return this._commands;
  }

  addCommand(command: SpeakerCommand) {
    this._commands.push(command);
  }

  async onMessage(msg: QueryMessage) {
    const { noNewMsg } = this.checkIfHasNewMsg(msg);
    for (const command of this.commands) {
      if (command.match(msg)) {
        // 关闭小爱的回复（放歌共存场景下不打断音乐）
        await this.pauseXiaoai(command);
        // 执行命令
        const answer = await command.run(msg);
        // 回复用户
        if (answer) {
          if (noNewMsg() && this.status === "running") {
            await this.response({
              ...answer,
              keepAlive: this.keepAlive,
            });
          }
        }
        await this.exitKeepAliveIfNeeded();
        return;
      }
    }
  }

  /**
   * 命中指令后暂停小爱当前播放。
   *
   * 默认无条件暂停（打断小爱的 TTS 回复，避免两个声音重叠）。
   * 指令声明 `pauseOnMusic: false` 时，只在「当前没在播放音乐」时才暂停，
   * 这样小爱/外部音乐插件正在放歌时，一句闲聊不会把歌掐断。
   */
  private async pauseXiaoai(command: SpeakerCommand) {
    if (command.pauseOnMusic === false && (await this.isPlayingMusic())) {
      if (this.debug) {
        this.logger.debug("正在播放音乐，跳过 pause");
      }
      return;
    }
    await this.MiNA!.pause();
  }

  /**
   * 当前是否正在播放音乐/媒体（区别于小爱自己的 TTS 播报）。
   *
   * 判据与 BaseSpeaker 里「小爱自己开始播放音乐」的判定保持一致：
   * `status === "playing"` 且 `media_type` 非空 —— 小爱念 TTS 时 media_type 为空。
   * 设备不支持查询播放状态时返回 false，即退化为「照旧暂停」。
   */
  private async isPlayingMusic(): Promise<boolean> {
    try {
      const status = await this.MiNA?.getStatus();
      return !!status && status.status === "playing" && !!status.media_type;
    } catch {
      return false;
    }
  }

  /**
   * 是否保持设备响应状态
   */
  keepAlive = false;

  async enterKeepAlive() {
    // 唤醒
    this.keepAlive = true;
  }

  async exitKeepAlive() {
    // 退出唤醒状态
    this.keepAlive = false;
  }

  private _preTimer: any;
  async exitKeepAliveIfNeeded() {
    // 无响应一段时间后自动退出唤醒状态
    if (this._preTimer) {
      clearTimeout(this._preTimer);
    }
    const { noNewMsg } = this.checkIfHasNewMsg();
    this._preTimer = setTimeout(async () => {
      if (
        this.keepAlive &&
        !this.responding &&
        noNewMsg() &&
        this.status === "running"
      ) {
        await this.exitKeepAlive();
      }
    }, this.exitKeepAliveAfter * 1000);
  }

  checkIfHasNewMsg(currentMsg?: QueryMessage) {
    const currentTimestamp = (currentMsg ?? this.currentQueryMsg)?.timestamp;
    return {
      hasNewMsg: () => currentTimestamp !== this.currentQueryMsg?.timestamp,
      noNewMsg: () => currentTimestamp === this.currentQueryMsg?.timestamp,
    };
  }

  private _tempMsgs: QueryMessage[] = [];
  async fetchNextMessage(): Promise<QueryMessage | undefined> {
    if (!this.currentQueryMsg) {
      await this._fetchFirstMessage();
      // 第一条消息仅用作初始化消息游标，不响应
      return;
    }
    return this._fetchNextMessage();
  }

  private async _fetchFirstMessage() {
    const msgs = await this.getMessages({
      limit: 1,
      filterAnswer: false,
    });
    this.currentQueryMsg = msgs[0];
  }

  private async _fetchNextMessage(): Promise<QueryMessage | undefined> {
    if (this._tempMsgs.length > 0) {
      // 当前有暂存的新消息（从新到旧），依次处理之
      return this._fetchNextTempMessage();
    }
    // 拉取最新的 2 条 msg（用于和上一条消息比对是否连续）
    const nextMsg = await this._fetchNext2Messages();
    if (nextMsg !== "continue") {
      return nextMsg;
    }
    // 继续向上拉取其他新消息
    return this._fetchNextRemainingMessages();
  }

  private async _fetchNext2Messages() {
    // 拉取最新的 2 条 msg（用于和上一条消息比对是否连续）
    let msgs = await this.getMessages({ limit: 2 });
    if (
      msgs.length < 1 ||
      firstOf(msgs)!.timestamp <= this.currentQueryMsg!.timestamp
    ) {
      // 没有拉到新消息
      return;
    }
    if (
      firstOf(msgs)!.timestamp > this.currentQueryMsg!.timestamp &&
      (msgs.length === 1 ||
        lastOf(msgs)!.timestamp <= this.currentQueryMsg!.timestamp)
    ) {
      // 刚好收到一条新消息
      this.currentQueryMsg = firstOf(msgs);
      return this.currentQueryMsg;
    }
    // 还有其他新消息，暂存当前的新消息
    for (const msg of msgs) {
      if (msg.timestamp > this.currentQueryMsg!.timestamp) {
        this._tempMsgs.push(msg);
      }
    }
    return "continue";
  }

  private _fetchNextTempMessage() {
    const nextMsg = this._tempMsgs.pop();
    this.currentQueryMsg = nextMsg;
    return nextMsg;
  }

  private async _fetchNextRemainingMessages(maxPage = 3) {
    // 继续向上拉取其他新消息
    let currentPage = 0;
    while (true) {
      currentPage++;
      if (currentPage > maxPage) {
        // 拉取新消息超长，取消拉取
        return this._fetchNextTempMessage();
      }
      const nextTimestamp = lastOf(this._tempMsgs)!.timestamp;
      const msgs = await this.getMessages({
        limit: 10,
        timestamp: nextTimestamp,
      });
      for (const msg of msgs) {
        if (msg.timestamp >= nextTimestamp) {
          // 忽略上一页的消息
          continue;
        } else if (msg.timestamp > this.currentQueryMsg!.timestamp) {
          // 继续添加新消息
          this._tempMsgs.push(msg);
        } else {
          // 拉取到历史消息处
          return this._fetchNextTempMessage();
        }
      }
    }
  }

  private _lastConversation: any;
  async getMessages(options?: {
    limit?: number;
    timestamp?: number;
    filterAnswer?: boolean;
  }): Promise<QueryMessage[]> {
    const filterAnswer = options?.filterAnswer ?? true;
    const conversation = await this.MiNA!.getConversations(options);
    this._lastConversation = conversation;
    let records = conversation?.records ?? [];
    if (filterAnswer) {
      // 过滤有小爱回答的消息
      records = records.filter(
        (e) =>
          ["TTS", "LLM"].includes(e.answers[0]?.type) && // 过滤 TTS 和 LLM 消息
          e.answers.length === 1 // 播放音乐时会有 TTS、Audio 两个 Answer
      );
    }
    return records.map((e) => {
      const msg: any = e.answers[0];
      const answer = msg?.tts?.text?.trim() ?? msg?.llm?.text?.trim();
      return {
        answer,
        text: e.query,
        timestamp: e.time,
      };
    });
  }
}
