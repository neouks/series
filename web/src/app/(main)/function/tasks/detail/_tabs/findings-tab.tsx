"use client";

import * as React from "react";

import { DownloadIcon } from "lucide-react";
import { toast } from "sonner";

import { FindingWorkspaceDetail } from "@/components/finding-detail-content";
import { FindingPagination } from "@/components/finding-pagination";
import { FindingLayoutToggle, FindingPresentation, useFindingPresentation } from "@/components/finding-presentation";
import { FindingSelector } from "@/components/finding-selector";
import { FindingSuppressionRules } from "@/components/finding-suppression-rules";
import { FindingsToolbar } from "@/components/findings-toolbar";
import { useNotificationRead } from "@/components/task-notifications";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Spinner } from "@/components/ui/spinner";
import { api } from "@/lib/api";
import { findingRowKey, selectFinding } from "@/lib/findings";
import { useStoredSortPreference } from "@/lib/sort-preference";
import type { FindingsPage, TaskFindingFilters } from "@/lib/types";

const FIELDS = ["time"] as const;
export function FindingsTab({ taskId }: { taskId: string }) {
  return <TaskFindings key={taskId} taskId={taskId} />;
}
function TaskFindings({ taskId }: { taskId: string }) {
  const presentation = useFindingPresentation("series_task_findings_layout");
  const beginRead = useNotificationRead("findings");
  const [sort, setSort] = useStoredSortPreference("series_task_findings_sort", FIELDS, "time", "desc");
  const [page, setPage] = React.useState(1);
  const [pageSize, setPageSize] = React.useState(50);
  const [filters, setFilters] = React.useState<TaskFindingFilters>({
    scope: "all",
    status: "all",
    severity: "all",
    query: "",
  });
  const [search, setSearch] = React.useState("");
  const [data, setData] = React.useState<FindingsPage | null>(null);
  const [selectedKey, setSelectedKey] = React.useState<string | null>(null);
  const [selectedIds, setSelectedIds] = React.useState<Set<string>>(new Set());
  const [exporting, setExporting] = React.useState(false);
  const [error, setError] = React.useState("");
  const [revision, refresh] = React.useReducer((n: number) => n + 1, 0);
  const changeFilters = React.useCallback((value: Partial<TaskFindingFilters>) => {
    setFilters((current) => ({ ...current, ...value }));
    setData(null);
    setPage(1);
  }, []);
  React.useEffect(() => {
    if (search.trim() === filters.query) return;
    const timer = setTimeout(() => changeFilters({ query: search.trim() }), 300);
    return () => clearTimeout(timer);
  }, [search, filters.query, changeFilters]);
  React.useEffect(() => {
    void revision;
    let active = true;
    let timer: ReturnType<typeof setTimeout>;
    async function load() {
      const markRead = beginRead();
      try {
        const value = await api.taskFindingsPage(taskId, page, pageSize, sort.direction, filters);
        if (!active) return;
        const lastPage = Math.max(1, Math.ceil(value.total / pageSize));
        if (page > lastPage) {
          setPage(lastPage);
          return;
        }
        setData(value);
        setSelectedKey((key) => {
          const selected = selectFinding(value.items, key);
          return selected ? findingRowKey(selected) : null;
        });
        setError("");
        markRead();
      } catch (e) {
        if (active) setError((e as Error).message);
      } finally {
        if (active) timer = setTimeout(() => void load(), 3000);
      }
    }
    void load();
    return () => {
      active = false;
      clearTimeout(timer);
    };
  }, [taskId, page, pageSize, sort.direction, filters, beginRead, revision]);
  const selected = data ? selectFinding(data.items, selectedKey) : null;
  function toggle(ids: string[], checked: boolean) {
    setSelectedIds((current) => {
      const next = new Set(current);
      for (const id of ids) {
        if (checked) next.add(id);
        else next.delete(id);
      }
      return next;
    });
  }
  async function exportSelected() {
    if (exporting || !selectedIds.size) return;
    setExporting(true);
    try {
      await api.exportFindings({ format: "md-single", scope: "selected", ids: [...selectedIds] });
      toast.success("已开始下载导出文件");
    } catch (e) {
      toast.error(`导出失败：${(e as Error).message}`);
    } finally {
      setExporting(false);
    }
  }
  return (
    <section className="findings-workspace findings-task-workspace" data-findings-surface aria-label="任务发现">
      <FindingsToolbar
        total={data?.total}
        search={search}
        onSearch={setSearch}
        sort={sort.direction}
        onSort={(direction) => {
          setData(null);
          setPage(1);
          setSort({ field: "time", direction: direction as "asc" | "desc" });
        }}
        sortOptions={[
          { value: "desc", label: "最近发现" },
          { value: "asc", label: "最早发现" },
        ]}
        view={filters.scope ?? "all"}
        onView={(scope) => changeFilters({ scope: scope as TaskFindingFilters["scope"] })}
        viewOptions={[
          { value: "all", label: "全部发现" },
          { value: "local", label: "当前任务" },
          { value: "inherited", label: "关联任务" },
        ]}
        status={filters.status ?? "all"}
        onStatus={(status) => changeFilters({ status: status as TaskFindingFilters["status"] })}
        severity={filters.severity ?? "all"}
        onSeverity={(severity) => changeFilters({ severity: severity as TaskFindingFilters["severity"] })}
        activeFilters={[filters.status, filters.severity, filters.scope].filter((v) => v && v !== "all").length}
        onReset={() => {
          setSearch("");
          changeFilters({ query: "", severity: "all", status: "all", scope: "all" });
        }}
        filters={
          <Field>
            <FieldLabel>详情展示</FieldLabel>
            <FindingLayoutToggle value={presentation.layout} onChange={presentation.changeLayout} />
            <FieldDescription>关联任务的发现保持只读。</FieldDescription>
          </Field>
        }
        actions={
          <>
            <FindingSuppressionRules taskId={taskId} readOnly={filters.scope === "inherited"} />
            <Button
              variant="ghost"
              size="icon"
              aria-label="导出选中发现"
              title="选中发现后导出"
              onClick={() => void exportSelected()}
              disabled={exporting || selectedIds.size === 0}
            >
              {exporting ? <Spinner /> : <DownloadIcon />}
            </Button>
            {selectedIds.size > 0 && <span className="text-muted-foreground text-xs">已选 {selectedIds.size} 条</span>}
          </>
        }
      />
      <FindingPresentation
        {...presentation}
        onToggleList={presentation.toggleList}
        onToggleDetail={presentation.toggleDetail}
        onOpenChange={presentation.setDetailOpen}
        detail={
          selected ? (
            <FindingWorkspaceDetail
              finding={selected}
              contextTask={taskId}
              onChanged={refresh}
              onDelete={async (finding, reason) => {
                if (!finding.finding_id) return;
                await api.deleteFinding(finding.finding_id, reason);
                toggle([finding.finding_id], false);
                setSelectedKey(null);
                presentation.setDetailOpen(false);
                refresh();
              }}
            />
          ) : (
            <p>请选择漏洞查看详情。</p>
          )
        }
      >
        <div className="findings-list-panel">
          {error && (
            <Alert variant="destructive">
              <AlertDescription>
                加载失败：{error}
                <Button variant="outline" size="sm" onClick={refresh}>
                  重试
                </Button>
              </AlertDescription>
            </Alert>
          )}
          {!data && !error ? (
            <div className="flex justify-center p-8">
              <Spinner />
            </div>
          ) : (
            data && (
              <FindingSelector
                flat={presentation.layout === "flat"}
                items={data?.items ?? []}
                contextTask={taskId}
                selectedKey={
                  selected && (presentation.layout === "split" || presentation.detailOpen)
                    ? findingRowKey(selected)
                    : null
                }
                selectedIds={selectedIds}
                onToggleSelected={(id, checked) => toggle([id], checked)}
                onToggleSelectedPage={toggle}
                onSelect={(item) => {
                  setSelectedKey(findingRowKey(item));
                  presentation.setDetailOpen(true);
                }}
              />
            )
          )}
          {data && (
            <FindingPagination
              page={page}
              pageSize={pageSize}
              total={data.total}
              onPageChange={(value) => {
                setData(null);
                setPage(value);
              }}
              onPageSizeChange={(value) => {
                setData(null);
                setPageSize(value);
                setPage(1);
              }}
            />
          )}
        </div>
      </FindingPresentation>
    </section>
  );
}
