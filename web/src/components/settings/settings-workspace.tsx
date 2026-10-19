"use client";

import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Select, SelectContent, SelectGroup, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";
import { settingsTab, settingsURL, type SettingsTab } from "@/lib/workspace-navigation";
import { Bot, Brain, Palette, Plug, Settings2, Shield, Sparkles, Wrench } from "lucide-react";
import dynamic from "next/dynamic";
import { usePathname, useSearchParams } from "next/navigation";
import * as React from "react";
import { SettingsContext, SettingsDraftContext } from "./settings-context";

function Loading() {
  return (
    <div role="status" aria-label="加载设置" className="flex flex-col gap-4">
      <Skeleton className="h-9 w-1/2" />
      <Skeleton className="h-48 w-full" />
    </div>
  );
}
const panels = {
  llm: dynamic(() => import("@/app/(main)/system/llm/_components/llm-settings"), { loading: Loading }),
  agents: dynamic(() => import("@/app/(main)/system/agents/_components/agents-settings"), { loading: Loading }),
  mcp: dynamic(() => import("@/app/(main)/system/mcp/_components/mcp-settings"), { loading: Loading }),
  skills: dynamic(() => import("@/app/(main)/system/skills/_components/skills-settings"), { loading: Loading }),
  tools: dynamic(() => import("@/app/(main)/system/tools/_components/tools-settings"), { loading: Loading }),
  intercept: dynamic(() => import("@/app/(main)/system/intercept/_components/intercept-settings"), {
    loading: Loading,
  }),
  general: dynamic(() => import("@/app/(main)/system/settings/_components/general-settings"), { loading: Loading }),
  appearance: dynamic(() => import("./appearance-settings"), { loading: Loading }),
};
export const settingsSections = [
  { id: "llm", label: "模型", icon: Brain },
  { id: "agents", label: "Agent", icon: Bot },
  { id: "mcp", label: "MCP", icon: Plug },
  { id: "skills", label: "Skill", icon: Sparkles },
  { id: "tools", label: "自定义工具", icon: Wrench },
  { id: "intercept", label: "拦截规则", icon: Shield },
  { id: "general", label: "系统配置", icon: Settings2 },
  { id: "appearance", label: "外观", icon: Palette },
] satisfies { id: SettingsTab; label: string; icon: typeof Brain }[];

