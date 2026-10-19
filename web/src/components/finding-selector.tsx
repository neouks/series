"use client";

import Link from "next/link";

import { CircleAlertIcon, EllipsisIcon, ExternalLinkIcon, EyeIcon, SearchIcon } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty";
import { Item, ItemActions, ItemContent, ItemGroup } from "@/components/ui/item";
import { findingRowKey, fmtTime } from "@/lib/findings";
import { statusMeta } from "@/lib/status";
import type { Finding } from "@/lib/types";
import { cn } from "@/lib/utils";

export function FindingSelector({
  flat = false,
  items,
  selectedKey,
  onSelect,
  selectedIds,
  onToggleSelected,
  onToggleSelectedPage,
  selectAllLabel = "选择当前页全部",
  contextTask,
}: {
  flat?: boolean;
  items: Finding[];
  selectedKey: string | null;
  onSelect: (finding: Finding) => void;
  selectedIds?: Set<string>;
  onToggleSelected?: (id: string, checked: boolean) => void;
  onToggleSelectedPage?: (ids: string[], checked: boolean) => void;
  selectAllLabel?: string;
  contextTask?: string;
}) {
  const selectable = items.filter(
    (item): item is Finding & { finding_id: string } => !!item.finding_id && !item.inherited,
  );
  const checked = selectable.filter((item) => selectedIds?.has(item.finding_id)).length;
  return (
    <div className="finding-selector flex min-w-0 flex-col gap-3">
      {onToggleSelectedPage && items.length > 0 && (
        <div className="flex items-center gap-2 text-muted-foreground text-xs">
          <Checkbox
            aria-label={selectAllLabel}
            disabled={!selectable.length}
            checked={checked > 0 && checked < selectable.length ? "indeterminate" : checked > 0}
            onCheckedChange={(value) =>
              onToggleSelectedPage(
                selectable.map((item) => item.finding_id),
                value === true,
              )
            }
          />
          <span>{selectAllLabel}</span>
        </div>
      )}
      <ItemGroup className={cn("min-w-0", !flat && "max-h-[36rem] overflow-y-auto")} aria-label="漏洞选择列表">
        {items.map((item) => {
          const title = item.name || item.vulnclass || "未分类发现";
          const key = findingRowKey(item);
          return (
            <Item
              key={key}
              role="listitem"
              variant="outline"
              className="finding-list-row"
              data-selected={selectedKey === key || undefined}
            >
              {onToggleSelected && (
                <Checkbox
                  aria-label={`导出 ${title}`}
                  disabled={!item.finding_id || item.inherited}
                  checked={!!item.finding_id && !!selectedIds?.has(item.finding_id)}
                  onCheckedChange={(value) => item.finding_id && onToggleSelected(item.finding_id, value === true)}
                />
              )}
              <ItemContent className="min-w-0">
                <button
                  type="button"
                  className="finding-row-open"
                  onClick={() => onSelect(item)}
                  aria-label={`查看发现：${title}`}
                  aria-pressed={selectedKey === key}
                >
                  <span className="finding-row-title truncate" title={title}>
                    {title}
                  </span>
                  <span className="finding-row-meta">
                    <span className="finding-severity" data-severity={item.severity}>
                      <CircleAlertIcon aria-hidden="true" />
                      {statusMeta("severity", item.severity).label}
                    </span>
                    <span aria-hidden="true">·</span>
                    <span>{statusMeta("finding", item.status).label}</span>
                    <span aria-hidden="true">·</span>
                    <time dateTime={item.ts} title={fmtTime(item.ts)}>
                      {fmtTime(item.ts)}
                    </time>
                    {item.inherited && <Badge variant="outline">关联任务 · 只读</Badge>}
                  </span>
                  <span
                    className="finding-row-assets truncate"
                    title={item.assets?.map((asset) => asset.label).join("、")}
                  >
                    {item.assets?.map((asset) => asset.label).join(" · ") || "未关联资产"}
                  </span>
                </button>
              </ItemContent>
              <ItemActions>
                <DropdownMenu>
                  <DropdownMenuTrigger asChild>
                    <Button variant="ghost" size="icon-sm" aria-label={`更多操作：${title}`}>
                      <EllipsisIcon />
                    </Button>
                  </DropdownMenuTrigger>
                  <DropdownMenuContent align="end">
                    <DropdownMenuGroup>
                      <DropdownMenuItem onSelect={() => onSelect(item)}>
                        <EyeIcon />
                        查看详情
                      </DropdownMenuItem>
                      {item.finding_id && (
                        <DropdownMenuItem asChild>
                          <Link
                            href={`/function/findings/detail?id=${encodeURIComponent(item.finding_id)}${contextTask ? `&context_task=${encodeURIComponent(contextTask)}` : ""}`}
                          >
                            <ExternalLinkIcon />
                            独立页面查看
                          </Link>
                        </DropdownMenuItem>
                      )}
                    </DropdownMenuGroup>
                  </DropdownMenuContent>
                </DropdownMenu>
              </ItemActions>
            </Item>
          );
        })}
      </ItemGroup>
      {!items.length && (
        <Empty className="min-h-52">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <SearchIcon />
            </EmptyMedia>
            <EmptyTitle>没有匹配的发现</EmptyTitle>
            <EmptyDescription>尝试更换关键词或清除筛选条件。</EmptyDescription>
          </EmptyHeader>
        </Empty>
      )}
    </div>
  );
}
