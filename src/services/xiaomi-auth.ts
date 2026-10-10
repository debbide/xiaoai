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
 * 流程：
 *   step1: GET /pass/serviceLogin?sid=xxx 拿 _sign/qs/callback
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

export function saveIdentitySession(identitySession: string) {
  try {
    const p = cookiesPath();
    const cur = exists(p) ? (readJSONSync(p) as any) : {};
    cur.identity_session = identitySession;
    cur.updatedAt = Date.now();
    writeJSON(p, cur);
  } catch (e) {
    logger.error("保存 identity_session 失败", e);
  }
}

export function loadIdentitySession(): string | undefined {
  try {
    const p = cookiesPath();
    if (!exists(p)) return undefined;
    const cur = readJSONSync(p) as any;
    return cur?.identity_session;
  } catch {
    return undefined;
  }
}

/* ---------------- 工具 ---------------- */

function md5upper(s: string): string {
  return crypto.createHash("md5").update(s, "utf8").digest("hex").toUpperCase();
}

function sha1b64(s: string): string {
  return crypto.createHash("sha1").update(s, "utf8").digest("base64");
}

function parseAuthJson(text: string): any {
  // 小米返回 &&&START&&&{...} 格式
  const t = text.replace("&&&START&&&", "").trim();
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
    let location: string = auth.location;
    // micoapi 需要 clientSign（hass-xiaomi-miot _login_step2）
    if (creds.sid !== "xiaomiio" && auth.nonce && auth.ssecurity) {
      const sign = sha1b64(`nonce=${auth.nonce}&${auth.ssecurity}`);
      location += "&clientSign=" + encodeURIComponent(sign);
    }
    return {
      ok: true,
      location,
      nonce: auth.nonce,
      ssecurity: auth.ssecurity,
      passToken: auth.passToken,
      userId: String(auth.userId || ""),
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
  const doOnce = async () => {
    const headers: Record<string, string> = {
      "User-Agent": userAgent(deviceId),
      "Content-Type": "application/x-www-form-urlencoded",
    };
    const ck = jar.header();
    if (ck) headers["Cookie"] = ck;
    const resp = await fetch(location, {
      method: "GET",
      headers,
      redirect: "manual",
    });
    // 手动跟 302，收集每一跳的 cookie
    let cur: Response = resp;
    let hops = 0;
    while (
      cur.status >= 300 &&
      cur.status < 400 &&
      cur.headers.get("location") &&
      hops < 5
    ) {
      jar.setFromHeaders(cur.headers);
      const next = cur.headers.get("location")!;
      const h2: Record<string, string> = {
        "User-Agent": userAgent(deviceId),
      };
      const ck2 = jar.header();
      if (ck2) h2["Cookie"] = ck2;
      cur = await fetch(next, { method: "GET", headers: h2, redirect: "manual" });
      hops++;
    }
    jar.setFromHeaders(cur.headers);
    return cur;
  };

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
  const deviceId = creds.deviceId || randomDeviceId();
  const jar = new CookieJar();
  try {
    const s1 = await step1(creds, deviceId, jar);
    // step1 不要求 code==0，只要拿到 _sign/qs 就继续（mi-service-lite 同样逻辑）
    if (!s1._sign) {
      return { ok: false, verifyRequired: false, error: "serviceLogin 未返回签名参数" };
    }
    const s2 = await step2(creds, deviceId, jar, s1);
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
    const st = await fetchServiceToken(s2.location, jar, creds.sid, deviceId, true);
    if (!st.serviceToken) {
      return { ok: false, verifyRequired: false, error: st.error };
    }
    const account = {
      sid: creds.sid,
      userId: st.userId || creds.userId,
      password: creds.password,
      deviceId,
      did: undefined as string | undefined,
      pass: {
        ssecurity: s2.ssecurity,
        nonce: s2.nonce,
        passToken: s2.passToken,
      },
      serviceToken: st.serviceToken,
    };
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
  const deviceId = creds.deviceId || randomDeviceId();
  const jar = new CookieJar();
  try {
    const v = await verifyTicket(ticket, verifyUrl, jar, {
      sid: creds.sid,
      deviceId,
    });
    if (!v.ok) {
      return { ok: false, error: v.error };
    }
    // verify 成功后重走完整登录（此时风控已过，step2 会直接返回 location）
    // hass-xiaomi-miot: 先跟 verify 返回的 location，再重走 step1→step2→step3
    if (v.location) {
      const headers: Record<string, string> = {
        "User-Agent": userAgent(deviceId),
      };
      const ck = jar.header();
      if (ck) headers["Cookie"] = ck;
      const resp = await fetch(v.location, {
        method: "GET",
        headers,
        redirect: "manual",
      });
      jar.setFromHeaders(resp.headers);
    }
    const s1 = await step1(creds, deviceId, jar);
    if (!s1._sign) {
      return { ok: false, error: "验证后 serviceLogin 未返回签名参数" };
    }
    const s2 = await step2(creds, deviceId, jar, s1);
    if (s2.notificationUrl) {
      return { ok: false, error: "验证后仍触发风控，请稍后再试" };
    }
    if (!s2.ok || !s2.location) {
      return { ok: false, error: s2.error || "验证后登录失败" };
    }
    const st = await fetchServiceToken(s2.location, jar, creds.sid, deviceId, true);
    if (!st.serviceToken) {
      return { ok: false, error: st.error };
    }
    const account = {
      sid: creds.sid,
      userId: st.userId || creds.userId,
      password: creds.password,
      deviceId,
      did: undefined as string | undefined,
      pass: {
        ssecurity: s2.ssecurity,
        nonce: s2.nonce,
        passToken: s2.passToken,
      },
      serviceToken: st.serviceToken,
    };
    return { ok: true, account };
  } catch (e: any) {
    return { ok: false, error: e?.message ?? String(e) };
  }
}
