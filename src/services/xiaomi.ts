/**
 * 小米登录封装（自研登录流程，替代 mi-service-lite 过时的 getAccount）：
 * - 程序化走完 serviceLogin → serviceLoginAuth2 → identity/list → verifyPhone
 * - 风控时在 Web 页显示验证码输入框，不用去浏览器
 * - micoapi 的 sts 瞬时 401 自动重试一次
 * - 登录成功后直接构造 MiNA/MiIOT 实例（不再经过 mi-service-lite 的 getAccount）
 */
import { MiNA, MiIOT } from "mi-service-lite";
import { Logger } from "../utils/log";
import { Status } from "./status";
import { SessionStore } from "./session";
import {
  xiaomiAuthLogin,
  xiaomiAuthVerifyAndLogin,
  XiaomiCreds,
} from "./xiaomi-auth";

const logger = Logger.create({ tag: "XiaomiLogin" });

export interface XiaomiLoginResult {
  ok: boolean;
  miNA?: any;
  miIOT?: any;
  /** 需要短信/邮箱验证码时为 true，前端显示输入框 */
  verifyRequired?: boolean;
  /** 触发风控的 sid（xiaomiio / micoapi） */
  verifySid?: string;
  /** 浏览器验证链接（兜底：用户也可手动去浏览器） */
  verifyUrls?: Record<string, string>;
  /** 验证方式：4=手机短信，8=邮箱 */
  verifyOptions?: number[];
  error?: string;
}

/** 待完成的验证流程（跨 HTTP 请求保持） */
interface PendingVerify {
  creds: XiaomiCreds;
  sid: "xiaomiio" | "micoapi";
  verifyUrl: string;
  options: number[];
  /** 已完成的 sid → account，另一 sid 还没走 */
  done: Record<string, any>;
}

let pending: PendingVerify | null = null;

export function getPendingVerify(): PendingVerify | null {
  return pending;
}

export function clearPendingVerify() {
  pending = null;
}

async function buildClients(
  accounts: Record<string, any>,
  did?: string
): Promise<{ miNA?: any; miIOT?: any }> {
  let miNA: any;
  let miIOT: any;
  if (accounts["micoapi"]) {
    const acc = { ...accounts["micoapi"], did };
    await MiNA.getDevice(acc).catch((e: any) =>
      logger.error("MiNA.getDevice 异常", e?.message ?? e)
    );
    miNA = new MiNA(acc);
  }
  if (accounts["xiaomiio"]) {
    const acc = { ...accounts["xiaomiio"], did };
    await MiIOT.getDevice(acc).catch((e: any) =>
      logger.error("MiIOT.getDevice 异常", e?.message ?? e)
    );
    miIOT = new MiIOT(acc);
  }
  return { miNA, miIOT };
}

function toVerifyUrls(p: PendingVerify): Record<string, string> {
  return { [p.sid]: p.verifyUrl };
}

/**
 * 登录（Web“授权登录”/“测试小米登录”按钮触发）。
 * 按 sid 顺序走：先 xiaomiio，再 micoapi。任一触发风控即停下等验证码。
 */
export async function xiaomiLogin(config: {
  userId: string;
  password: string;
  did?: string;
}): Promise<XiaomiLoginResult> {
  pending = null;
  Status.xiaomi = "logging-in";
  Status.lastError = "";
  Status.verifyUrls = {};

  const sids: Array<"xiaomiio" | "micoapi"> = ["xiaomiio", "micoapi"];
  const accounts: Record<string, any> = {};

  for (const sid of sids) {
    const creds: XiaomiCreds = {
      userId: config.userId,
      password: config.password,
      sid,
    };
    logger.log(`小米登录 [${sid}] …`);
    const r = await xiaomiAuthLogin(creds);
    if (r.ok && "account" in r && r.account) {
      accounts[sid] = r.account;
      logger.success(`小米登录 [${sid}] 成功`);
      continue;
    }
    if (!r.ok && (r as any).verifyRequired) {
      const v = r as any;
      pending = {
        creds,
        sid,
        verifyUrl: v.verifyUrl,
        options: v.options || [4],
        done: accounts,
      };
      Status.xiaomi = "verify-code-required";
      Status.verifyUrls = toVerifyUrls(pending);
      const optText = (v.options || [4])
        .map((o: number) => (o === 4 ? "手机短信" : o === 8 ? "邮箱" : `方式${o}`))
        .join("/");
      Status.lastError = v.codeSent
        ? `小米账号 [${sid}] 触发安全验证，验证码已发送（${optText}），请在下方输入后提交`
        : `小米账号 [${sid}] 触发安全验证，验证码发送失败（${v.sendError || "未知错误"}），请稍后点“授权登录”重试`;
      logger.error(`小米登录 [${sid}] 触发风控，等待验证码`);
      return {
        ok: false,
        verifyRequired: true,
        verifySid: sid,
        verifyUrls: Status.verifyUrls,
        verifyOptions: v.options || [4],
        error: Status.lastError,
      };
    }
    const err = (r as any).error || "登录失败";
    Status.xiaomi = "failed";
    Status.lastError = `小米登录 [${sid}] 失败：${err}`;
    logger.error(Status.lastError);
    return { ok: false, error: Status.lastError };
  }

  // 两个 sid 都成功
  const { miNA, miIOT } = await buildClients(accounts, config.did);
  if (!miNA || !miIOT) {
    Status.xiaomi = "failed";
    Status.lastError = "小米登录异常：未能构造设备实例";
    return { ok: false, error: Status.lastError };
  }
  Status.xiaomi = "ok";
  Status.verifyUrls = {};
  Status.lastError = "";
  Status.lastLoginAt = Date.now();
  const d = miIOT?.account?.device;
  Status.device = d
    ? {
        name: d.name,
        desc: d.desc,
        model: d.model,
        did: d.did,
        fw: d?.extra?.fw_version,
      }
    : null;
  SessionStore.set({ miNA, miIOT, at: Date.now() });
  logger.success("小米登录成功（xiaomiio + micoapi）");
  return { ok: true, miNA, miIOT, verifyUrls: {} };
}

