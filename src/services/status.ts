/**
 * 全局状态单例：供 Web 管理页展示小米登录态、验证链接、服务运行态。
 */

export type XiaomiState =
  | "idle" // 未开始
  | "logging-in" // 登录中
  | "ok" // 已登录
  | "verify-required" // 触发风控，需要浏览器验证（兜底）
  | "verify-code-required" // 触发风控，等待用户输入短信/邮箱验证码
  | "failed"; // 登录失败（账号密码错等）

export type ServiceState = "stopped" | "starting" | "running";

class StatusStore {
  xiaomi: XiaomiState = "idle";
  service: ServiceState = "stopped";
  /** sid -> 验证链接（如 micoapi / xiaomiio） */
  verifyUrls: Record<string, string> = {};
  lastError = "";
  lastLoginAt = 0;
  configReady = false;
  device: Record<string, any> | null = null;

  snapshot() {
    return {
      xiaomi: this.xiaomi,
      service: this.service,
      verifyUrls: this.verifyUrls,
      lastError: this.lastError,
      lastLoginAt: this.lastLoginAt,
      configReady: this.configReady,
      device: this.device,
    };
  }
}

export const Status = new StatusStore();
