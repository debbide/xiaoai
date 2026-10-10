import type { MiIOT, MiNA, MiServiceConfig } from "mi-service-lite";
import { clamp, sleep } from "../../utils/base";
import { jsonEncode } from "../../utils/parse";
import { Logger } from "../../utils/log";
import { StreamResponse } from "./stream";
import { kAreYouOK } from "../../utils/string";
import { fastRetry } from "../../utils/retry";
import { xiaomiLogin } from "../xiaomi";

export type TTSProvider = "xiaoai" | "custom";

type Speaker = {
  name?: string;
  gender?: string;
  speaker: string;
};

type ActionCommand = [number, number];
type PropertyCommand = [number, number, number];

export type BaseSpeakerConfig = MiServiceConfig & {
  /**
   * 启用调试（仅调试 MiGPT 相关日志）
   */
  debug?: boolean;
  /**
   * 追踪 Mi Service 相关日志（更底层）
   */
  enableTrace?: boolean;
  /**
   * 是否启用流式响应
   *
   * 部分小爱音箱型号不支持查询播放状态，需要关闭流式响应
   *
   * 关闭后会在 LLM 回答完毕后再 TTS 完整文本，且无法使用唤醒模式等功能
   */
  streamResponse?: boolean;
  /**
   * 语音合成服务商
   */
  tts?: TTSProvider;
  /**
   * 小爱音箱 TTS 指令
   *
   * 比如：小爱音箱 Pro（lx06） -> [5, 1]
   *
   * 具体指令可在此网站查询：https://home.miot-spec.com
   */
  ttsCommand?: ActionCommand;
  /**
   * 小爱音箱唤醒指令
   *
   * 比如：小爱音箱 Pro（lx06） -> [5, 3]
   *
   * 具体指令可在此网站查询：https://home.miot-spec.com
   */
  wakeUpCommand?: ActionCommand;
  /**
   * 查询小爱音响是否在播放中指令
   *
   * 比如：小爱音箱 Play（lx05） -> [3, 1, 1]
   *
   * 具体指令可在此网站查询：https://home.miot-spec.com
   */
  playingCommand?: PropertyCommand;
  /**
   * 播放状态检测间隔（单位毫秒，最低 500 毫秒，默认 1 秒）
   */
  checkInterval?: number;
  /**
   *   下发 TTS 指令多长时间后开始检测播放状态（单位秒，默认 3 秒）
   */
  checkTTSStatusAfter?: number;
  /**
   * TTS 开始/结束提示音
   */
  audioBeep?: string;
  /**
   * 网络请求超时时长，单位毫秒，默认值 3000 （3 秒）
   */
  timeout?: number;
};

export class BaseSpeaker {
  MiNA?: MiNA;
  MiIOT?: MiIOT;
  config: MiServiceConfig;
  logger = Logger.create({ tag: "Speaker" });
  debug = false;
  streamResponse = true;
  checkInterval: number;
  checkTTSStatusAfter: number;
  tts: TTSProvider;
  ttsCommand: ActionCommand;
  wakeUpCommand: ActionCommand;
  playingCommand?: PropertyCommand;

  constructor(config: BaseSpeakerConfig) {
    this.config = config;
    this.config.timeout = config.timeout ?? 5000;
    const {
      debug = false,
      streamResponse = true,
      checkInterval = 1000,
      checkTTSStatusAfter = 3,
      tts = "xiaoai",
      playingCommand,
      ttsCommand = [5, 1],
      wakeUpCommand = [5, 3],
      audioBeep = process.env.AUDIO_BEEP,
    } = config;
    this.debug = debug;
    this.streamResponse = streamResponse;
    this.audioBeep = audioBeep;
    this.checkInterval = clamp(checkInterval, 500, Infinity);
    this.checkTTSStatusAfter = checkTTSStatusAfter;
    this.tts = tts;
    // todo 考虑维护常见设备型号的指令列表，并自动从 spec 文件判断属性权限
    this.ttsCommand = ttsCommand;
    this.wakeUpCommand = wakeUpCommand;
    this.playingCommand = playingCommand;
  }

  /**
   * 把外部已登录好的会话注入进来（Web 页“授权登录”走的流程），
   * 避免 Speaker.run() 里重复登录刷小米接口。
   */
  setSession(miNA: MiNA, miIOT: MiIOT) {
    this.MiNA = miNA;
    this.MiIOT = miIOT;
  }

