# MiGPT fork 私有版

基于 [idootop/mi-gpt](https://github.com/idootop/mi-gpt) v4.2.0（已归档）魔改的私有版本。
**不推远端，仅本地使用。**

## 和原版的区别

| 原版 | 本 fork |
|---|---|
| 配置靠 `.env` + `.migpt.js` 文件 | Web 管理页表单配置，存 `/app/conf/config.json` |
| 启动即自动登录小米，失败直接崩进程 + docker 重启刷接口 | 启动**只跑 Web 页（8090）**，不自动登录；用户点“授权登录”才走登录流程 |
| 风控验证链接藏在日志里，要翻日志复制 | 验证链接直接显示在 Web 页，可点击跳转；**或直接在页面输入短信验证码，程序化完成验证，不用去浏览器** |
| 登录失败直接崩进程 | 登录失败只打日志不崩；风控时等验证码输入 |
| 无 | 三个独立测试按钮：小米登录 / OpenAI 中转 / 音箱设备可达性 |

## 快速开始（Docker Compose）

```bash
cd /home/docker/migpt   # 或你放 compose 的目录
# 把本仓库的 docker-compose.yml 拷过去（build 上下文指向本仓库目录）
docker compose build
docker compose up -d
```

打开 http://宿主机IP:8090：
1. 点右上“配置”，填小米账号（纯数字 ID + 密码 + 米家里的音箱名）、OpenAI 中转（model/apiKey/baseURL）
2. 保存后容器会自动重启（约几秒）
3. 回到状态页，先点“测试小米登录” / “测试 OpenAI 中转” / “测试音箱设备”
4. 都 OK 后点“🔑 授权登录并启动服务”

如果小米触发异地验证：页面会提示“等待输入验证码”（小米已向手机/邮箱下发），
直接在页面输入 6 位验证码点提交，程序化走完验证（identity/list → verifyPhone），
不用去浏览器，也不用等 1 小时。**验证通过后如遇 sts 瞬时 401 会自动重试一次。**
（兜底：页面上也有浏览器验证链接，手动走完也行。）

## 使用说明

- LX06（小爱音箱 Pro）默认值已预填：`ttsCommand=[5,1]`、`wakeUpCommand=[5,3]`、`streamResponse=true`
- 对音箱说：`小爱同学，请讲个笑话`（单句）；`小爱同学，召唤`（连续对话）；`小爱同学，退出`
- 配置文件在宿主机 `./conf/config.json`，含明文密码，注意保管
- 日志：`docker logs mi-gpt`；最近 120 行也在 Web 状态页可见

## 已知限制（和原版一样）

- 走小米云轮询，有 1~2 秒延迟；原版小爱偶尔会先抢答一句（劫持方案固有）
- 小米云接口是逆向的，哪天小米改了就得跟着修——自用小范围，低调点活得久

## 改了哪些文件（相对上游 v4.2.0）

- 新增 `src/services/config.ts`：JSON 配置加载/保存/默认值
- 新增 `src/services/status.ts`：全局状态（登录态/验证链接/服务态，新增 `verify-code-required`）
- 新增 `src/services/logbuffer.ts`：内存日志环（Web 页展示）
- 新增 `src/services/session.ts`：登录会话保持
- 新增 `src/services/xiaomi-auth.ts`：**自研小米登录**（替代 mi-service-lite 过时的 getAccount）。
  移植自 HomeAssistant hass-xiaomi-miot：serviceLogin → serviceLoginAuth2 →
  identity/list（持久化 identity_session 到 `/app/conf/cookies.json`）→
  verifyPhone/verifyEmail 程序化提交验证码 → serviceToken；
  micoapi 的 sts 401 自动重试一次。用原生 fetch，无新增依赖。
- 新增 `src/services/xiaomi.ts`：登录编排（按 xiaomiio→micoapi 顺序，风控时停下等验证码），
  成功后直接构造 MiNA/MiIOT 实例；新增 `xiaomiSubmitVerifyCode()` 续跑中断的流程。
- 新增 `src/services/supervisor.ts`：手动“授权登录 → 注入会话 → 后台启动服务”
- 新增 `src/services/web.ts`：原生 http 管理页（:8090），状态/配置/三个测试按钮；
  新增 `POST /api/xiaomi/verify-code`（提交短信验证码），`verify-code-required` 时显示输入框
- 新增 `src/boot.ts`：新启动流程（只起 Web，不自动登录）
- 改 `src/services/speaker/base.ts`：`initMiServices()` 返回 boolean 不再 assert 崩；新增 `setSession()`
- 改 `src/services/speaker/speaker.ts`：`run()` 登录失败只打日志直接返回，不崩进程
- 改 `src/services/openai.ts`：新增 `reset()`（配置变更后重建 client）
- 改 `src/index.ts`：导出 `boot`
- 改 `app.js`：调 `boot()`
- 改 `Dockerfile`：EXPOSE 8090 + 建 /app/conf
- 新增 `docker-compose.yml`：compose 示例（含代理绕开）
