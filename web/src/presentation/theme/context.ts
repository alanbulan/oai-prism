import { createContext, useContext } from 'react';

export type ThemeMode = 'light' | 'dark' | 'system';

export interface ThemeContextValue {
  mode: ThemeMode;
  isDark: boolean;
  setMode: (m: ThemeMode) => void;
}

export const ThemeContext = createContext<ThemeContextValue>({
  mode: 'system',
  isDark: false,
  setMode: () => {},
});

export const useThemeMode = () => useContext(ThemeContext);
