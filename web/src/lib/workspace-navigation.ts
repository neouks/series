/** Shared route/shortcut policy. Keep source-location and search query parameters intact. */
export const NEW_TASK_PATH = "/function/tasks/new";
export const SETTINGS_TABS = ["llm", "agents", "mcp", "skills", "tools", "intercept", "general", "appearance"] as const;
export type SettingsTab = (typeof SETTINGS_TABS)[number];

export function settingsTab(value: string | null): SettingsTab {
  return SETTINGS_TABS.includes(value as SettingsTab) ? (value as SettingsTab) : "general";
}

export function settingsURL(pathname: string, search: string, tab: SettingsTab | null): string {
  const params = new URLSearchParams(search);
  if (tab) {
    params.set("settings", "1");
    params.set("settingsTab", tab);
  } else {
    params.delete("settings");
    params.delete("settingsTab");
  }
  const query = params.toString();
  return `${pathname}${query ? `?${query}` : ""}`;
}

export function workspaceShortcut(
  event: {
    key: string;
    metaKey: boolean;
    ctrlKey: boolean;
    altKey: boolean;
    shiftKey: boolean;
    isComposing: boolean;
    defaultPrevented: boolean;
  },
  blocked: boolean,
): "new-task" | null {
  if (
    blocked ||
    event.defaultPrevented ||
    event.isComposing ||
    event.altKey ||
    event.shiftKey ||
    !(event.metaKey || event.ctrlKey)
  )
    return null;
  if (event.key.toLowerCase() === "j") return "new-task";
  return null;
}
