/**
 * 小米登录（自研实现，替代 mi-service-lite 过时的 getAccount）。
 * 移植自 HomeAssistant hass-xiaomi-miot 的 xiaomi_cloud.py（持续维护，几十万用户验证）。
 *
 * 关键修复（mi-service-lite 缺失）：
 * 1. 风控时走 identity/list 获取验证方式，并持久化 identity_session cookie
 *    （对应 hass-xiaomi-miot PR #2880，没有这个 verify_ticket 必失败）
 * 2. 程序化提交短信/邮箱验证码（verifyPhone / verifyEmail），不用去浏览器
 * 3. micoapi 的 sts 接口验证后可能瞬时 401，自动重试一次
 *
 * 对齐 hass-xiaomi-miot master（PR #2948 / #2950）的修复：
 * 4. serviceLogin 在“已带有效会话”时（刚通过短信/邮箱验证、或本地有 passToken）
 *    会直接返回 code=0 + location，且**不再返回 _sign**。此时必须跳过
 *    serviceLoginAuth2，直接从该响应取 ssecurity/nonce/passToken；
 *    否则 ssecurity 缺失 → 后续 api.io.mi.com 请求全部 “invalid signature”。
 *    （上游原话：verify 分支若提前 return，会跳过唯一取 ssecurity 的 step1）
 * 5. 消费 verify 返回的 location 时要跟随 3xx 跳转（HA allow_redirects=True），
 *    才能拿到验证结果页下发的 passToken 等 cookie。
 * 6. 解析 &&&START&&& JSON 时保留大整数精度（nonce/userId 是 19 位整数），
 *    否则 micoapi 的 clientSign 会算错（对齐 mi-service-lite parseAuthPass）。
 *
 * 流程：
 *   step1: GET /pass/serviceLogin?sid=xxx
 *          → 未登录：拿 _sign/qs/callback 继续 step2
 *          → 已登录：直接拿 location + ssecurity，跳过 step2
 *   step2: POST /pass/serviceLoginAuth2 提交账号密码
 *          → 成功返回 location；风控返回 notificationUrl
 *   step3: GET {notificationUrl → identity/list} 拿 identity_session + 验证方式
 *   step3b: POST /identity/auth/sendPhoneTicket|sendEmailTicket 下发短信/邮件验证码
 *          （identity/list 不会触发下发，必须显式调用；参考
 *           PiotrMachowski/Xiaomi-cloud-tokens-extractor 的 do_2fa_email_flow）
 *   step4: POST /identity/auth/verifyPhone|verifyEmail 提交用户输入的验证码
 *          → 成功后重走 step1→step2→step5
 *   step5: GET location 拿 serviceToken（micoapi 的 sts 401 则重试一次）
 */
import crypto from "crypto";
import { Logger } from "../utils/log";
import { readJSONSync, writeJSON, exists } from "../utils/io";
import { configPath } from "./config";
import path from "path";

const logger = Logger.create({ tag: "XiaomiAuth" });

const ACCOUNT_BASE = "https://account.xiaomi.com";
const UA_TPL =
  "Android-7.1.1-1.0.0-ONEPLUS A3010-136-%s APP/xiaomi.smarthome APPV/62830";

/* ---------------- CookieJar ---------------- */

class CookieJar {
  private map = new Map<string, string>();

  setFromHeaders(headers: Headers) {
    // undici 的 Headers.getSetCookie() 返回 string[]
    const getSetCookie = (headers as any).getSetCookie as
      | (() => string[])
      | undefined;
    const rawList: string[] = getSetCookie
      ? getSetCookie.call(headers)
      : [];
    // 兜底：单个 set-cookie 头
    if (rawList.length === 0) {
      const single = headers.get("set-cookie");
      if (single) rawList.push(single);
    }
    for (const line of rawList) {
      const pair = line.split(";")[0];
      const eq = pair.indexOf("=");
      if (eq > 0) {
        const k = pair.slice(0, eq).trim();
        const v = pair.slice(eq + 1).trim();
        if (k) this.map.set(k, v);
      }
    }
  }

  set(k: string, v: string) {
    this.map.set(k, v);
  }

  get(k: string): string | undefined {
    return this.map.get(k);
  }

