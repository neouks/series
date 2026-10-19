"use client";

import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Select, SelectContent, SelectGroup, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { fontOptions, type FontKey } from "@/lib/fonts/registry";
import { applyFont } from "@/lib/preferences/layout-utils";
import { persistPreference } from "@/lib/preferences/preferences-storage";
import { usePreferencesStore } from "@/stores/preferences/preferences-provider";

export default function AppearanceSettings() {
  const mode = usePreferencesStore((s) => s.themeMode);
  const setMode = usePreferencesStore((s) => s.setThemeMode);
  const font = usePreferencesStore((s) => s.font);
  const setFont = usePreferencesStore((s) => s.setFont);
  return (
    <FieldGroup className="max-w-xl">
      <Field>
        <FieldLabel>颜色模式</FieldLabel>
        <FieldDescription>选择浅色、深色，或跟随系统。修改即时生效。</FieldDescription>
        <ToggleGroup
          type="single"
          variant="outline"
          value={mode}
          onValueChange={(v) => {
            if (v !== "light" && v !== "dark" && v !== "system") return;
            setMode(v);
            void persistPreference("theme_mode", v);
          }}
          aria-label="颜色模式"
        >
          <ToggleGroupItem value="light">浅色</ToggleGroupItem>
          <ToggleGroupItem value="dark">深色</ToggleGroupItem>
          <ToggleGroupItem value="system">跟随系统</ToggleGroupItem>
        </ToggleGroup>
      </Field>
      <Field>
        <FieldLabel htmlFor="appearance-font">界面字体</FieldLabel>
        <Select
          value={font}
          onValueChange={(v) => {
            setFont(v as FontKey);
            applyFont(v);
            void persistPreference("font", v as FontKey);
          }}
        >
          <SelectTrigger id="appearance-font">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectGroup>
              {fontOptions.map((f) => (
                <SelectItem key={f.key} value={f.key}>
                  {f.label}
                </SelectItem>
              ))}
            </SelectGroup>
          </SelectContent>
        </Select>
        <FieldDescription>使用本地字体资源；侧栏可通过折叠按钮收起。</FieldDescription>
      </Field>
    </FieldGroup>
  );
}