  /**
   * 初始化小米服务。
   * @returns 是否成功；失败时只打日志、绝不抛错（由 Web 页展示原因并手动重试）。
   */
  async initMiServices(): Promise<boolean> {
    if (this.MiNA && this.MiIOT) {
      this.logDeviceInfo();
      return true;
    }
    const res = await xiaomiLogin(this.config);
    if (res.ok && res.miNA && res.miIOT) {
      this.MiNA = res.miNA;
      this.MiIOT = res.miIOT;
      this.logDeviceInfo();
      return true;
    }
    return false;
  }

  private logDeviceInfo() {
    if (this.debug) {
      const d: any = this.MiIOT!.account?.device;
      this.logger.debug(
        "配置参数：",
        jsonEncode(this.config, { prettier: true })
      );
      this.logger.debug(
        "环境变量：",
        jsonEncode(process.env, { prettier: true })
      );
      this.logger.debug(
        "设备信息：",
        jsonEncode(
          {
            name: d?.name,
            desc: d?.desc,
            model: d?.model,
            rom: d?.extra?.fw_version,
          },
          { prettier: true }
        )
      );
    }
  }

  wakeUp() {
    if (this.debug) {
      this.logger.debug("wakeUp");
    }
    return this.MiIOT!.doAction(...this.wakeUpCommand);
  }

  async unWakeUp() {
    if (this.debug) {
      this.logger.debug("unWakeUp");
    }
    // 通过 TTS 不发音文本，使小爱退出唤醒状态
    await this.MiNA!.pause();
    await sleep(100);
    await this.MiIOT!.doAction(...this.ttsCommand, kAreYouOK);
    await sleep(100);
  }

  audioBeep?: string;
  responding = false;
  /**
   * 检测是否有新消息
   *
   * 有新消息产生时，旧的回复会终止
   */
  checkIfHasNewMsg() {
    return { hasNewMsg: () => false, noNewMsg: () => true };
  }
  async response(options: {
    tts?: TTSProvider;
    text?: string;
    stream?: StreamResponse;
    audio?: string;
    speaker?: string;
    keepAlive?: boolean;
    playSFX?: boolean;
    hasNewMsg?: () => boolean;
  }) {
    let {
      text,
      audio,
      stream,
      playSFX = true,
      keepAlive = false,
      tts = this.tts,
    } = options ?? {};
    options.hasNewMsg ??= this.checkIfHasNewMsg().hasNewMsg;

    if (!text && !stream && !audio) {
      return;
    }

    const customTTS = process.env.TTS_BASE_URL;
    if (!customTTS) {
      tts = "xiaoai"; // 没有提供 TTS 接口时，只能使用小爱自带 TTS
    }

    const ttsNotXiaoai = tts !== "xiaoai" && !audio;
    playSFX = this.streamResponse && ttsNotXiaoai && playSFX;

    if (ttsNotXiaoai && !stream) {
      // 长文本 TTS 转化成 stream 分段模式
      stream = StreamResponse.createStreamResponse(text!);
    }

    let res;
    this.responding = true;
    // 开始响应
    if (stream) {
      let replyText = "";
      while (true) {
        let { nextSentence, noMore } = stream.getNextResponse();
        if (!this.streamResponse) {
          nextSentence = await stream.getFinalResult();
          noMore = true;
        }
        if (nextSentence) {
          if (replyText.length < 1) {
            // 播放开始提示音
            if (playSFX && this.audioBeep) {
              if (this.debug) {
                this.logger.debug("开始播放提示音");
              }
              await this.MiNA!.play({ url: this.audioBeep });
            }
            // 在播放 TTS 语音之前，先取消小爱音箱的唤醒状态，防止将 TTS 语音识别成用户指令
            if (ttsNotXiaoai) {
              await this.unWakeUp();
            }
          }
          res = await this._response({
            ...options,
            text: nextSentence,
            playSFX: false,
            keepAlive: false,
          });
          if (res === "break") {
            // 终止回复
            stream.cancel();
            break;
          }
          replyText += nextSentence;
        }
        if (noMore) {
          if (replyText.length > 0) {
            // 播放结束提示音
            if (playSFX && this.audioBeep) {
              if (this.debug) {
                this.logger.debug("结束播放提示音");
              }
              await this.MiNA!.play({ url: this.audioBeep });
            }
          }
          // 保持唤醒状态
          if (keepAlive) {
            await this.wakeUp();
          }
          // 播放完毕
          break;
        }
        await sleep(this.checkInterval);
      }
      if (replyText.length < 1) {
        return "error";
      }
    } else {
      res = await this._response(options);
    }
    this.responding = false;
    return res;
  }