  header(): string {
    const parts: string[] = [];
    this.map.forEach((v, k) => parts.push(`${k}=${v}`));
    return parts.join("; ");
  }

  toJSON(): Record<string, string> {
    const o: Record<string, string> = {};
    this.map.forEach((v, k) => (o[k] = v));
    return o;
  }

  load(o: Record<string, string>) {
    for (const k of Object.keys(o || {})) this.map.set(k, o[k]);
  }
}

function cookiesPath(): string {
  return path.join(path.dirname(configPath()), "cookies.json");
}

/**
 * cookies.json 里持久化的单个 sid 登录会话（对齐 mi-service-lite 的 .mi.json）。
 * 见 loadSession() 的注释说明为什么必须有它。
 */
interface PersistedSession {
  deviceId?: string;
  userId?: string;
  cUserId?: string;
  passToken?: string;
  ssecurity?: string;
  nonce?: string;
  serviceToken?: string;
  updatedAt?: number;
}

function readState(): any {
  try {
    const p = cookiesPath();
    return exists(p) ? (readJSONSync(p) as any) || {} : {};
  } catch {
    return {};
  }
}

function patchState(patch: Record<string, any>) {
  try {
    writeJSON(cookiesPath(), { ...readState(), ...patch, updatedAt: Date.now() });
  } catch (e) {
    logger.error("保存登录状态失败", e);
  }
}

export function saveIdentitySession(identitySession: string) {
  patchState({ identity_session: identitySession });
}

export function loadIdentitySession(): string | undefined {
  return readState()?.identity_session;
}

/**
 * 读取某个 sid 上次登录留下的会话。
 *
 * 为什么必须持久化（这是「上个版本不用输验证码、这个版本却要」的根因）：
 * mi-service-lite 的原实现 getMiService() 会把整个 account 写到 .mi.json，
 * 下次启动时复用其中的 deviceId 和 pass.passToken，作为 cookie 发给
 * serviceLogin（见其 _getLoginCookies）。服务端据此认出「这台可信设备」，
 * 直接返回 code=0 + location，跳过 serviceLoginAuth2，也**不会**下发验证码。
 *
 * 本 fork 重写登录流程时丢掉了这层持久化：
 *   - deviceId 每次都是 randomDeviceId()，从未落盘；
 *   - 每次都是 new CookieJar()，不带任何历史 cookie；
 * → 小米每次都当成陌生设备登录 → 每次都要求验证码 → 反复调用
 *   sendPhoneTicket → 撞上 code=70022「验证码发送过多，请明天再试」。
 *
 * 修复：把 deviceId / passToken / userId / cUserId 落盘并在下次登录时复用。
 * 注意：全新设备第一次仍需验证一次，验证通过后 passToken 落盘，之后即可免验证码。
 */
export function loadSession(sid: string): PersistedSession | undefined {
  migrateLegacyMiJson();
  return readState()?.sessions?.[sid];
}

let migratedLegacy = false;

/**
 * 一次性把 mi-service-lite 遗留的 .mi.json 会话迁移进来。
 * 老版本（用 mi-service-lite getMiNA/getMiIOT 的那版）会把可信设备写在那里，
 * 迁移过来后可能直接免验证码登录，省掉重新验证。
 */
function migrateLegacyMiJson() {
  if (migratedLegacy) return;
  migratedLegacy = true;
  const cands = [
    path.join(process.cwd(), ".mi.json"),
    path.join(path.dirname(configPath()), ".mi.json"),
  ];
  const map: Record<string, string> = { miiot: "xiaomiio", mina: "micoapi" };
  for (const p of cands) {
    try {
      if (!exists(p)) continue;
      const raw = readJSONSync(p) as any;
      if (!raw || typeof raw !== "object") continue;
      let n = 0;
      for (const key of Object.keys(map)) {
        const a = raw[key];
        if (!a) continue;
        const sid = map[key];
        if (loadSession(sid)?.passToken) continue; // 已有更新会话，不覆盖
        saveSession(sid, {
          deviceId: a.deviceId,
          userId: a.userId != null ? String(a.userId) : undefined,
          cUserId: a.pass?.cUserId != null ? String(a.pass.cUserId) : undefined,
          passToken: a.pass?.passToken,
          ssecurity: a.pass?.ssecurity,
          nonce: a.pass?.nonce,
          serviceToken: a.serviceToken,
        });
        n++;
      }
      if (n > 0) logger.log(`已从遗留文件迁移 ${n} 个登录会话: ${p}`);
    } catch (e) {
      logger.error("迁移 .mi.json 失败", e);
    }
  }
}

