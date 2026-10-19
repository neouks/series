"use client";

import type { SettingsTab } from "@/lib/workspace-navigation";
import * as React from "react";

export const SettingsContext = React.createContext<{
  open: (tab?: SettingsTab) => void;
  registerDirty: (id: string, dirty: boolean) => void;
} | null>(null);

export const SettingsDraftContext = React.createContext<((id: string, dirty: boolean) => void) | null>(null);

export function useSettings() {
  const context = React.useContext(SettingsContext);
  if (!context) throw new Error("Missing settings provider");
  return context;
}

/** Editors also run on their original standalone routes, where no dialog guard is needed. */
export function useSettingsDirty(dirty: boolean) {
  const register = React.useContext(SettingsDraftContext);
  const id = React.useId();
  React.useEffect(() => {
    register?.(id, dirty);
    return () => register?.(id, false);
  }, [register, id, dirty]);
}

/** A successful load/save increments revision in the same batch as its field updates. */
export function useSettingsDraft(value: unknown, revision: number, enabled = true) {
  const serialized = JSON.stringify(value);
  const [saved, setSaved] = React.useState({ revision, serialized });
  React.useEffect(() => {
    if (saved.revision !== revision) setSaved({ revision, serialized });
  }, [revision, serialized, saved.revision]);
  useSettingsDirty(enabled && saved.revision === revision && saved.serialized !== serialized);
}
