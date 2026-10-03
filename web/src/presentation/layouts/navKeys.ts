export const NAV_KEYS = ['accounts', 'statistics', 'chat'] as const;
export type NavKey = (typeof NAV_KEYS)[number];