export function saveSession(sid: string, s: PersistedSession) {
  const cur = readState();
  patchState({
    sessions: {
      ...(cur?.sessions || {}),
      [sid]: { ...s, updatedAt: Date.now() },
    },
  });
}

/** 把上次登录的会话 cookie 灌进新的 cookie jar，让 serviceLogin 直接认出「已登录设备」 */
function seedSessionCookies(jar: CookieJar, s?: PersistedSession) {
  if (!s) return;
  if (s.userId) jar.set("userId", s.userId);
  if (s.deviceId) jar.set("deviceId", s.deviceId);
  if (s.cUserId) jar.set("cUserId", s.cUserId);
  if (s.passToken) jar.set("passToken", s.passToken);
  const is = loadIdentitySession();
  if (is) jar.set("identity_session", is);
}

/* ---------------- 工具 ---------------- */

function md5upper(s: string): string {
  return crypto.createHash("md5").update(s, "utf8").digest("hex").toUpperCase();
}

function sha1b64(s: string): string {
  return crypto.createHash("sha1").update(s, "utf8").digest("base64");
}

function parseAuthJson(text: string): any {
  // 小米返回 &&&START&&&{...} 格式。
  // 注意：nonce / userId 等字段是 19 位大整数，JSON.parse 会丢精度，
  // 导致 micoapi 的 clientSign 算错、cookie 里的 userId 被四舍五入 →
  // 后续请求 “invalid signature”。先把 9 位以上的数字转成字符串再解析
  // （与 mi-service-lite 的 parseAuthPass 一致）。
  const t = text
    .replace("&&&START&&&", "")
    .replace(/:(\d{9,})/g, ':"$1"')
    .trim();
  try {
    return JSON.parse(t);
  } catch {
    return { _raw: text };
  }
}

function randomDeviceId(): string {
  return (
    "android_" +
    crypto.randomBytes(8).toString("hex")
  );
}

/** micoapi 需要在 location 上追加 clientSign（sha1(`nonce=..&ssecurity`) 的 base64） */
function withClientSign(location: string, sid: string, auth: any): string {
  if (sid !== "xiaomiio" && auth?.nonce && auth?.ssecurity) {
    const sign = sha1b64(`nonce=${auth.nonce}&${auth.ssecurity}`);
    return location + "&clientSign=" + encodeURIComponent(sign);
  }
  return location;
}

async function httpGet(
  url: string,
  jar: CookieJar,
  opts: { params?: Record<string, string>; headers?: Record<string, string> } = {}
): Promise<{ status: number; text: string; headers: Headers; json: any }> {
  const u = new URL(url.startsWith("http") ? url : ACCOUNT_BASE + url);
  if (opts.params) {
    for (const k of Object.keys(opts.params)) u.searchParams.set(k, opts.params[k]);
  }
  const headers: Record<string, string> = { ...(opts.headers || {}) };
  const ck = jar.header();
  if (ck) headers["Cookie"] = ck;
  const resp = await fetch(u.toString(), {
    method: "GET",
    headers,
    redirect: "manual",
  });
  jar.setFromHeaders(resp.headers);
  const text = await resp.text();
  return { status: resp.status, text, headers: resp.headers, json: parseAuthJson(text) };
}

async function httpPost(
  url: string,
  jar: CookieJar,
  opts: {
    params?: Record<string, string>;
    form?: Record<string, string>;
    headers?: Record<string, string>;
    cookies?: Record<string, string>;
  } = {}
): Promise<{ status: number; text: string; headers: Headers; json: any }> {
  const u = new URL(url.startsWith("http") ? url : ACCOUNT_BASE + url);
  if (opts.params) {
    for (const k of Object.keys(opts.params)) u.searchParams.set(k, opts.params[k]);
  }
  const headers: Record<string, string> = {
    "Content-Type": "application/x-www-form-urlencoded",
    ...(opts.headers || {}),
  };
  const extra = new CookieJar();
  if (opts.cookies) {
    for (const k of Object.keys(opts.cookies)) extra.set(k, opts.cookies[k]);
  }
  const ck = jar.header();
  const ck2 = extra.header();
  const combined = [ck, ck2].filter(Boolean).join("; ");
  if (combined) headers["Cookie"] = combined;
  const body = new URLSearchParams(opts.form || {}).toString();
  const resp = await fetch(u.toString(), {
    method: "POST",
    headers,
    body,
    redirect: "manual",
  });
  jar.setFromHeaders(resp.headers);
  const text = await resp.text();
  return { status: resp.status, text, headers: resp.headers, json: parseAuthJson(text) };
}