  private async _response(options: {
    tts?: TTSProvider;
    text?: string;
    audio?: string;
    speaker?: string;
    keepAlive?: boolean;
    playSFX?: boolean;
    hasNewMsg?: () => boolean;
  }) {
    let {
      text,
      audio,
      playSFX = true,
      keepAlive = false,
      tts = this.tts,
      speaker = this._currentSpeaker,
    } = options ?? {};

    const hasNewMsg = () => options.hasNewMsg?.();

    const ttsText = text?.replace(/\n\s*\n/g, "\n")?.trim();
    const ttsNotXiaoai = tts !== "xiaoai" && !audio;
    playSFX = this.streamResponse && ttsNotXiaoai && playSFX;

    // 播放回复
    const play = async (args?: { tts?: string; url?: string }) => {
      this.logger.log("🔊 " + (ttsText ?? audio));
      // 播放开始提示音
      if (playSFX && this.audioBeep) {
        if (this.debug) {
          this.logger.debug("开始播放提示音（inner）");
        }
        await this.MiNA!.play({ url: this.audioBeep });
      }
      // 在播放 TTS 语音之前，先取消小爱音箱的唤醒状态，防止将 TTS 语音识别成用户指令
      if (ttsNotXiaoai) {
        await this.unWakeUp();
      }
      if (args?.tts) {
        await this.MiIOT!.doAction(...this.ttsCommand, args.tts);
      } else {
        await this.MiNA!.play(args);
      }
      if (!this.streamResponse) {
        // 非流式响应，直接返回，不再等待设备播放完毕
        // todo 考虑后续通过 MIoT 通知事件，接收设备播放状态变更通知。
        return;
      }
      // 等待一段时间，确保本地设备状态已更新
      await sleep(this.checkTTSStatusAfter * 1000);
      // 等待回答播放完毕
      const retry = fastRetry(this, "设备状态");
      while (true) {
        // 检测设备播放状态
        let playing: any = { status: "idle" };
        let res = this.playingCommand
          ? await this.MiIOT!.getProperty(
              this.playingCommand[0],
              this.playingCommand[1]
            )
          : await this.MiNA!.getStatus();
        if (this.debug) {
          this.logger.debug(jsonEncode({ playState: res ?? "undefined" }));
        }
        if (this.playingCommand && res === this.playingCommand[2]) {
          playing = { status: "playing" };
        }
        if (!this.playingCommand) {
          playing = { ...playing, ...res };
        }
        if (
          hasNewMsg() ||
          !this.responding || // 有新消息
          (playing.status === "playing" && playing.media_type) // 小爱自己开始播放音乐
        ) {
          // 响应被中断
          return "break";
        }
        const isOk = retry.onResponse(res);
        if (isOk === "break") {
          break; // 获取设备状态异常
        }
        if (res != null && playing.status !== "playing") {
          break;
        }
        await sleep(this.checkInterval);
      }
      // 播放结束提示音
      if (playSFX && this.audioBeep) {
        if (this.debug) {
          this.logger.debug("结束播放提示音（inner）");
        }
        await this.MiNA!.play({ url: this.audioBeep });
      }
      // 保持唤醒状态
      if (keepAlive) {
        await this.wakeUp();
      }
    };

    // 开始响应
    let res;
    if (audio) {
      // 优先播放音频回复
      res = await play({ url: audio });
    } else if (ttsText) {
      // 文字回复
      switch (tts) {
        case "custom":
          const _text = encodeURIComponent(ttsText);
          const url = `${process.env.TTS_BASE_URL}/tts.mp3?speaker=${
            speaker || ""
          }&text=${_text}`;
          res = await play({ url });
          break;
        case "xiaoai":
        default:
          res = await play({ tts: ttsText });
          break;
      }
    }
    return res;
  }

  private _speakers?: Speaker[];
  private _currentSpeaker: string | undefined;
  async switchSpeaker(speaker: string) {
    if (!this._speakers && process.env.TTS_BASE_URL) {
      const resp = await fetch(`${process.env.TTS_BASE_URL}/speakers`).catch(
        () => null
      );
      const res = await resp?.json().catch(() => null);
      if (Array.isArray(res)) {
        this._speakers = res;
      }
    }
    if (!this._speakers) {
      return false;
    }
    const target = this._speakers.find(
      (e) => e.name === speaker || e.speaker === speaker
    );
    if (target) {
      this._currentSpeaker = target.speaker;
      return true;
    }
  }
}
