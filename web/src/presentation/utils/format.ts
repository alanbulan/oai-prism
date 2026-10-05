/**
 * 展示层格式化工具：耗时、运行时长、时间戳、客户端标识。
 * 只做显示转换，不改变数据本身；原始值在详情里另行展示，便于排障核对。
 */

const pad = (n: number) => String(n).padStart(2, '0');

/** 毫秒耗时 → 人类可读：8 ms / 1.25 s / 16.2 s / 4 分 14 秒 / 1 小时 5 分 */
export function formatDuration(ms: number | null | undefined): string {
  if (ms == null || !Number.isFinite(ms) || ms < 0) return '-';
  if (ms < 1000) return `${Math.round(ms)} ms`;
  if (ms < 10_000) return `${(ms / 1000).toFixed(2)} s`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)} s`;
  const totalSec = Math.round(ms / 1000);
  if (totalSec < 3600) {
    const m = Math.floor(totalSec / 60);
    const s = totalSec % 60;
    return s ? `${m} 分 ${s} 秒` : `${m} 分钟`;
  }
  const h = Math.floor(totalSec / 3600);
  const m = Math.floor((totalSec % 3600) / 60);
  return m ? `${h} 小时 ${m} 分` : `${h} 小时`;
}

/** token 数紧凑显示：856 / 12.3k / 4.56M（完整数字放在 Tooltip 里） */
export function formatTokens(n: number | null | undefined): string {
  if (n == null || !Number.isFinite(n)) return '-';
  if (n < 1000) return String(Math.round(n));
  if (n < 1_000_000) return `${(n / 1000).toFixed(n < 10_000 ? 2 : 1).replace(/\.?0+$/, '')}k`;
  return `${(n / 1_000_000).toFixed(2).replace(/\.?0+$/, '')}M`;
}

/** 秒级运行时长：42 秒 / 9 分 53 秒 / 3 小时 12 分 / 2 天 4 小时 */
export function formatUptime(sec: number | null | undefined): string {
  if (!sec || sec < 0) return '0 秒';
  const s = Math.floor(sec);
  if (s < 60) return `${s} 秒`;
  if (s < 3600) return `${Math.floor(s / 60)} 分 ${s % 60} 秒`;
  if (s < 86400) return `${Math.floor(s / 3600)} 小时 ${Math.floor((s % 3600) / 60)} 分`;
  return `${Math.floor(s / 86400)} 天 ${Math.floor((s % 86400) / 3600)} 小时`;
}

function toDate(ts: string | number | Date | null | undefined): Date | null {
  if (ts == null || ts === '') return null;
  const d = ts instanceof Date ? ts : new Date(ts);
  // Go 的零值时间（0001-01-01）表示"未知"，不当作真实日期展示
  return Number.isNaN(d.getTime()) || d.getUTCFullYear() <= 1 ? null : d;
}

/** 2026-10-03 22:27:02（本地时区，24 小时制） */
export function formatDateTime(ts: string | number | Date | null | undefined): string {
  const d = toDate(ts);
  if (!d) return '-';
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

/** 2026-10-03 */
export function formatDate(ts: string | number | Date | null | undefined): string {
  const d = toDate(ts);
  if (!d) return '-';
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

/** 列表用的紧凑时间：10-03 22:27:02（跨年时带年份） */
export function formatCompactTime(ts: string | number | Date | null | undefined): string {
  const d = toDate(ts);
  if (!d) return '-';
  const time = `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
  const md = `${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
  return d.getFullYear() === new Date().getFullYear() ? `${md} ${time}` : `${d.getFullYear()}-${md} ${time}`;
}

/** 整点小时桶的轴标签：22:00 */
export function formatHour(ts: string | number | Date | null | undefined): string {
  const d = toDate(ts);
  return d ? `${pad(d.getHours())}:00` : '';
}

/** 相对时间：刚刚 / 3 分钟前 / 2 小时前 / 5 天前，超过 30 天回落为日期 */
export function formatRelative(ts: string | number | Date | null | undefined): string {
  const d = toDate(ts);
  if (!d) return '-';
  const diff = Math.round((Date.now() - d.getTime()) / 1000);
  if (diff < 0) return formatDateTime(d);
  if (diff < 45) return '刚刚';
  if (diff < 3600) return `${Math.max(1, Math.round(diff / 60))} 分钟前`;
  if (diff < 86400) return `${Math.floor(diff / 3600)} 小时前`;
  if (diff < 30 * 86400) return `${Math.floor(diff / 86400)} 天前`;
  return formatDate(d);
}

/**
 * 客户端地址规整：早期版本记录的是 RemoteAddr（带端口），
 * 这里统一剥掉端口与 IPv6 方括号，并标注回环地址。
 */
export function normalizeIp(raw: string | null | undefined): { ip: string; local: boolean } {
  let ip = (raw || '').trim();
  const bracket = /^\[([^\]]+)\](?::\d+)?$/.exec(ip);
  if (bracket) ip = bracket[1];
  else if (/^[\d.]+:\d+$/.test(ip)) ip = ip.slice(0, ip.lastIndexOf(':'));
  const local = ip === '::1' || ip.startsWith('127.') || ip === 'localhost';
  return { ip: ip || '-', local };
}

export interface ClientInfo {
  /** 客户端名称，如 Codex TUI、OpenAI Python（SDK）、curl */
  name: string;
  version?: string;
  kind: 'codex' | 'sdk' | 'cli' | 'browser' | 'other';
}

/** 从 User-Agent 识别调用方，列表里只显示名称与版本，完整 UA 留给详情 */
export function parseClient(ua: string | null | undefined): ClientInfo {
  const s = (ua || '').trim();
  if (!s) return { name: '未知客户端', kind: 'other' };

  const codex = /^codex[-_]?([a-z]+)?[^/\s]*\/([\w.-]+)/i.exec(s);
  if (codex) {
    const flavor = (codex[1] || '').toLowerCase();
    const name = flavor === 'tui' ? 'Codex TUI' : flavor === 'exec' ? 'Codex exec' : 'Codex CLI';
    return { name, version: codex[2], kind: 'codex' };
  }
  const sdk = /^(OpenAI|Anthropic)\/(Python|JS|Node|Go|Java)\s+([\w.-]+)/i.exec(s);
  if (sdk) {
    const lang = /^js$/i.test(sdk[2]) ? 'Node' : sdk[2];
    return { name: `${sdk[1]} ${lang}`, version: sdk[3], kind: 'sdk' };
  }
  const tools: [RegExp, string, ClientInfo['kind']][] = [
    [/^claude-cli\/([\w.-]+)/i, 'Claude Code', 'cli'],
    [/^curl\/([\w.-]+)/i, 'curl', 'cli'],
    [/^Python-urllib\/([\w.-]+)/i, 'Python urllib', 'sdk'],
    [/^python-requests\/([\w.-]+)/i, 'Python requests', 'sdk'],
    [/^python-httpx\/([\w.-]+)/i, 'Python httpx', 'sdk'],
    [/^axios\/([\w.-]+)/i, 'axios', 'sdk'],
    [/^node(?:-fetch)?(?:\/([\w.-]+))?$/i, 'Node.js', 'sdk'],
    [/^Go-http-client\/([\w.-]+)/i, 'Go net/http', 'sdk'],
    [/^PostmanRuntime\/([\w.-]+)/i, 'Postman', 'cli'],
  ];
  for (const [re, name, kind] of tools) {
    const m = re.exec(s);
    if (m) return { name, version: m[1], kind };
  }
  if (/^Mozilla\//.test(s)) {
    const browsers: [RegExp, string][] = [
      [/HeadlessChrome\/([\d.]+)/, 'Headless Chrome'],
      [/Edg\/([\d.]+)/, 'Edge'],
      [/Firefox\/([\d.]+)/, 'Firefox'],
      [/Chrome\/([\d.]+)/, 'Chrome'],
      [/Version\/([\d.]+).*Safari/, 'Safari'],
    ];
    for (const [re, name] of browsers) {
      const m = re.exec(s);
      if (m) return { name, version: m[1].split('.')[0], kind: 'browser' };
    }
    // 内嵌 WebView 的桌面应用：UA 末尾通常是应用名（如 "… like Gecko) WorkBuddy"）
    const tail = /\)\s*([A-Za-z][\w.-]*)(?:\/([\w.-]+))?\s*$/.exec(s);
    if (tail) return { name: tail[1], version: tail[2], kind: 'browser' };
    return { name: '浏览器', kind: 'browser' };
  }
  const generic = /^([A-Za-z][\w.-]*)\/([\w.-]+)/.exec(s);
  if (generic) return { name: generic[1], version: generic[2], kind: 'other' };
  return { name: s.length > 24 ? `${s.slice(0, 24)}…` : s, kind: 'other' };
}