/* ---------------- 登录流程 ---------------- */

export interface AuthStep12Result {
  ok: boolean;
  location?: string;
  nonce?: string;
  ssecurity?: string;
  passToken?: string;
  userId?: string;
  cUserId?: string;
  notificationUrl?: string;
  captchaUrl?: string;
  step1?: any;
  error?: string;
}

export interface XiaomiCreds {
  userId: string; // 用户名（可以是小米ID/手机/邮箱，hass 用 username）
  password: string;
  sid: "xiaomiio" | "micoapi";
  deviceId?: string;
}

function userAgent(deviceId: string): string {
  return UA_TPL.replace("%s", deviceId);
}

/** step1: 拿 _sign/qs/callback */
async function step1(
  creds: XiaomiCreds,
  deviceId: string,
  jar: CookieJar
): Promise<any> {
  jar.set("sdkVersion", "3.8.6");
  jar.set("deviceId", deviceId);
  const r = await httpGet("/pass/serviceLogin", jar, {
    params: { sid: creds.sid, _json: "true" },
    headers: { "User-Agent": userAgent(deviceId) },
  });
  return r.json;
}

/** step2: 提交账号密码 */
async function step2(
  creds: XiaomiCreds,
  deviceId: string,
  jar: CookieJar,
  s1: any
): Promise<AuthStep12Result> {
  const form: Record<string, string> = {
    user: creds.userId,
    hash: md5upper(creds.password),
    callback: s1.callback || "",
    sid: s1.sid || creds.sid,
    qs: s1.qs || "",
    _sign: s1._sign || "",
  };
  const r = await httpPost("/pass/serviceLoginAuth2", jar, {
    params: { _json: "true", _dc: String(Date.now()) },
    form,
    headers: { "User-Agent": userAgent(deviceId) },
  });
  const auth = r.json;
  if (auth.location) {
    return {
      ok: true,
      location: withClientSign(auth.location, creds.sid, auth),
      nonce: auth.nonce,
      ssecurity: auth.ssecurity,
      passToken: auth.passToken,
      userId: String(auth.userId || ""),
      cUserId: auth.cUserId ? String(auth.cUserId) : undefined,
      step1: s1,
    };
  }
  if (auth.notificationUrl) {
    return { ok: false, notificationUrl: auth.notificationUrl, step1: s1 };
  }
  if (auth.captchaUrl) {
    return { ok: false, captchaUrl: auth.captchaUrl, step1: s1, error: "需要图形验证码（暂不支持，请去网页完成一次登录）" };
  }
  const code = auth.code;
  if (code === 70016 || code === 20003 || code === 70002) {
    return { ok: false, error: "账号或密码错误（code=" + code + "）" };
  }
  return { ok: false, error: "登录失败 code=" + code + " " + (auth.desc || auth.message || "") };
}

/**
 * step1 + step2 合并入口，返回统一的登录中间结果。
 *
 * 关键（对齐 hass-xiaomi-miot master，PR #2948）：
 * serviceLogin 在“已带有效会话”时——比如刚通过短信/邮箱验证、或本地已有
 * passToken——会直接返回 code=0 + location，并且**不再返回 _sign**。
 * 这时必须跳过 serviceLoginAuth2，直接从该响应里取 ssecurity/nonce/passToken。
 * 若仍按老逻辑要求 _sign 并强制走 step2，验证码流程会在最后一步直接报
 * “验证后 serviceLogin 未返回签名参数”，或即使走通也拿不到 ssecurity，
 * 导致后续所有 api.io.mi.com 请求 “invalid signature”。
 */
