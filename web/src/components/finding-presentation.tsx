"use client";

import * as React from "react";

import { ChevronLeftIcon, ChevronRightIcon } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { getLocalStorageValue, setLocalStorageValue } from "@/lib/local-storage.client";
import { cn } from "@/lib/utils";

export function useFindingPresentation(key: string) {
  const [layout, setLayout] = React.useState<"flat" | "split">("flat");
  const [detailOpen, setDetailOpen] = React.useState(false);
  const [listCollapsed, setListCollapsed] = React.useState(false);
  const [detailCollapsed, setDetailCollapsed] = React.useState(false);
  const listKey = `${key}_list_collapsed`;
  const detailKey = `${key}_detail_collapsed`;
  React.useEffect(() => {
    setLayout(getLocalStorageValue(key) === "split" ? "split" : "flat");
    const list = getLocalStorageValue(listKey) === "1";
    const detail = getLocalStorageValue(detailKey) === "1";
    // Never restore both panels collapsed: there would be nothing left to read.
    setListCollapsed(list && !detail);
    setDetailCollapsed(detail && !list);
  }, [key, listKey, detailKey]);
  const changeLayout = (value: string) => {
    if (value !== "flat" && value !== "split") return;
    setLayout(value);
    setDetailOpen(false);
    setLocalStorageValue(key, value);
  };
  const toggleList = () => {
    const next = !listCollapsed;
    setListCollapsed(next);
    setLocalStorageValue(listKey, next ? "1" : "0");
    if (next) {
      setDetailCollapsed(false);
      setLocalStorageValue(detailKey, "0");
    }
  };
  const toggleDetail = () => {
    const next = !detailCollapsed;
    setDetailCollapsed(next);
    setLocalStorageValue(detailKey, next ? "1" : "0");
    if (next) {
      setListCollapsed(false);
      setLocalStorageValue(listKey, "0");
    }
  };
  return {
    layout,
    changeLayout,
    detailOpen,
    setDetailOpen,
    listCollapsed,
    detailCollapsed,
    toggleList,
    toggleDetail,
  };
}

export function FindingLayoutToggle({ value, onChange }: { value: string; onChange: (value: string) => void }) {
  return (
    <ToggleGroup
      type="single"
      variant="outline"
      size="sm"
      spacing={0}
      className="shrink-0"
      value={value}
      onValueChange={onChange}
      aria-label="漏洞展示方式"
    >
      <ToggleGroupItem value="flat">平铺</ToggleGroupItem>
      <ToggleGroupItem value="split">分栏</ToggleGroupItem>
    </ToggleGroup>
  );
}

function CollapsedRail({ side, label, onExpand }: { side: "left" | "right"; label: string; onExpand: () => void }) {
  const Icon = side === "left" ? ChevronRightIcon : ChevronLeftIcon;
  return (
    <aside
      className="findings-collapsed-rail hidden self-stretch flex-col items-center gap-3 rounded-xl border bg-card px-1 py-3 xl:flex"
      aria-label={`${label}已收起`}
    >
      <Button
        type="button"
        variant="ghost"
        size="icon"
        className="size-8"
        onClick={onExpand}
        aria-label={`展开${label}`}
        title={`展开${label}`}
      >
        <Icon className="size-4" />
      </Button>
      <span className="text-muted-foreground text-xs [writing-mode:vertical-rl] tracking-wide">{label}</span>
    </aside>
  );
}

export function FindingPresentation({
  layout,
  detailOpen,
  onOpenChange,
  detail,
  children,
  listCollapsed,
  detailCollapsed,
  onToggleList,
  onToggleDetail,
}: {
  layout: "flat" | "split";
  detailOpen: boolean;
  onOpenChange: (open: boolean) => void;
  detail: React.ReactNode;
  children: React.ReactNode;
  listCollapsed: boolean;
  detailCollapsed: boolean;
  onToggleList: () => void;
  onToggleDetail: () => void;
}) {
  const split = layout === "split";
  let splitGrid = "";
  if (split) {
    if (listCollapsed) splitGrid = "xl:grid-cols-[3.25rem_minmax(0,1fr)]";
    else if (detailCollapsed) splitGrid = "xl:grid-cols-[minmax(0,1fr)_3.25rem]";
    else splitGrid = "xl:grid-cols-[minmax(20rem,0.9fr)_minmax(0,2fr)]";
  }
  return (
    <div className="flex min-w-0 flex-col gap-2">
      {split && (
        <div className="hidden items-center justify-end gap-1 xl:flex" role="toolbar" aria-label="发现分栏控制">
          <Button
            type="button"
            variant="ghost"
            size="sm"
            className="h-7 gap-1 px-2 text-muted-foreground text-xs"
            onClick={onToggleList}
            aria-pressed={listCollapsed}
            aria-label={listCollapsed ? "展开发现列表" : "收起发现列表"}
            title={listCollapsed ? "展开发现列表" : "收起发现列表"}
          >
            {listCollapsed ? (
              <ChevronRightIcon className="size-3.5" data-icon="inline-start" />
            ) : (
              <ChevronLeftIcon className="size-3.5" data-icon="inline-start" />
            )}
            {listCollapsed ? "展开列表" : "收起列表"}
          </Button>
          <Button
            type="button"
            variant="ghost"
            size="sm"
            className="h-7 gap-1 px-2 text-muted-foreground text-xs"
            onClick={onToggleDetail}
            aria-pressed={detailCollapsed}
            aria-label={detailCollapsed ? "展开漏洞详情" : "收起漏洞详情"}
            title={detailCollapsed ? "展开漏洞详情" : "收起漏洞详情"}
          >
            {detailCollapsed ? (
              <ChevronLeftIcon className="size-3.5" data-icon="inline-start" />
            ) : (
              <ChevronRightIcon className="size-3.5" data-icon="inline-start" />
            )}
            {detailCollapsed ? "展开详情" : "收起详情"}
          </Button>
        </div>
      )}
      <div className={cn("grid min-w-0 items-start gap-4", splitGrid)}>
        {split ? (
          <>
            <div className={cn("min-w-0", listCollapsed && "xl:hidden")}>{children}</div>
            {listCollapsed && <CollapsedRail side="left" label="发现列表" onExpand={onToggleList} />}
            <div className={cn("min-w-0", detailCollapsed && "xl:hidden")}>{detail}</div>
            {detailCollapsed && <CollapsedRail side="right" label="漏洞详情" onExpand={onToggleDetail} />}
          </>
        ) : (
          <>
            {children}
            <Dialog open={detailOpen} onOpenChange={onOpenChange}>
              <DialogContent className="max-h-[90dvh] overflow-y-auto sm:max-w-5xl" aria-describedby={undefined}>
                <DialogHeader>
                  <DialogTitle>漏洞详情</DialogTitle>
                </DialogHeader>
                <div className="min-w-0">{detailOpen && detail}</div>
              </DialogContent>
            </Dialog>
          </>
        )}
      </div>
    </div>
  );
}
