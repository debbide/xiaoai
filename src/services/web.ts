/**
 * Web 管理页（Node 原生 http，无新增依赖）
 * - GET  /            状态页：登录态 / 验证链接（可点击）/ 三个测试按钮 / 日志
 * - GET  /config      配置表单页
 * - POST /config      保存配置（保存后容器自动重启以生效）
 * - POST /api/xiaomi/test    测试小米登录（不启动服务）
 * - POST /api/xiaomi/login   授权登录并启动语音服务
 * - POST /api/openai/test    测试 OpenAI 中转连通性
 * - POST /api/device/test    测试音箱设备可达性
 */
import http from "http";
import { Logger } from "../utils/log";
import { Status } from "./status";
import { getRecentLogs } from "./logbuffer";
import {
  ForkConfig,
  loadConfig,
  saveConfig,
  applyEnv,
  hasXiaomiAccount,
} from "./config";
import { xiaomiLogin, xiaomiSubmitVerifyCode } from "./xiaomi";
import { SessionStore } from "./session";
import { loginAndStart, getClient, startWithSession } from "./supervisor";
import { openai } from "./openai";

const logger = Logger.create({ tag: "Web" });

let currentConfig: ForkConfig | null = null;

function esc(s: any): string {
  return String(s ?? "")
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

function readBody(req: http.IncomingMessage): Promise<string> {
  return new Promise((resolve, reject) => {
    let body = "";
    req.on("data", (c) => {
      body += c;
      if (body.length > 1024 * 1024) reject(new Error("body too large"));
    });
    req.on("end", () => resolve(body));
    req.on("error", reject);
  });
}

function sendJson(res: http.ServerResponse, obj: any, code = 200) {
  res.writeHead(code, { "Content-Type": "application/json; charset=utf-8" });
  res.end(JSON.stringify(obj));
}

function sendHtml(res: http.ServerResponse, html: string, code = 200) {
  res.writeHead(code, { "Content-Type": "text/html; charset=utf-8" });
  res.end(html);
}

const kCss = `
body{font-family:-apple-system,"PingFang SC","Microsoft YaHei",sans-serif;max-width:860px;margin:0 auto;padding:16px;background:#f5f5f5;color:#222}
.card{background:#fff;border-radius:10px;padding:16px;margin:12px 0;box-shadow:0 1px 3px rgba(0,0,0,.08)}
h1{font-size:22px}h2{font-size:17px;margin:0 0 10px}
.badge{display:inline-block;padding:3px 10px;border-radius:12px;font-size:13px;font-weight:700}
.ok{background:#e6f9ec;color:#0a7d2c}.warn{background:#fff4e0;color:#b26a00}.err{background:#fdecea;color:#c62828}.idle{background:#ececec;color:#666}
button{padding:9px 16px;margin:4px 6px 4px 0;border:0;border-radius:8px;background:#1677ff;color:#fff;font-size:14px;cursor:pointer}
button:disabled{background:#bbb;cursor:wait}
button.ghost{background:#eee;color:#222}
a.verify{display:block;margin:6px 0;word-break:break-all;color:#1677ff}
pre.logs{background:#111;color:#cfc;font-size:12px;padding:10px;border-radius:8px;max-height:260px;overflow:auto;white-space:pre-wrap}
label{display:block;margin:10px 0 4px;font-size:14px;color:#555}
input[type=text],input[type=password],select{width:100%;padding:9px;border:1px solid #ccc;border-radius:8px;font-size:14px;box-sizing:border-box}
input[type=checkbox]{transform:scale(1.3)}
.row{display:flex;gap:10px}.row>div{flex:1}
.hint{font-size:12px;color:#888;margin-top:4px}
.result{margin-top:8px;padding:10px;border-radius:8px;background:#f0f4ff;font-size:14px;display:none;white-space:pre-wrap;word-break:break-all}
`;

function statusPage(): string {
  return `<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>MiGPT 管理页</title><style>${kCss}</style></head><body>
<h1>🎙️ MiGPT 管理页 <span style="font-size:12px;color:#888">fork 私有版</span></h1>

<div class="card"><h2>📡 运行状态</h2>
<div id="status">加载中…</div>
<div style="margin-top:10px">
<button id="btnLogin" onclick="api('/api/xiaomi/login','loginResult')">🔑 授权登录并启动服务</button>
<button class="ghost" onclick="location.href='/config'">⚙️ 配置</button>
</div><div class="result" id="loginResult"></div></div>

<div class="card"><h2>🧪 连通性测试</h2>
<button onclick="api('/api/xiaomi/test','t1')">测试小米登录</button>
<button onclick="api('/api/openai/test','t2')">测试 OpenAI 中转</button>
<button onclick="api('/api/device/test','t3')">测试音箱设备</button>
<div class="result" id="t1"></div><div class="result" id="t2"></div><div class="result" id="t3"></div>
<div class="hint">小米登录测试只验证账号，不启动语音服务；“授权登录”成功后才开始轮询音箱消息。</div></div>

<div class="card"><h2>📜 最近日志</h2><pre class="logs" id="logs"></pre></div>

<script>
async function refresh(){
  try{
    const s = await (await fetch('/api/status')).json();
    const xm = {idle:['未登录','idle'],'logging-in':['登录中…','warn'],ok:['已登录','ok'],'verify-required':['需浏览器验证','err'],'verify-code-required':['等待输入验证码','warn'],failed:['登录失败','err']}[s.xiaomi]||[s.xiaomi,'idle'];
    const sv = {stopped:['已停止','idle'],starting:['启动中…','warn'],running:['运行中','ok']}[s.service]||[s.service,'idle'];
    let h = '<div>小米账号：<span class="badge '+xm[1]+'">'+xm[0]+'</span>　语音服务：<span class="badge '+sv[1]+'">'+sv[0]+'</span></div>';
    if(!s.configReady) h += '<div style="margin-top:8px">⚠️ 还没填配置，先去 <a href="/config">配置页</a> 填写小米账号和 OpenAI 中转。</div>';
    if(s.xiaomi==='verify-code-required'){
      h += '<div class="card" style="background:#fff8e1;margin:12px 0 0;padding:12px;border-radius:8px">'
        + '<div style="font-weight:700;margin-bottom:8px">📱 小米下发了短信/邮箱验证码，请输入：</div>'
        + '<div class="row"><div><input type="text" id="verifyTicket" placeholder="6位验证码" maxlength="12" style="font-size:18px;letter-spacing:4px"></div>'
        + '<div><button onclick="submitVerifyCode()">✅ 提交验证码</button></div></div>'
        + '<div class="result" id="verifyResult"></div>'
        + '<div class="hint">没收到？等 60 秒后点“授权登录”重试（会重新下发）。也可以打开下方浏览器链接走网页验证。</div></div>';
    }
    const urls = s.verifyUrls||{};
    const keys = Object.keys(urls);
    if(keys.length){
      h += '<div style="margin-top:10px;color:#c62828;font-weight:700">🔥 小米触发了异地登录验证，请依次打开下面链接完成验证，然后点“授权登录”重试：</div>';
      keys.forEach(k=>{ h += '<a class="verify" target="_blank" href="'+urls[k]+'">👉 ['+k+'] 打开验证</a>'; });
    }
    if(s.lastError) h += '<div style="margin-top:8px;color:#c62828">'+esc(s.lastError)+'</div>';
    if(s.device) h += '<div style="margin-top:8px" class="hint">设备：'+esc(s.device.name||'')+'（'+esc(s.device.model||'')+'） did='+esc(s.device.did||'')+'</div>';
    document.getElementById('status').innerHTML = h;
    document.getElementById('logs').textContent = (s.logs||[]).join('\\n');
  }catch(e){}
}
function esc(s){return String(s??'').replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');}
async function api(url, rid){
  const box = document.getElementById(rid); box.style.display='block'; box.textContent='请求中…';
  const btns = document.querySelectorAll('button'); btns.forEach(b=>b.disabled=true);
  try{
    const r = await (await fetch(url,{method:'POST'})).json();
    box.textContent = JSON.stringify(r, null, 2);
  }catch(e){ box.textContent = '请求失败: '+e; }
  btns.forEach(b=>b.disabled=false); refresh();
}
async function submitVerifyCode(){
  const box = document.getElementById('verifyResult'); box.style.display='block'; box.textContent='提交中…';
  const ticket = (document.getElementById('verifyTicket')||{}).value || '';
  const btns = document.querySelectorAll('button'); btns.forEach(b=>b.disabled=true);
  try{
    const r = await (await fetch('/api/xiaomi/verify-code',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({ticket})})).json();
    box.textContent = JSON.stringify(r, null, 2);
    if(r.verifyRequired){ /* 还需继续输入（另一个 sid） */ }
  }catch(e){ box.textContent = '请求失败: '+e; }
  btns.forEach(b=>b.disabled=false); refresh();
}
refresh(); setInterval(refresh, 5000);
</script></body></html>`;
}

function configPage(cfg: ForkConfig, msg = ""): string {
  const f = (name: string, val: any, type = "text", hint = "") =>
    `<label>${name}</label><input type="${type}" name="${name}" value="${esc(val)}">${hint ? `<div class="hint">${hint}</div>` : ""}`;
  const arr = (a: string[]) => (a || []).join(",");
  return `<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>MiGPT 配置</title><style>${kCss}</style></head><body>
<h1>⚙️ 配置</h1>
${msg ? `<div class="card"><b>${msg}</b></div>` : ""}
<form method="POST" action="/config">
<div class="card"><h2>小米账号</h2>
${f("xiaomi.userId", cfg.xiaomi.userId, "text", "纯数字小米 ID（account.xiaomi.com → 个人信息里看），不要填手机号")}
${f("xiaomi.password", cfg.xiaomi.password, "password")}
${f("xiaomi.did", cfg.xiaomi.did, "text", "米家 App 里的音箱名称，必须一字不差；留空则自动取账号下设备")}
</div>
<div class="card"><h2>OpenAI 中转</h2>
${f("openai.model", cfg.openai.model)}
${f("openai.apiKey", cfg.openai.apiKey, "password")}
${f("openai.baseURL", cfg.openai.baseURL, "text", "你的 OpenAI 兼容中转地址，一般以 /v1 结尾")}
</div>
<div class="card"><h2>音箱（LX06 默认值已填好）</h2>
<div class="row"><div>${f("speaker.name", cfg.speaker.name)}</div>
<div><label>tts</label><select name="speaker.tts">
<option value="xiaoai"${cfg.speaker.tts === "xiaoai" ? " selected" : ""}>xiaoai（小爱自带）</option>
<option value="custom"${cfg.speaker.tts === "custom" ? " selected" : ""}>custom（第三方 TTS）</option></select></div></div>
<div class="row"><div>${f("speaker.ttsCommand", (cfg.speaker.ttsCommand || []).join(","), "text", "LX06=5,1")}</div>
<div>${f("speaker.wakeUpCommand", (cfg.speaker.wakeUpCommand || []).join(","), "text", "LX06=5,3")}</div></div>
<label><input type="checkbox" name="speaker.streamResponse" value="1"${cfg.speaker.streamResponse ? " checked" : ""}> 流式响应 streamResponse（LX06 保持开启）</label>
${f("speaker.callAIKeywords", arr(cfg.speaker.callAIKeywords), "text", "逗号分隔，如：请,你")}
${f("speaker.wakeUpKeywords", arr(cfg.speaker.wakeUpKeywords), "text", "逗号分隔，如：召唤")}
${f("speaker.exitKeywords", arr(cfg.speaker.exitKeywords), "text", "逗号分隔，如：关闭,退出,再见")}
</div>
<div class="card">
<button type="submit">💾 保存配置</button>
<button type="button" class="ghost" onclick="location.href='/'">返回状态页</button>
<div class="hint">保存后容器会自动重启使配置生效（约几秒），然后回到状态页。</div>
</div></form></body></html>`;
}

function parsePair(s: string): [number, number] {
  const p = String(s || "")
    .split(",")
    .map((x) => parseInt(x.trim(), 10))
    .filter((x) => !isNaN(x));
  return [p[0] ?? 5, p[1] ?? 1];
}

async function handlePostConfig(body: string): Promise<ForkConfig> {
  const q = new URLSearchParams(body);
  const g = (k: string) => (q.get(k) ?? "").trim();
  const cfg = (await loadConfig()).config;
  cfg.xiaomi.userId = g("xiaomi.userId");
  cfg.xiaomi.password = g("xiaomi.password");
  cfg.xiaomi.did = g("xiaomi.did");
  cfg.openai.model = g("openai.model") || "gpt-4o";
  cfg.openai.apiKey = g("openai.apiKey");
  cfg.openai.baseURL = g("openai.baseURL") || "https://api.openai.com/v1";
  cfg.speaker.name = g("speaker.name") || "傻妞";
  cfg.speaker.tts = g("speaker.tts") === "custom" ? "custom" : "xiaoai";
  cfg.speaker.ttsCommand = parsePair(g("speaker.ttsCommand"));
  cfg.speaker.wakeUpCommand = parsePair(g("speaker.wakeUpCommand"));
  cfg.speaker.streamResponse = q.get("speaker.streamResponse") === "1";
  const split = (s: string) =>
    s
      .split(/[,，]/)
      .map((x) => x.trim())
      .filter(Boolean);
  cfg.speaker.callAIKeywords = split(g("speaker.callAIKeywords"));
  cfg.speaker.wakeUpKeywords = split(g("speaker.wakeUpKeywords"));
  cfg.speaker.exitKeywords = split(g("speaker.exitKeywords"));
  await saveConfig(cfg);
  applyEnv(cfg);
  openai.reset();
  currentConfig = cfg;
  Status.configReady = hasXiaomiAccount(cfg);
  return cfg;
}

async function testOpenAI(): Promise<any> {
  const t0 = Date.now();
  try {
    const msg: any = await Promise.race([
      openai.chat({
        user: "ping",
        system: "只回复 pong，不要说其他话。",
      }),
      new Promise((_, rej) =>
        setTimeout(() => rej(new Error("请求超时(25s)，检查 baseURL/网络")), 25000)
      ),
    ]);
    const text = msg?.content ?? "";
    if (text) {
      return {
        ok: true,
        latencyMs: Date.now() - t0,
        model: currentConfig?.openai.model,
        reply: String(text).slice(0, 200),
      };
    }
    return { ok: false, error: "中转返回为空（见容器日志里的 LLM 响应异常）" };
  } catch (e: any) {
    return { ok: false, error: e?.message ?? String(e) };
  }
}

async function testDevice(): Promise<any> {
  const sess = SessionStore.get();
  const client = getClient();
  const miNA = (client as any)?.speaker?.MiNA ?? sess?.miNA;
  const miIOT = (client as any)?.speaker?.MiIOT ?? sess?.miIOT;
  if (!miNA || !miIOT) {
    return { ok: false, error: "还没有可用的小米会话，请先点“测试小米登录”或“授权登录”" };
  }
  try {
    const t0 = Date.now();
    const st = await Promise.race([
      miNA.getStatus(),
      new Promise((_, rej) => setTimeout(() => rej(new Error("timeout")), 15000)),
    ]);
    const d = miIOT?.account?.device;
    return {
      ok: true,
      latencyMs: Date.now() - t0,
      device: d
        ? { name: d.name, model: d.model, did: d.did, fw: d?.extra?.fw_version }
        : null,
      speakerStatus: st ?? null,
    };
  } catch (e: any) {
    return { ok: false, error: "设备不可达: " + (e?.message ?? e) };
  }
}

export async function startWeb(cfg: ForkConfig): Promise<http.Server> {
  currentConfig = cfg;
  Status.configReady = hasXiaomiAccount(cfg);
  const port = cfg.web.port || 8090;
  const server = http.createServer(async (req, res) => {
    try {
      const url = new URL(req.url || "/", "http://localhost");
      if (req.method === "GET" && url.pathname === "/") {
        sendHtml(res, statusPage());
        return;
      }
      if (req.method === "GET" && url.pathname === "/api/status") {
        sendJson(res, {
          ...Status.snapshot(),
          logs: getRecentLogs(120),
          time: Date.now(),
        });
        return;
      }
      if (req.method === "GET" && url.pathname === "/config") {
        const { config } = await loadConfig();
        sendHtml(res, configPage(config, url.searchParams.get("saved") ? "✅ 已保存，容器即将重启生效…" : ""));
        return;
      }
      if (req.method === "POST" && url.pathname === "/config") {
        const body = await readBody(req);
        await handlePostConfig(body);
        logger.log("配置已保存，容器即将重启生效");
        // 让 docker restart 策略拉起新进程（新配置只在启动时加载）
        setTimeout(() => process.exit(0), 1200);
        res.writeHead(302, { Location: "/config?saved=1" });
        res.end();
        return;
      }
      if (req.method === "POST" && url.pathname === "/api/xiaomi/test") {
        if (!hasXiaomiAccount(cfg)) {
          sendJson(res, { ok: false, error: "请先在配置页填写小米账号和密码" });
          return;
        }
        const r = await xiaomiLogin({
          userId: cfg.xiaomi.userId,
          password: cfg.xiaomi.password,
          did: cfg.xiaomi.did || undefined,
        });
        sendJson(res, {
          ok: r.ok,
          verifyUrls: r.verifyUrls,
          error: r.error,
          state: Status.xiaomi,
        });
        return;
      }
      if (req.method === "POST" && url.pathname === "/api/xiaomi/login") {
        if (!hasXiaomiAccount(cfg)) {
          sendJson(res, { ok: false, error: "请先在配置页填写小米账号和密码" });
          return;
        }
        const r = await loginAndStart(cfg);
        sendJson(res, r);
        return;
      }
      if (req.method === "POST" && url.pathname === "/api/xiaomi/verify-code") {
        if (!hasXiaomiAccount(cfg)) {
          sendJson(res, { ok: false, error: "请先在配置页填写小米账号和密码" });
          return;
        }
        let ticket = "";
        try {
          const body = await readBody(req);
          ticket = (JSON.parse(body).ticket || "").trim();
        } catch {
          /* ignore */
        }
        const r = await xiaomiSubmitVerifyCode(ticket, {
          userId: cfg.xiaomi.userId,
          password: cfg.xiaomi.password,
          did: cfg.xiaomi.did || undefined,
        });
        // 验证码通过且全部 sid 登录完成 → 用已验证的会话直接启动服务（不再重复登录）
        if (r.ok && (r as any).miNA && (r as any).miIOT) {
          const started = await startWithSession(cfg, (r as any).miNA, (r as any).miIOT);
          sendJson(res, { ...r, service: started });
          return;
        }
        sendJson(res, r);
        return;
      }
      if (req.method === "POST" && url.pathname === "/api/openai/test") {
        if (!currentConfig?.openai.apiKey) {
          sendJson(res, { ok: false, error: "请先在配置页填写 OpenAI API Key" });
          return;
        }
        sendJson(res, await testOpenAI());
        return;
      }
      if (req.method === "POST" && url.pathname === "/api/device/test") {
        sendJson(res, await testDevice());
        return;
      }
      sendJson(res, { error: "not found" }, 404);
    } catch (e: any) {
      logger.error("Web 请求异常", e?.message ?? e);
      sendJson(res, { ok: false, error: e?.message ?? String(e) }, 500);
    }
  });
  await new Promise<void>((resolve) => server.listen(port, "0.0.0.0", resolve));
  logger.success(`Web 管理页已启动: http://0.0.0.0:${port}`);
  return server;
}