async function loginStep12(
  creds: XiaomiCreds,
  deviceId: string,
  jar: CookieJar
): Promise<AuthStep12Result> {
  const s1 = await step1(creds, deviceId, jar);
  const fields = s1 && !s1._raw ? Object.keys(s1).join(",") : "(解析失败)";
  logger.log(`serviceLogin[${creds.sid}] 返回字段: ${fields}`);

  // 顺序很关键：必须先判 _sign。
  // 未登录时 serviceLogin 的响应也可能带 location（callback 落地地址），
  // 若先判 location 就会误跳过 serviceLoginAuth2 → 拿不到 ssecurity，
  // 后续 api.io.mi.com 签名失败（表现为 MiIOT.getDevice 报
  // “The "data" argument must be of type string ... Received undefined”）。
  // 参考 Xiaomi-cloud-tokens-extractor login_step_1：先查 _sign，再查 ssecurity。
  if (s1 && s1._sign) {
    const s2 = await step2(creds, deviceId, jar, s1);
    // step2 偶尔不带 ssecurity，回退到 step1 的值
    // （HA: self.ssecurity = auth.get('ssecurity', self.ssecurity)）
    if (s2.ok && !s2.ssecurity && s1.ssecurity) {
      s2.ssecurity = s1.ssecurity;
    }
    return s2;
  }
  // 已带有效会话：serviceLogin 直接给出 location + ssecurity，无需再提交账号密码
  if (s1 && s1.location) {
    return {
      ok: true,
      location: withClientSign(s1.location, creds.sid, s1),
      nonce: s1.nonce,
      ssecurity: s1.ssecurity,
      passToken: s1.passToken,
      userId: String(s1.userId || ""),
      cUserId: s1.cUserId ? String(s1.cUserId) : undefined,
      step1: s1,
    };
  }
  return { ok: false, error: "serviceLogin 未返回签名参数" };
}

/**
 * step3: 风控时调用 identity/list，获取验证方式并持久化 identity_session。
 * 注意：这一步只返回验证方式，不会触发短信/邮件下发，下发靠 step3b 的 send 接口。
 */
export async function checkIdentityList(
  verifyUrl: string,
  jar: CookieJar,
  deviceId?: string
): Promise<{ options: number[]; identitySession: string }> {
  // 先打开 authStart 页面（token_extractor 同样做法），让服务端下发 ick 等 cookie
  try {
    await httpGet(verifyUrl, jar, {
      headers: deviceId ? { "User-Agent": userAgent(deviceId) } : {},
    });
  } catch (e) {
    logger.error("打开 authStart 页面异常（继续）", (e as any)?.message ?? e);
  }
  const listUrl = verifyUrl.replace(
    "fe/service/identity/authStart",
    "identity/list"
  );
  const r = await httpGet(listUrl, jar, {});
  const identitySession = jar.get("identity_session") || "";
  if (!identitySession) {
    throw new Error("identity_session 缺失，小米未下发验证会话");
  }
  saveIdentitySession(identitySession);
  const flag = r.json.flag ?? 4;
  const options: number[] = r.json.options || [flag];
  logger.log(`验证方式: ${JSON.stringify(options)}（4=手机短信，8=邮箱）`);
  return { options, identitySession };
}

/** 从 notificationUrl 提取 context 参数（send/verify 接口需要原样回传） */
function extractContext(verifyUrl: string): string {
  try {
    return new URL(verifyUrl).searchParams.get("context") || "";
  } catch {
    return "";
  }
}

/**
 * step3b: 下发短信/邮箱验证码。
 *
 * identity/list 只返回验证方式，不会触发下发；必须显式调用：
 *   POST /identity/auth/sendPhoneTicket（手机短信，flag=4）
 *   POST /identity/auth/sendEmailTicket（邮箱，flag=8）
 * 参数照抄 token_extractor 的 do_2fa_email_flow（经实测可用的流程）。
 */
