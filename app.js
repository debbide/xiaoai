// MiGPT fork 私有版入口：只启动 Web 管理页，不自动登录小米。
// 配置见 /app/conf/config.json（可用 MIGPT_CONF 环境变量覆盖路径）。
import { boot } from "./dist/index.cjs";

boot().catch((e) => {
  console.error("启动失败:", e);
  process.exit(1);
});
