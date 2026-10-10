/**
 * 配置管理：全部配置收敛到一个 JSON 文件，不再依赖 .env / .migpt.js。
 * 默认路径 /app/conf/config.json，可用环境变量 MIGPT_CONF 覆盖（方便本地调试）。
 */
import { kEnvs } from "../utils/env";
import { readJSONSync, writeJSON, exists } from "../utils/io";

export interface ForkConfig {
  xiaomi: {
    userId: string;
    password: string;
    did: string;
  };
  openai: {
    model: string;
    apiKey: string;
    baseURL: string;
  };
  speaker: {
    name: string;
    tts: "xiaoai" | "custom";
    ttsCommand: [number, number];
    wakeUpCommand: [number, number];
    streamResponse: boolean;
    callAIKeywords: string[];
    wakeUpKeywords: string[];
    exitKeywords: string[];
  };
  web: {
    port: number;
  };
}

export function defaultConfig(): ForkConfig {
  return {
    xiaomi: { userId: "", password: "", did: "" },
    openai: {
      model: "gpt-4o",
      apiKey: "",
      baseURL: "https://api.openai.com/v1",
    },
    speaker: {
      name: "傻妞",
      tts: "xiaoai",
      ttsCommand: [5, 1], // LX06 小爱音箱 Pro
      wakeUpCommand: [5, 3], // LX06 小爱音箱 Pro
      streamResponse: true,
      callAIKeywords: ["请", "你"],
      wakeUpKeywords: ["召唤"],
      exitKeywords: ["关闭", "退出", "再见"],
    },
    web: { port: 8090 },
  };
}

export function configPath(): string {
  return process.env.MIGPT_CONF || "/app/conf/config.json";
}

/** 深合并：缺失字段用默认值补齐，保证老配置文件向前兼容 */
function mergeDefaults(cfg: any): ForkConfig {
  const d = defaultConfig();
  return {
    xiaomi: { ...d.xiaomi, ...(cfg?.xiaomi ?? {}) },
    openai: { ...d.openai, ...(cfg?.openai ?? {}) },
    speaker: { ...d.speaker, ...(cfg?.speaker ?? {}) },
    web: { ...d.web, ...(cfg?.web ?? {}) },
  };
}

export async function loadConfig(): Promise<{ config: ForkConfig; isNew: boolean }> {
  const p = configPath();
  if (!exists(p)) {
    const cfg = defaultConfig();
    await writeJSON(p, cfg);
    return { config: cfg, isNew: true };
  }
  const raw = readJSONSync(p) as any;
  return { config: mergeDefaults(raw), isNew: false };
}

export async function saveConfig(cfg: ForkConfig): Promise<boolean> {
  return writeJSON(configPath(), cfg);
}

/**
 * 把 openai 配置映射到 openai 官方 SDK 读取的环境变量。
 * 注意：utils/env.ts 的 kEnvs 在模块加载时快照了 process.env，
 * 所以这里两边都要写。
 */
export function applyEnv(cfg: ForkConfig) {
  const { model, apiKey, baseURL } = cfg.openai;
  if (model) {
    process.env.OPENAI_MODEL = model;
    kEnvs.OPENAI_MODEL = model;
  }
  if (apiKey) {
    process.env.OPENAI_API_KEY = apiKey;
    kEnvs.OPENAI_API_KEY = apiKey;
  }
  if (baseURL) {
    process.env.OPENAI_BASE_URL = baseURL;
    kEnvs.OPENAI_BASE_URL = baseURL;
  }
}

export function hasXiaomiAccount(cfg: ForkConfig): boolean {
  return !!(cfg.xiaomi.userId && cfg.xiaomi.password);
}