export async function sendVerifyTicket(
  flag: number,
  verifyUrl: string,
  sid: string,
  jar: CookieJar,
  deviceId: string
): Promise<{ ok: boolean; error?: string }> {
  const api =
    flag === 4
      ? "/identity/auth/sendPhoneTicket"
      : "/identity/auth/sendEmailTicket";
  const context = extractContext(verifyUrl);
  const r = await httpPost(api, jar, {
    params: {
      _dc: String(Date.now()),
      sid,
      context,
      mask: "0",
      _locale: "zh_CN",
    },
    form: {
      retry: "0",
      icode: "",
      _json: "true",
      ick: jar.get("ick") || "",
    },
    headers: { "User-Agent": userAgent(deviceId) },
  });
  const j: any = r.json || {};
  if (j.code === 0 || j.result === "ok") {
    logger.success(
      `验证码已下发（flag=${flag}，${flag === 4 ? "手机短信" : "邮箱"}）`
    );
    return { ok: true };
  }
  const msg = `发送验证码异常 flag=${flag} HTTP=${r.status} resp=${JSON.stringify(
    j
  ).slice(0, 300)}`;
  logger.error(msg);
  return {
    ok: false,
    error: `发送验证码失败（code=${j.code ?? "?"} ${j.desc || j.message || ""}）`,
  };
}

/**
 * step4: 提交用户输入的短信/邮箱验证码。
 * 参数照抄 token_extractor 的 do_2fa_email_flow（query 带 sid/context/mask/_locale，
 * 表单带 ick），成功返回 { code: 0, location }，之后要重走 step1→step2→step5。
 */
export async function verifyTicket(
  ticket: string,
  verifyUrl: string,
  jar: CookieJar,
  opts: { sid?: string; deviceId?: string } = {}
): Promise<{ ok: boolean; location?: string; error?: string }> {
  const persisted = loadIdentitySession();
  if (persisted) jar.set("identity_session", persisted);
  const { options } = await checkIdentityList(verifyUrl, jar, opts.deviceId);
  const context = extractContext(verifyUrl);
  const sid = opts.sid || "";
  let last: any = null;
  for (const flag of options) {
    const api =
      flag === 4
        ? "/identity/auth/verifyPhone"
        : flag === 8
          ? "/identity/auth/verifyEmail"
          : null;
    if (!api) continue;
    const r = await httpPost(api, jar, {
      params: {
        _dc: String(Date.now()),
        _flag: String(flag),
        _json: "true",
        ...(sid ? { sid } : {}),
        ...(context ? { context } : {}),
        mask: "0",
        _locale: "zh_CN",
      },
      form: {
        _flag: String(flag),
        ticket: ticket.trim(),
        trust: "false",
        _json: "true",
        ick: jar.get("ick") || "",
      },
      headers: opts.deviceId ? { "User-Agent": userAgent(opts.deviceId) } : {},
      cookies: { identity_session: jar.get("identity_session") || "" },
    });
    last = r.json;
    if (last && last.code === 0) {
      // 成功后清掉本地 identity_session（对端已失效）
      saveIdentitySession("");
      return { ok: true, location: last.location };
    }
    logger.error(`验证码提交被拒 flag=${flag}`, JSON.stringify(last).slice(0, 300));
  }
  return {
    ok: false,
    error:
      "验证码被拒绝" +
      (last ? `（code=${last.code} ${last.desc || last.message || ""}）` : ""),
  };
}

/**
 * GET 并手动跟随 3xx 跳转，沿途收集每一跳的 Set-Cookie。
 * 等价于 hass-xiaomi-miot 里 `allow_redirects=True` 的行为。
 */
async function getWithRedirects(
  url: string,
  jar: CookieJar,
  deviceId: string,
  opts: { headers?: Record<string, string>; maxHops?: number } = {}
): Promise<Response> {
  const ua = userAgent(deviceId);
  const maxHops = opts.maxHops ?? 5;
  const doFetch = (target: string) => {
    const headers: Record<string, string> = {
      "User-Agent": ua,
      ...(opts.headers || {}),
    };
    const ck = jar.header();
    if (ck) headers["Cookie"] = ck;
    return fetch(target, { method: "GET", headers, redirect: "manual" });
  };
  let cur = await doFetch(url);
  jar.setFromHeaders(cur.headers);
  let hops = 0;
  while (
    cur.status >= 300 &&
    cur.status < 400 &&
    cur.headers.get("location") &&
    hops < maxHops
  ) {
    // Location 可能是相对路径，用当前 URL 解析成绝对地址
    const next = new URL(cur.headers.get("location")!, cur.url).toString();
    cur = await doFetch(next);
    jar.setFromHeaders(cur.headers);
    hops++;
  }
  return cur;
}

