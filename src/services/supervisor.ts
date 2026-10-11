/**
* 服务督导：管理 MiGPT 客户端生命周期。
* - 容器启动时自动登录、自动启动服务，只跑 Web。
* - 用户在 Web 页点“授权登录”后，才创建客户端并以后台方式启动消息循环。
*/
import type { MiGPT} from "../index";
import { Logger} from "../utils/log";
import { Status} from "./status";
import { SessionStore} from "./session";
import { ForkConfig} from "./config";
import { xiaomiLogin} from "./xiaomi";

const logger = Logger.create({ tag: "Supervisor"});

// 由 boot.ts 注入，避免循环引用
let MiGPTClass: any = null;
export function setMiGPTClass(c: any) {
MiGPTClass = c;
}

let client: MiGPT | null = null;

export function getClient(): MiGPT | null {
return client;
}

export function toMiGPTConfig(cfg: ForkConfig): any {
return {
speaker: {
userId: cfg.xiaomi.userId,
password: cfg.xiaomi.password,
did: cfg.xiaomi.did || undefined,
passToken: cfg.xiaomi.passToken || undefined,
tts: cfg.speaker.tts,
ttsCommand: cfg.speaker.ttsCommand,
wakeUpCommand: cfg.speaker.wakeUpCommand,
streamResponse: cfg.speaker.streamResponse,
callAIKeywords: cfg.speaker.callAIKeywords,
wakeUpKeywords: cfg.speaker.wakeUpKeywords,
exitKeywords: cfg.speaker.exitKeywords,
name: cfg.speaker.name,
},
};
}

export interface LoginAndStartResult {
ok: boolean;
stage: string;
verifyUrls?: Record<string, string>;
verifySid?: string;
verifyOptions?: number[];
error?: string;
}

/**
* 用已登录的会话直接启动服务（验证码流程走完后调用，避免重复登录刷接口）。
*/
export async function startWithSession(
cfg: ForkConfig,
miNA: any,
miIOT: any
): Promise<LoginAndStartResult> {
if (Status.service === "running" || Status.service === "starting") {
return { ok: true, stage: "already-running" };
}
if (!MiGPTClass) {
return { ok: false, stage: "not-ready", error: "服务类未初始化" };
}
Status.service = "starting";
try {
const c = MiGPTClass.create(toMiGPTConfig(cfg));
(c as any).speaker.setSession(miNA, miIOT);
client = c as MiGPT;
c.start()
.then(() => {
if (Status.service !== "stopped") {
Status.service = "stopped";
logger.error("语音服务消息循环已退出");
}
})
.catch((e) => {
Status.service = "stopped";
Status.lastError = "服务启动异常: " + (e?.message ?? e);
logger.error("服务启动异常", e);
});
SessionStore.set({ miNA, miIOT, at: Date.now() });
logger.success("语音服务启动中（复用已验证的会话）");
return { ok: true, stage: "service-started" };
} catch (e: any) {
Status.service = "stopped";
const msg = "服务启动异常: " + (e?.message ?? e);
Status.lastError = msg;
return { ok: false, stage: "exception", error: msg };
}
}

/**
* 授权登录并启动服务（Web 按钮触发）：
* 1) 先走一次登录（风控时返回 verifyRequired，前端显示验证码输入框，不崩进程）
* 2) 成功则把会话注入 Speaker，后台启动消息循环
*/
export async function loginAndStart(cfg: ForkConfig): Promise<LoginAndStartResult> {
if (Status.service === "running" || Status.service === "starting") {
return { ok: true, stage: "already-running"};
}
if (!MiGPTClass) {
return { ok: false, stage: "not-ready", error: "服务类未初始化"};
}
Status.service = "starting";
try {
const login = await xiaomiLogin({
userId: cfg.xiaomi.userId,
password: cfg.xiaomi.password,
did: cfg.xiaomi.did || undefined,
passToken: cfg.xiaomi.passToken || undefined,
});
if (!login.ok) {
Status.service = "stopped";
if ((login as any).verifyRequired) {
return {
ok: false,
stage: "verify-code-required",
verifySid: (login as any).verifySid,
verifyUrls: login.verifyUrls,
verifyOptions: (login as any).verifyOptions,
error: login.error,
};
}
return {
ok: false,
stage: "login-failed",
verifyUrls: login.verifyUrls,
error: login.error,
};
}
const c = MiGPTClass.create(toMiGPTConfig(cfg));
// 把已登录的会话直接注入，避免 Speaker.run() 里重复登录刷小米接口
(c as any).speaker.setSession(login.miNA, login.miIOT);
client = c as MiGPT;
// 后台启动，不阻塞 Web 请求；speaker.run() 内会自己上报 running/stopped
c.start()
.then(() => {
if (Status.service!== "stopped") {
Status.service = "stopped";
logger.error("语音服务消息循环已退出");
}
})
.catch((e) => {
Status.service = "stopped";
Status.lastError = "服务启动异常: " + (e?.message?? e);
logger.error("服务启动异常", e);
});
logger.success("语音服务启动中，可在状态页查看进度");
return { ok: true, stage: "service-started"};
} catch (e: any) {
Status.service = "stopped";
const msg = "授权登录异常: " + (e?.message?? e);
Status.lastError = msg;
logger.error(msg);
return { ok: false, stage: "exception", error: msg};
}
}
