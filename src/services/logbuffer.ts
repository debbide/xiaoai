/**
 * 内存日志环形缓冲：Web 状态页展示最近日志用。
 * 在 boot 时通过 installLogBuffer() 接管 console，保持原输出不变。
 */

const kMaxLines = 300;
const lines: string[] = [];
let installed = false;

function fmt(args: any[]): string {
  return args
    .map((a) => {
      if (typeof a === "string") return a;
      try {
        return JSON.stringify(a);
      } catch {
        return String(a);
      }
    })
    .join(" ");
}

export function installLogBuffer() {
  if (installed) return;
  installed = true;
  const origLog = console.log.bind(console);
  const origErr = console.error.bind(console);
  const origWarn = console.warn.bind(console);
  const push = (s: string) => {
    lines.push(s);
    if (lines.length > kMaxLines) lines.splice(0, lines.length - kMaxLines);
  };
  console.log = (...args: any[]) => {
    push(fmt(args));
    origLog(...args);
  };
  console.error = (...args: any[]) => {
    push("[ERR] " + fmt(args));
    origErr(...args);
  };
  console.warn = (...args: any[]) => {
    push("[WARN] " + fmt(args));
    origWarn(...args);
  };
}

export function getRecentLogs(n = 120): string[] {
  return lines.slice(-n);
}
