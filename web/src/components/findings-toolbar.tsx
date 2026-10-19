"use client";

import type { ReactNode } from "react";

import { ListFilterIcon, SearchIcon, XIcon } from "lucide-react";

import { Button } from "@/components/ui/button";
import { FieldGroup } from "@/components/ui/field";
import { InputGroup, InputGroupAddon, InputGroupInput } from "@/components/ui/input-group";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Select, SelectContent, SelectGroup, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { FINDING_STATUSES, SEVERITIES } from "@/lib/findings";
import { statusMeta } from "@/lib/status";

export function FindingFilterSelect({
  label,
  value,
  onChange,
  options,
  sort = false,
}: {
  label: string;
  value: string;
  onChange: (value: string) => void;
  options: { value: string; label: string }[];
  sort?: boolean;
}) {
  return (
    <Select value={value} onValueChange={onChange}>
      <SelectTrigger data-findings-control={sort ? "sort" : "filter"} aria-label={label}>
        <SelectValue />
      </SelectTrigger>
      <SelectContent position="popper">
        <SelectGroup>
          {options.map((option) => (
            <SelectItem key={option.value} value={option.value}>
              {option.label}
            </SelectItem>
          ))}
        </SelectGroup>
      </SelectContent>
    </Select>
  );
}

export function FindingsToolbar({
  total,
  search,
  onSearch,
  sort,
  onSort,
  sortOptions,
  view,
  onView,
  viewOptions,
  status,
  onStatus,
  severity,
  onSeverity,
  filters,
  activeFilters = 0,
  onReset,
  actions,
}: {
  total?: number;
  search: string;
  onSearch: (value: string) => void;
  sort: string;
  onSort: (value: string) => void;
  sortOptions: { value: string; label: string }[];
  view: string;
  onView: (value: string) => void;
  viewOptions: { value: string; label: string }[];
  status: string;
  onStatus: (value: string) => void;
  severity: string;
  onSeverity: (value: string) => void;
  filters: ReactNode;
  activeFilters?: number;
  onReset: () => void;
  actions?: ReactNode;
}) {
  return (
    <header className="findings-toolbar">
      <div className="flex flex-wrap items-baseline gap-2.5">
        <h2 className="findings-heading">全部发现</h2>
        <span className="text-muted-foreground tabular-nums" aria-live="polite">
          {total === undefined ? "正在加载…" : `${total} 条发现`}
        </span>
      </div>
      <div className="findings-search-row">
        <InputGroup data-findings-control="search">
          <InputGroupInput
            type="search"
            maxLength={200}
            value={search}
            onChange={(event) => onSearch(event.target.value)}
            placeholder="搜索发现"
            aria-label="搜索发现"
          />
          <InputGroupAddon>
            <SearchIcon aria-hidden="true" />
          </InputGroupAddon>
        </InputGroup>
        <FindingFilterSelect label="发现排序" sort value={sort} onChange={onSort} options={sortOptions} />
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <FindingFilterSelect label="发现范围" value={view} onChange={onView} options={viewOptions} />
        <FindingFilterSelect
          label="按状态筛选"
          value={status}
          onChange={onStatus}
          options={[
            { value: "all", label: "状态" },
            ...FINDING_STATUSES.map((value) => ({ value, label: statusMeta("finding", value).label })),
          ]}
        />
        <FindingFilterSelect
          label="按严重度筛选"
          value={severity}
          onChange={onSeverity}
          options={[
            { value: "all", label: "严重度" },
            ...SEVERITIES.map((value) => ({ value, label: statusMeta("severity", value).label })),
          ]}
        />
        <Popover>
          <PopoverTrigger asChild>
            <Button variant="outline" data-findings-control="filter">
              <ListFilterIcon data-icon="inline-start" />
              筛选{activeFilters > 0 && <span className="tabular-nums">{activeFilters}</span>}
            </Button>
          </PopoverTrigger>
          <PopoverContent align="start" className="w-80 max-w-[calc(100vw-2rem)]">
            <FieldGroup>{filters}</FieldGroup>
          </PopoverContent>
        </Popover>
        {(activeFilters > 0 || search) && (
          <Button variant="ghost" onClick={onReset} size="sm">
            <XIcon data-icon="inline-start" />
            清除筛选
          </Button>
        )}
      </div>
      {actions && <div className="findings-actions">{actions}</div>}
    </header>
  );
}
