/**
 * 会话保持：登录成功后的 MiNA/MiIOT 实例，供
 * “授权登录”注入给 Speaker、以及“设备可达性测试”复用。
 */
export interface XiaomiSession {
  miNA: any;
  miIOT: any;
  at: number;
}

let session: XiaomiSession | null = null;

export const SessionStore = {
  get: () => session,
  set(s: XiaomiSession | null) {
    session = s;
  },
  clear() {
    session = null;
  },
};