/**
 * step5: GET location 换取 serviceToken。
 * micoapi 的 sts 接口可能瞬时 401，重试一次。
 */
export async function fetchServiceToken(
  location: string,
  jar: CookieJar,
  sid: string,
  deviceId: string,
  retryOnce = true
): Promise<{ serviceToken?: string; userId?: string; retried401?: boolean; error?: string }> {
  const doOnce = () =>
    getWithRedirects(location, jar, deviceId, {
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
    });

  let resp = await doOnce();
  let retried401 = false;
  // micoapi sts 瞬时 401：重试一次（hass-xiaomi-miot 已确认这是小米侧瞬时行为）
  if (
    retryOnce &&
    resp.status === 401 &&
    sid === "micoapi" &&
    location.includes("api2.mina.mi.com")
  ) {
    logger.log("sts 首次 401（小米侧瞬时行为），重试一次…");
    await new Promise((r) => setTimeout(r, 1500));
    resp = await doOnce();
    retried401 = true;
  }
  const serviceToken = jar.get("serviceToken");
  if (!serviceToken) {
    return {
      error: `换取 serviceToken 失败（HTTP ${resp.status}）`,
      retried401,
    };
  }
  return { serviceToken, userId: jar.get("userId"), retried401 };
}

/**
 * 用登录中间结果 + serviceToken 组装 mi-service-lite 需要的 MiAccount。
 * 带上 cUserId：MiIOT 的请求 cookie 会用到它（account.pass.cUserId）。
 */
function makeAccount(
  creds: XiaomiCreds,
  deviceId: string,
  s2: AuthStep12Result,
  serviceToken: string,
  userId?: string
) {
  return {
    sid: creds.sid,
    userId: userId || creds.userId,
    password: creds.password,
    deviceId,
    did: undefined as string | undefined,
    pass: {
      ssecurity: s2.ssecurity,
      nonce: s2.nonce,
      passToken: s2.passToken,
      cUserId: s2.cUserId,
    },
    serviceToken,
  };
}

/**
 * 完整登录（不含验证码提交）：
 * 返回 { ok, account } 或 { ok:false, verifyRequired, verifyUrl, options }
 */
export async function xiaomiAuthLogin(
  creds: XiaomiCreds
): Promise<
  | { ok: true; account: any }
  | {
      ok: false;
      verifyRequired: boolean;
      verifyUrl?: string;
      options?: number[];
      /** 验证码是否已下发（sendPhoneTicket/sendEmailTicket 成功） */
      codeSent?: boolean;
      sendError?: string;
      error?: string;
    }
> {
  const saved = loadSession(creds.sid) || {};
  const deviceId = creds.deviceId || saved.deviceId || randomDeviceId();
  const jar = new CookieJar();
  seedSessionCookies(jar, saved);
  // 无论本次是否要输验证码，都先把 deviceId 固定并落盘，
  // 保证「登录 → 验证码 → 重走登录」全程是同一台设备。
  saveSession(creds.sid, {
    ...saved,
    deviceId,
    userId: saved.userId || creds.userId,
  });
  if (saved.passToken) {
    logger.log(
      `[${creds.sid}] 复用上次登录会话（deviceId=${deviceId}），尝试免验证码登录`
    );
  }
  try {
    // step1(+step2)：已带会话时直接拿 location，否则提交账号密码
    const s2 = await loginStep12(creds, deviceId, jar);
    if (s2.notificationUrl) {
      // 风控：走 identity/list 拿验证方式，然后显式下发验证码（step3b）
      const { options } = await checkIdentityList(s2.notificationUrl, jar, deviceId);
      // 优先手机短信(4)，其次邮箱(8)
      const flag = options.includes(4) ? 4 : options.includes(8) ? 8 : options[0];
      let codeSent = false;
      let sendError = "";
      if (flag === 4 || flag === 8) {
        const sent = await sendVerifyTicket(
          flag,
          s2.notificationUrl,
          creds.sid,
          jar,
          deviceId
        );
        codeSent = sent.ok;
        sendError = sent.error || "";
      } else {
        sendError = `不支持的验证方式: ${JSON.stringify(options)}`;
      }
      return {
        ok: false,
        verifyRequired: true,
        verifyUrl: s2.notificationUrl,
        options,
        codeSent,
        sendError,
      };
    }
    if (!s2.ok || !s2.location) {
      return { ok: false, verifyRequired: false, error: s2.error || "登录失败" };
    }
    if (!s2.ssecurity) {
      // 没有 ssecurity 就没法给 api.io.mi.com 签名，这里明确报错，
      // 而不是等到 MiIOT.getDevice 里抛 crypto 异常（信息量为零）
      return {
        ok: false,
        verifyRequired: false,
        error: "登录响应缺少 ssecurity，无法为 api.io.mi.com 签名（请重试；持续出现请反馈日志）",
      };
    }
    const st = await fetchServiceToken(s2.location, jar, creds.sid, deviceId, true);
    if (!st.serviceToken) {
      return { ok: false, verifyRequired: false, error: st.error };
    }
    const account = makeAccount(creds, deviceId, s2, st.serviceToken, st.userId);
    // 登录成功：把会话（含 passToken）落盘，下次启动即可免验证码
    saveSession(creds.sid, {
      deviceId,
      userId: account.userId,
      cUserId: s2.cUserId,
      passToken: s2.passToken,
      ssecurity: s2.ssecurity,
      nonce: s2.nonce,
      serviceToken: st.serviceToken,
    });
    return { ok: true, account };
  } catch (e: any) {
    return { ok: false, verifyRequired: false, error: e?.message ?? String(e) };
  }
}

