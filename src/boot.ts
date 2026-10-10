/**
* 新启动流程（fork 私有版）：
* 1. 只加载配置、只启动 Web 管理页（8090），不自动登录小米。
* 2. 用户在 Web 页点“授权登录”后，才走小米登录并启动语音服务。
* 这样彻底告别：启动即崩、docker restart 刷登录接口加重风控。
*/
import { MiGPT} from "./index";
import { installLogBuffer} from "./services/logbuffer";
import { loadConfig, applyEnv, hasXiaomiAccount, configPath} from "./services/config";
import { setMiGPTClass} from "./services/supervisor";
import { startWeb} from "./services/web";
import { kBannerASCII} from "./utils/string";
import { Logger} from "./utils/log";

const logger = Logger.create({ tag: "Boot"});

export async function boot() {
installLogBuffer();
console.log(kBannerASCII);
console.log(" MiGPT fork 私有版 by: 大哥\n");

const { config, isNew} = await loadConfig();
applyEnv(config);
setMiGPTClass(MiGPT);

const port = config.web.port || 8090;
await startWeb(config);

if (isNew) {
logger.log(`首次启动，已生成默认配置: ${configPath()}`);
}
if (!hasXiaomiAccount(config)) {
logger.log("⚠️ 尚未配置小米账号，语音服务不会启动。");
} else {
logger.log("配置已就绪，但【不会】自动登录小米。");
}
logger.success(
`请打开 Web 管理页 http://<本机IP>:${port} 完成配置，点击“授权登录”启动语音服务。`
);
}
