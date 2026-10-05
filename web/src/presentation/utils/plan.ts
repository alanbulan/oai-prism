/**
 * 上游订阅计划的名称与排序。
 *
 * 计划取自账号令牌里的 chatgpt_plan_type（free / plus / prolite / pro / team …），
 * 筛选项、表格标签、编辑下拉都从这里取，不再各写一份固定列表 ——
 * 早先筛选只有 Pro / Team / Free，导入的 Plus、Pro Lite 账号怎么选都筛不出来。
 */

const PLANS: { key: string; label: string }[] = [
  { key: 'free', label: 'Free' },
  { key: 'go', label: 'Go' },
  { key: 'plus', label: 'Plus' },
  { key: 'prolite', label: 'Pro Lite' },
  { key: 'pro', label: 'Pro' },
  { key: 'team', label: 'Team' },
  { key: 'business', label: 'Business' },
  { key: 'enterprise', label: 'Enterprise' },
  { key: 'edu', label: 'Edu' },
];

/** 计划的归一键：小写、去掉空格与连字符（Pro Lite / pro_lite → prolite） */
export function planKey(plan: string | undefined | null): string {
  return (plan || '').toLowerCase().replace(/[\s_-]+/g, '');
}

/** 计划的显示名（未知计划原样显示，没有计划时为"未知"） */
export function planLabel(plan: string | undefined | null): string {
  const k = planKey(plan);
  if (!k) return '未知';
  return PLANS.find((p) => p.key === k)?.label ?? (plan as string);
}

/** 按计划高低排序用的序号（未知计划排在最后） */
export function planRank(plan: string | undefined | null): number {
  const i = PLANS.findIndex((p) => p.key === planKey(plan));
  return i < 0 ? PLANS.length : i;
}

/** 标签颜色：团队 / 企业金色，Pro 系蓝色，Plus 青色，其余默认 */
export function planColor(plan: string | undefined | null): string {
  const k = planKey(plan);
  if (k === 'team' || k === 'business' || k === 'enterprise' || k === 'edu') return 'gold';
  if (k === 'pro' || k === 'prolite') return 'blue';
  if (k === 'plus') return 'cyan';
  return 'default';
}

/** 编辑账号时可选的计划（当前值不在其中时一并列出） */
export function planOptions(current?: string): { value: string; label: string }[] {
  const opts = PLANS.filter((p) => ['free', 'plus', 'prolite', 'pro', 'team', 'enterprise'].includes(p.key)).map((p) => ({
    value: p.key,
    label: `${p.label} 计划`,
  }));
  const k = planKey(current);
  if (k && !opts.some((o) => o.value === k)) opts.push({ value: k, label: `${planLabel(current)} 计划` });
  return opts;
}