/**
 * 验证码提交后的继续流程：verifyTicket → 重走 step1/step2/step5。
 */
export async function xiaomiAuthVerifyAndLogin(
  creds: XiaomiCreds,
  ticket: string,
  verifyUrl: string
): Promise<{ ok: true; account: any } | { ok: false; error?: string }> {
  // 必须复用触发本次验证的同一个 deviceId（xiaomiAuthLogin 已落盘），
  // 否则验证成功后重走登录时又变成「陌生设备」，可能再次被要求验证。
  const saved = loadSession(creds.sid) || {};
  const deviceId = creds.deviceId || saved.deviceId || randomDeviceId();
  const jar = new CookieJar();
  seedSessionCookies(jar, saved);
  try {
    const v = await verifyTicket(ticket, verifyUrl, jar, {
      sid: creds.sid,
      deviceId,
    });
    if (!v.ok) {
      return { ok: false, error: v.error };
    }
    // verify 成功后重走完整登录。
    // hass-xiaomi-miot: 先跟完 verify 返回的 location（allow_redirects=True），
    // 拿到 passToken 等 cookie，再重走 step1(+step2)→step3。
    // 注意：此时 serviceLogin 往往直接返回 code=0 + location（无 _sign），
    // 所以必须走 loginStep12（会跳过 step2 并取到 ssecurity），不能强制要求 _sign。
    if (v.location) {
      await getWithRedirects(v.location, jar, deviceId);
    }
    const s2 = await loginStep12(creds, deviceId, jar);
    if (s2.notificationUrl) {
      return { ok: false, error: "验证后仍触发风控，请稍后再试" };
    }
    if (!s2.ok || !s2.location) {
      return { ok: false, error: s2.error || "验证后登录失败" };
    }
    if (!s2.ssecurity) {
      return {
        ok: false,
        error: "验证后登录响应缺少 ssecurity，无法为 api.io.mi.com 签名（请重试；持续出现请反馈日志）",
      };
    }
    const st = await fetchServiceToken(s2.location, jar, creds.sid, deviceId, true);
    if (!st.serviceToken) {
      return { ok: false, error: st.error };
    }
    const account = makeAccount(creds, deviceId, s2, st.serviceToken, st.userId);
    // 验证通过后同样落盘：这台设备从此被小米信任，后续登录无需再输验证码
    saveSession(creds.sid, {
      deviceId,
      userId: account.userId,
      cUserId: s2.cUserId,
      passToken: s2.passToken,
      ssecurity: s2.ssecurity,
      nonce: s2.nonce,
      serviceToken: st.serviceToken,
    });
    return { ok: true, account };
  } catch (e: any) {
    return { ok: false, error: e?.message ?? String(e) };
  }
}