export function SettingsWorkspace({ children }: { children: React.ReactNode }) {
  const pathname = usePathname();
  const search = useSearchParams();
  const [active, setActive] = React.useState<SettingsTab | null>(null);
  const activeRef = React.useRef<SettingsTab | null>(null);
  const dirty = React.useRef(new Set<string>());
  const opener = React.useRef<HTMLElement | null>(null);
  const [pending, setPending] = React.useState<{ run: () => void; cancel?: () => void } | null>(null);
  const lastURL = React.useRef("");
  const registerDirty = React.useCallback((id: string, value: boolean) => {
    if (value) dirty.current.add(id);
    else dirty.current.delete(id);
  }, []);
  const apply = React.useCallback((tab: SettingsTab | null) => {
    dirty.current.clear();
    activeRef.current = tab;
    setActive(tab);
  }, []);
  const guard = React.useCallback((run: () => void, cancel?: () => void) => {
    if (dirty.current.size) setPending({ run, cancel });
    else run();
  }, []);
  const navigate = React.useCallback(
    (tab: SettingsTab | null) => {
      if (tab === activeRef.current) return;
      guard(() => {
        const url = settingsURL(window.location.pathname, window.location.search, tab) + window.location.hash;
        if (!activeRef.current && tab) {
          opener.current = document.activeElement as HTMLElement;
          window.history.pushState(null, "", url);
        } else window.history.replaceState(null, "", url);
        lastURL.current = url;
        apply(tab);
      });
    },
    [apply, guard],
  );
  const open = React.useCallback((tab: SettingsTab = "general") => navigate(tab), [navigate]);
  React.useEffect(() => {
    const next = search.get("settings") === "1" ? settingsTab(search.get("settingsTab")) : null;
    const url = window.location.pathname + window.location.search + window.location.hash;
    if (next === activeRef.current) {
      lastURL.current = url;
      return;
    }
    const previous = lastURL.current;
    guard(
      () => {
        apply(next);
        lastURL.current = url;
      },
      () => {
        if (previous) window.history.pushState(null, "", previous);
      },
    );
  }, [pathname, search, apply, guard]);
  React.useEffect(() => {
    const preventLoss = (event: BeforeUnloadEvent) => {
      if (dirty.current.size) {
        event.preventDefault();
        event.returnValue = "";
      }
    };
    window.addEventListener("beforeunload", preventLoss);
    return () => window.removeEventListener("beforeunload", preventLoss);
  }, []);
  const context = React.useMemo(() => ({ open, registerDirty }), [open, registerDirty]);
  const Panel = active ? panels[active] : null;
  const title = settingsSections.find((s) => s.id === active)?.label ?? "设置";
  return (
    <SettingsContext.Provider value={context}>
      {children}
      <Dialog
        open={active !== null}
        onOpenChange={(value) => {
          if (!value) navigate(null);
        }}
      >
        <DialogContent
          className="flex h-[min(800px,calc(100dvh-48px))] w-[calc(100vw-48px)] max-w-[1120px]! gap-0 overflow-hidden p-0 max-h-[calc(100dvh-48px)] max-md:h-dvh max-md:max-h-dvh max-md:w-screen max-md:max-w-none! max-md:rounded-none"
          onCloseAutoFocus={(event) => {
            event.preventDefault();
            if (opener.current?.isConnected) opener.current.focus();
          }}
        >
          <nav aria-label="设置栏目" className="hidden w-52 shrink-0 flex-col gap-1 border-r bg-muted/30 p-4 md:flex">
            <p className="px-3 pb-4 pt-2 text-xs font-medium text-muted-foreground">工作空间设置</p>
            {settingsSections.map(({ id, label, icon: Icon }) => (
              <Button
                key={id}
                variant={id === active ? "secondary" : "ghost"}
                className="justify-start"
                onClick={() => navigate(id)}
                aria-current={id === active ? "page" : undefined}
              >
                <Icon data-icon="inline-start" />
                {label}
              </Button>
            ))}
          </nav>
          <div className="flex min-w-0 flex-1 flex-col">
            <DialogHeader className="shrink-0 border-b px-6 py-5 pr-12">
              <DialogTitle>{title}</DialogTitle>
              <DialogDescription className="sr-only">
                管理 SERIES 工作空间配置，各项沿用独立保存方式。
              </DialogDescription>
              <div className="md:hidden">
                <Select value={active ?? "general"} onValueChange={(v) => navigate(settingsTab(v))}>
                  <SelectTrigger aria-label="设置栏目">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectGroup>
                      {settingsSections.map((s) => (
                        <SelectItem key={s.id} value={s.id}>
                          {s.label}
                        </SelectItem>
                      ))}
                    </SelectGroup>
                  </SelectContent>
                </Select>
              </div>
            </DialogHeader>
            <div className={cn("settings-panel workspace-page min-h-0 flex-1 overflow-y-auto overscroll-contain")}>
              {Panel && (
                <SettingsDraftContext.Provider value={registerDirty}>
                  <Panel key={active} />
                </SettingsDraftContext.Provider>
              )}
            </div>
          </div>
        </DialogContent>
      </Dialog>
      <AlertDialog
        open={!!pending}
        onOpenChange={(value) => {
          if (!value) {
            pending?.cancel?.();
            setPending(null);
          }
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>放弃未保存的修改？</AlertDialogTitle>
            <AlertDialogDescription>当前栏目的修改尚未保存。继续切换或关闭会丢弃这些修改。</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>继续编辑</AlertDialogCancel>
            <AlertDialogAction
              onClick={(event) => {
                event.preventDefault();
                const action = pending;
                setPending(null);
                action?.run();
              }}
            >
              放弃修改
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </SettingsContext.Provider>
  );
}