/**
 * 提交短信/邮箱验证码，继续之前中断的登录流程。
 */
export async function xiaomiSubmitVerifyCode(
  ticket: string,
  config: { userId: string; password: string; did?: string }
): Promise<XiaomiLoginResult> {
  const p = pending;
  if (!p) {
    return { ok: false, error: "没有待验证的登录流程，请先点“授权登录”" };
  }
  if (!ticket || !ticket.trim()) {
    return { ok: false, verifyRequired: true, verifySid: p.sid, verifyUrls: toVerifyUrls(p), verifyOptions: p.options, error: "请输入验证码" };
  }
  Status.xiaomi = "logging-in";
  Status.lastError = "";
  logger.log(`提交验证码 [${p.sid}] …`);

  const r = await xiaomiAuthVerifyAndLogin(p.creds, ticket.trim(), p.verifyUrl);
  if (!r.ok || !("account" in r) || !r.account) {
    Status.xiaomi = "verify-code-required";
    Status.lastError = `验证码提交失败 [${p.sid}]：${(r as any).error || "未知错误"}，可重新输入`;
    logger.error(Status.lastError);
    return {
      ok: false,
      verifyRequired: true,
      verifySid: p.sid,
      verifyUrls: toVerifyUrls(p),
      verifyOptions: p.options,
      error: Status.lastError,
    };
  }

  // 当前 sid 完成，继续下一个 sid
  const accounts: Record<string, any> = { ...p.done, [p.sid]: (r as any).account };
  logger.success(`小米验证通过 [${p.sid}]`);
  pending = null;

  const remaining: Array<"xiaomiio" | "micoapi"> = (["xiaomiio", "micoapi"] as Array<
    "xiaomiio" | "micoapi"
  >).filter((s) => !accounts[s]);

  for (const sid of remaining) {
    const creds: XiaomiCreds = {
      userId: config.userId,
      password: config.password,
      sid,
    };
    logger.log(`小米登录 [${sid}] …`);
    const r2 = await xiaomiAuthLogin(creds);
    if (r2.ok && "account" in r2 && (r2 as any).account) {
      accounts[sid] = (r2 as any).account;
      logger.success(`小米登录 [${sid}] 成功`);
      continue;
    }
    if (!r2.ok && (r2 as any).verifyRequired) {
      const v = r2 as any;
      pending = { creds, sid, verifyUrl: v.verifyUrl, options: v.options || [4], done: accounts };
      Status.xiaomi = "verify-code-required";
      Status.verifyUrls = toVerifyUrls(pending);
      Status.lastError = (v as any).codeSent
        ? `小米账号 [${sid}] 触发安全验证，验证码已发送，请输入后提交`
        : `小米账号 [${sid}] 触发安全验证，验证码发送失败（${(v as any).sendError || "未知错误"}），请稍后重试`;
      return {
        ok: false,
        verifyRequired: true,
        verifySid: sid,
        verifyUrls: Status.verifyUrls,
        verifyOptions: v.options || [4],
        error: Status.lastError,
      };
    }
    const err = (r2 as any).error || "登录失败";
    Status.xiaomi = "failed";
    Status.lastError = `小米登录 [${sid}] 失败：${err}`;
    return { ok: false, error: Status.lastError };
  }

  const { miNA, miIOT } = await buildClients(accounts, config.did);
  if (!miNA || !miIOT) {
    Status.xiaomi = "failed";
    Status.lastError = "小米登录异常：未能构造设备实例";
    return { ok: false, error: Status.lastError };
  }
  Status.xiaomi = "ok";
  Status.verifyUrls = {};
  Status.lastError = "";
  Status.lastLoginAt = Date.now();
  const d = miIOT?.account?.device;
  Status.device = d
    ? {
        name: d.name,
        desc: d.desc,
        model: d.model,
        did: d.did,
        fw: d?.extra?.fw_version,
      }
    : null;
  SessionStore.set({ miNA, miIOT, at: Date.now() });
  logger.success("小米登录成功（xiaomiio + micoapi）");
  return { ok: true, miNA, miIOT, verifyUrls: {} };
}
