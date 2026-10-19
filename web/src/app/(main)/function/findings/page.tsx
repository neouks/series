"use client";

import * as React from "react";

import Link from "next/link";

import { ArrowUpRightIcon, ChevronRightIcon, DownloadIcon } from "lucide-react";
import { toast } from "sonner";

import { FindingWorkspaceDetail } from "@/components/finding-detail-content";
import { FindingPagination } from "@/components/finding-pagination";
import { FindingLayoutToggle, FindingPresentation, useFindingPresentation } from "@/components/finding-presentation";
import { FindingSelector } from "@/components/finding-selector";
import { FindingFilterSelect, FindingsToolbar } from "@/components/findings-toolbar";
import { StatusBadge } from "@/components/status-badge";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field";
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group";
import { Spinner } from "@/components/ui/spinner";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";
import {
  FINDING_STATUSES,
  findingRowKey,
  fmtTime,
  isSameFinding,
  SEVERITIES,
  selectFinding,
  UNASSIGNED_TASK,
} from "@/lib/findings";
import { getLocalStorageValue, setLocalStorageValue } from "@/lib/local-storage.client";
import type { Finding, FindingAssetNode, FindingGroup, FindingStats, FindingStatus, Severity } from "@/lib/types";
import { cn } from "@/lib/utils";

import { AssetTree, assetPathOf } from "./_components/asset-tree";

const FINDING_LIST_PREFERENCE_KEY = "series_finding_list_preferences";

// 列表视图:flat = 跨任务平铺大表(默认);grouped = 按任务分组折叠;
// asset = 左侧资产树 + 右侧该子树下的发现。
type FindingView = "flat" | "grouped" | "asset";

const FINDING_VIEWS: FindingView[] = ["flat", "grouped", "asset"];

// 资产树的一次性快照。与另外两个视图不同,资产视图不轮询:进入视图、改筛选、
// 或本页改动了发现之后才重新查询。
interface AssetTreeState {
  nodes: FindingAssetNode[];
  findingTotal: number;
  truncated: boolean;
  droppedKinds: string[];
  loaded: boolean;
  loading: boolean;
}

const EMPTY_ASSET_TREE: AssetTreeState = {
  nodes: [],
  findingTotal: 0,
  truncated: false,
  droppedKinds: [],
  loaded: false,
  loading: false,
};

// 分组视图里每个已展开任务组自带一份分页状态,彼此独立。
interface GroupFindingsState {
  error?: string;
  items: Finding[];
  total: number;
  page: number;
  pageSize: number;
  loaded: boolean;
  loading: boolean;
}

// 平铺视图的页码单独放 state(而非塞进快照),筛选一变就能连带重置并触发重新加载。
interface FlatFindingsState {
  items: Finding[];
  total: number;
  loaded: boolean;
  loading: boolean;
}

const EMPTY_FLAT_STATE: FlatFindingsState = { items: [], total: 0, loaded: false, loading: false };

function findingGroupKey(group: FindingGroup) {
  return group.task_id === null ? UNASSIGNED_TASK : String(group.task_id);
}

const EMPTY_STATS: FindingStats = {
  total: 0,
  pending: 0,
  critical: 0,
  high: 0,
  medium: 0,
  low: 0,
  vulnclasses: [],
  tasks: [],
};

export default function FindingsPage() {
  const [view, setView] = React.useState<FindingView>("flat");
  const [severity, setSeverity] = React.useState<"all" | Severity>("all");
  const [status, setStatus] = React.useState<"all" | FindingStatus>("all");
  const [vulnclass, setVulnclass] = React.useState<string>("all");
  const [task, setTask] = React.useState<string>("all");
  const [sort, setSort] = React.useState<"severity" | "time">("time");
  const [search, setSearch] = React.useState("");
  const [query, setQuery] = React.useState("");
  const [expanded, setExpanded] = React.useState<string | null>(null);
  const [flat, setFlat] = React.useState<FlatFindingsState>(EMPTY_FLAT_STATE);
  const [flatPage, setFlatPage] = React.useState(1);
  const [flatPageSize, setFlatPageSize] = React.useState(50);
  const [assetTree, setAssetTree] = React.useState<AssetTreeState>(EMPTY_ASSET_TREE);
  const [assetScope, setAssetScope] = React.useState<string | null>(null);
  const [groups, setGroups] = React.useState<FindingGroup[]>([]);
  const [groupsSnapshot, setGroupsSnapshot] = React.useState("");
  const [groupsError, setGroupsError] = React.useState("");
  const initializedGroupScope = React.useRef("");
  const [groupTotal, setGroupTotal] = React.useState(0);
  const [expandedGroups, setExpandedGroups] = React.useState<Set<string>>(() => new Set());
  const [groupFindings, setGroupFindings] = React.useState<Record<string, GroupFindingsState>>({});
  const [total, setTotal] = React.useState(0);
  const [stats, setStats] = React.useState<FindingStats>(EMPTY_STATS);
  const [statsLoaded, setStatsLoaded] = React.useState(false);
  const [preferencesHydrated, setPreferencesHydrated] = React.useState(false);
  const [page, setPage] = React.useState(1);
  const [pageSize, setPageSize] = React.useState(10);
  const [deepenFinding, setDeepenFinding] = React.useState<Finding | null>(null);
  const [deepenDescription, setDeepenDescription] = React.useState("");
  const [deepening, setDeepening] = React.useState(false);
  const filterFingerprint = JSON.stringify([severity, status, vulnclass, task, sort, query]);
  const activeFilterFingerprint = React.useRef(filterFingerprint);
  activeFilterFingerprint.current = filterFingerprint;

  React.useEffect(() => {
    const raw = getLocalStorageValue(FINDING_LIST_PREFERENCE_KEY);
    if (raw) {
      try {
        const parsed = JSON.parse(raw) as {
          view?: unknown;
          severity?: unknown;
          status?: unknown;
          vulnclass?: unknown;
          task?: unknown;
          sort?: unknown;
        };
        if (FINDING_VIEWS.includes(parsed.view as FindingView)) setView(parsed.view as FindingView);
        if (parsed.severity === "all" || SEVERITIES.includes(parsed.severity as Severity)) {
          setSeverity(parsed.severity as "all" | Severity);
        }
        if (parsed.status === "all" || FINDING_STATUSES.includes(parsed.status as FindingStatus)) {
          setStatus(parsed.status as "all" | FindingStatus);
        }
        if (typeof parsed.vulnclass === "string" && parsed.vulnclass) setVulnclass(parsed.vulnclass);
        if (typeof parsed.task === "string" && parsed.task) setTask(parsed.task);
        if (parsed.sort === "severity" || parsed.sort === "time") setSort(parsed.sort);
      } catch {
        // Ignore malformed or legacy preferences and retain the defaults.
      }
    }
    setPreferencesHydrated(true);
  }, []);

  React.useEffect(() => {
    if (!preferencesHydrated) return;
    setLocalStorageValue(
      FINDING_LIST_PREFERENCE_KEY,
      JSON.stringify({ view, severity, status, vulnclass, task, sort }),
    );
  }, [preferencesHydrated, severity, sort, status, task, view, vulnclass]);

  React.useEffect(() => {
    const timer = window.setTimeout(() => setQuery(search.trim()), 300);
    return () => window.clearTimeout(timer);
  }, [search]);

  // setFindings 同时改写两个视图缓存里的同一条发现,切换视图不会看到过期状态。
  const setFindings = React.useCallback((update: (current: Finding[]) => Finding[]) => {
    setFlat((current) => ({ ...current, items: update(current.items) }));
    setGroupFindings((current) => {
      const next: Record<string, GroupFindingsState> = {};
      for (const [key, state] of Object.entries(current)) {
        next[key] = { ...state, items: update(state.items) };
      }
      return next;
    });
  }, []);

  // 勾选导出:按 finding_id(独立表 id)记选中项,跨页保留。
  const presentation = useFindingPresentation("series_global_findings_layout");
  const [selectedIds, setSelectedIds] = React.useState<Set<string>>(() => new Set());
  // 导出弹窗状态:范围(当前筛选/全部/选中) × 格式(md 单文件/md 分文件 zip/csv/json)。
  const [exportOpen, setExportOpen] = React.useState(false);
  const [exportScope, setExportScope] = React.useState<"filtered" | "all" | "selected">("filtered");
  const [exportFormat, setExportFormat] = React.useState<"md-single" | "md-zip" | "csv" | "json">("md-single");
  const [exporting, setExporting] = React.useState(false);

  const toggleSelected = React.useCallback((id: string, checked: boolean) => {
    setSelectedIds((prev) => {
      const next = new Set(prev);
      if (checked) next.add(id);
      else next.delete(id);
      return next;
    });
  }, []);

  const toggleSelectedPage = React.useCallback((ids: string[], checked: boolean) => {
    setSelectedIds((prev) => {
      const next = new Set(prev);
      for (const id of ids) {
        if (checked) next.add(id);
        else next.delete(id);
      }
      return next;
    });
  }, []);

  // 打开导出弹窗时,若有勾选项则默认范围切到「选中」,否则「当前筛选」。
  function openExport() {
    setExportScope(selectedIds.size > 0 ? "selected" : "filtered");
    setExportOpen(true);
  }

  async function doExport() {
    setExporting(true);
    try {
      await api.exportFindings({
        format: exportFormat,
        scope: exportScope,
        filters: {
          severity,
          status,
          vulnclass,
          task,
          query,
          sort,
          assetScope: view === "asset" ? (assetScope ?? undefined) : undefined,
        },
        ids: [...selectedIds],
      });
      setExportOpen(false);
      toast.success("已开始下载导出文件");
    } catch (e) {
      toast.error(`导出失败：${(e as Error).message}`);
    } finally {
      setExporting(false);
    }
  }

  const flatRequest = React.useRef(0);
  const assetTreeRequest = React.useRef(0);
  const groupRequests = React.useRef<Record<string, number>>({});
  const groupsRequest = React.useRef(0);
  const flatStateRef = React.useRef(flat);
  const expandedGroupsRef = React.useRef(expandedGroups);
  const groupFindingsRef = React.useRef(groupFindings);
  const visibleGroupKeysRef = React.useRef<Set<string>>(new Set());
  flatStateRef.current = flat;
  expandedGroupsRef.current = expandedGroups;
  groupFindingsRef.current = groupFindings;
  visibleGroupKeysRef.current = new Set(groups.map(findingGroupKey));

  // 资产视图右侧列表 = 平铺列表 + 选中子树的筛选,所以两个视图共用一份列表状态。
  const activeAssetScope = view === "asset" ? assetScope : null;

  // loadFlat 拉取平铺视图的当前页;task 筛选交给后端,与分组视图共用同一批筛选条件。
  const loadFlat = React.useCallback(async () => {
    const requestFilter = filterFingerprint;
    if (activeFilterFingerprint.current !== requestFilter) return;
    const request = ++flatRequest.current;
    setFlat((current) => ({ ...current, loading: true }));
    try {
      const result = await api.findingsPage({
        page: flatPage,
        pageSize: flatPageSize,
        severity,
        status,
        vulnclass,
        task,
        query,
        sort,
        assetScope: activeAssetScope ?? undefined,
      });
      if (request !== flatRequest.current || activeFilterFingerprint.current !== requestFilter) return;
      setFlat({ items: result.items, total: result.total, loaded: true, loading: false });
    } catch {
      if (request !== flatRequest.current || activeFilterFingerprint.current !== requestFilter) return;
      // Polling keeps the last successful snapshot visible.
      setFlat((current) => ({ ...current, loading: false }));
    }
  }, [activeAssetScope, filterFingerprint, flatPage, flatPageSize, severity, status, vulnclass, task, query, sort]);

  // loadAssetTree 取整棵资产树。树不随选中节点变化(否则选一下就塌成一条链),
  // 所以这里不带 assetScope。
  const loadAssetTree = React.useCallback(async () => {
    const requestFilter = filterFingerprint;
    if (activeFilterFingerprint.current !== requestFilter) return;
    const request = ++assetTreeRequest.current;
    setAssetTree((current) => ({ ...current, loading: true }));
    try {
      const result = await api.findingAssetTree({ severity, status, vulnclass, task, query, sort });
      if (request !== assetTreeRequest.current || activeFilterFingerprint.current !== requestFilter) return;
      setAssetTree({
        nodes: result.nodes ?? [],
        findingTotal: result.finding_total ?? 0,
        truncated: Boolean(result.truncated),
        droppedKinds: result.dropped_kinds ?? [],
        loaded: true,
        loading: false,
      });
    } catch (e) {
      if (request !== assetTreeRequest.current || activeFilterFingerprint.current !== requestFilter) return;
      setAssetTree((current) => ({ ...current, loading: false }));
      toast.error(`资产树加载失败：${(e as Error).message}`);
    }
  }, [filterFingerprint, severity, status, vulnclass, task, query, sort]);

  const refreshGroups = React.useCallback(async () => {
    const requestFilter = filterFingerprint;
    if (activeFilterFingerprint.current !== requestFilter) return;
    const request = ++groupsRequest.current;
    try {
      const result = await api.findingGroups({
        page,
        pageSize,
        severity,
        status,
        vulnclass,
        task,
        query,
        sort,
      });
      if (request !== groupsRequest.current || activeFilterFingerprint.current !== requestFilter) return;
      setGroups(result.items);
      setGroupsSnapshot(JSON.stringify([filterFingerprint, page, pageSize]));
      setGroupsError("");
      setGroupTotal(result.total);
      setTotal(result.finding_total);
    } catch (e) {
      if (request === groupsRequest.current && activeFilterFingerprint.current === requestFilter)
        setGroupsError((e as Error).message);
    }
  }, [filterFingerprint, page, pageSize, severity, status, vulnclass, task, query, sort]);

  const loadGroup = React.useCallback(
    async (key: string, groupPage: number, groupPageSize: number) => {
      const request = (groupRequests.current[key] ?? 0) + 1;
      const requestFilter = filterFingerprint;
      if (activeFilterFingerprint.current !== requestFilter) return;
      groupRequests.current[key] = request;
      setGroupFindings((current) => ({
        ...current,
        [key]: {
          items: current[key]?.items ?? [],
          total: current[key]?.total ?? 0,
          page: groupPage,
          pageSize: groupPageSize,
          loaded: current[key]?.loaded ?? false,
          loading: true,
        },
      }));
      try {
        const result = await api.findingsPage({
          page: groupPage,
          pageSize: groupPageSize,
          severity,
          status,
          vulnclass,
          task: key,
          query,
          sort,
        });
        if (groupRequests.current[key] !== request || activeFilterFingerprint.current !== requestFilter) return;
        setGroupFindings((current) => ({
          ...current,
          [key]: {
            items: result.items,
            total: result.total,
            page: result.page,
            pageSize: result.page_size,
            loaded: true,
            loading: false,
          },
        }));
      } catch (e) {
        if (groupRequests.current[key] !== request || activeFilterFingerprint.current !== requestFilter) return;
        setGroupFindings((current) => ({
          ...current,
          [key]: {
            ...(current[key] ?? {
              items: [],
              total: 0,
              page: groupPage,
              pageSize: groupPageSize,
              loaded: false,
            }),
            loading: false,
            error: (e as Error).message,
          },
        }));
      }
    },
    [filterFingerprint, severity, status, vulnclass, query, sort],
  );

  React.useEffect(() => {
    for (const [key, state] of Object.entries(groupFindings)) {
      if (!state.loaded || state.loading) continue;
      const lastPage = Math.max(1, Math.ceil(state.total / state.pageSize));
      if (state.page > lastPage) void loadGroup(key, lastPage, state.pageSize);
    }
  }, [groupFindings, loadGroup]);

  const toggleGroup = React.useCallback(
    (key: string) => {
      const opening = !expandedGroups.has(key);
      const next = new Set(expandedGroups);
      if (opening) next.add(key);
      else next.delete(key);
      setExpandedGroups(next);
      const state = groupFindings[key];
      if (opening && !state?.loaded && !state?.loading) {
        void loadGroup(key, state?.page ?? 1, state?.pageSize ?? 10);
      }
    },
    [expandedGroups, groupFindings, loadGroup],
  );

  React.useEffect(() => {
    if (view !== "grouped") {
      initializedGroupScope.current = "";
      return;
    }
    const scope = JSON.stringify([filterFingerprint, page, pageSize]);
    if (groupsSnapshot !== scope || initializedGroupScope.current === scope || !groups.length) return;
    initializedGroupScope.current = scope;
    const key = findingGroupKey(groups[0]);
    setExpandedGroups(new Set([key]));
    void loadGroup(key, 1, 10);
  }, [view, filterFingerprint, page, pageSize, groupsSnapshot, groups, loadGroup]);

  // 行内改动后刷新当前视图:平铺视图重拉当前页,分组视图刷组头 + 该发现所在的组。
  const refreshAfterMutation = React.useCallback(
    (finding: Finding, removed = false) => {
      if (view === "asset") {
        // 资产视图不轮询,所以改完要顺带把树的计数也重新算一次。
        void loadFlat();
        void loadAssetTree();
        return;
      }
      if (view === "flat") {
        // 删空最后一页时,页码由越界修正 effect 回退并连带重新加载。
        void loadFlat();
        return;
      }
      void refreshGroups();
      const key = finding.task_id ?? UNASSIGNED_TASK;
      const state = groupFindingsRef.current[key];
      if (state?.loaded) {
        const nextTotal = Math.max(0, state.total - (removed ? 1 : 0));
        const lastPage = Math.max(1, Math.ceil(nextTotal / state.pageSize));
        void loadGroup(key, Math.min(state.page, lastPage), state.pageSize);
      }
    },
    [loadAssetTree, loadFlat, loadGroup, refreshGroups, view],
  );

  // Reset every view's pagination and expansion when a shared finding filter changes.
  React.useEffect(() => {
    void filterFingerprint;
    setPage(1);
    setExpanded(null);
    setExpandedGroups(new Set());
    setGroupFindings({});
    setFlatPage(1);
    setFlat(EMPTY_FLAT_STATE);
    // 筛选变了树也会变,原先选中的节点可能已经不在树里,退回「全部资产」。
    setAssetScope(null);
    setAssetTree(EMPTY_ASSET_TREE);
  }, [filterFingerprint]);

  // 换资产节点等于换了一份结果集,回到第一页。
  React.useEffect(() => {
    void assetScope;
    setFlatPage(1);
  }, [assetScope]);

  // 资产树只在进入视图 / 筛选变化时查一次(以及本页改动发现后由
  // refreshAfterMutation 主动重拉),不做轮询。
  React.useEffect(() => {
    if (!preferencesHydrated || view !== "asset") return;
    void loadAssetTree();
  }, [loadAssetTree, preferencesHydrated, view]);

  // 只轮询当前视图:平铺视图刷当前页,分组视图刷组头与每个已展开的组(其分页彼此独立)。
  // 资产视图只查一次(见下面的 return),它的左树是导航结构,没必要每 5 秒重算。
  // 等偏好水合后再发首个请求,否则会先按默认视图/筛选白拉一次。
  React.useEffect(() => {
    if (!preferencesHydrated) return;
    const refresh = () => {
      if (view === "flat" || view === "asset") {
        if (!flatStateRef.current.loading) void loadFlat();
        return;
      }
      void refreshGroups();
      for (const key of expandedGroupsRef.current) {
        if (!visibleGroupKeysRef.current.has(key)) continue;
        const state = groupFindingsRef.current[key];
        if (state?.loaded && !state.loading) void loadGroup(key, state.page, state.pageSize);
      }
    };
    refresh();
    if (view === "asset") return;
    const timer = setInterval(refresh, 5000);
    return () => clearInterval(timer);
  }, [loadFlat, loadGroup, preferencesHydrated, refreshGroups, view]);

  React.useEffect(() => {
    const lastPage = Math.max(1, Math.ceil(groupTotal / pageSize));
    if (page > lastPage) setPage(lastPage);
  }, [groupTotal, page, pageSize]);

  React.useEffect(() => {
    if (!flat.loaded) return;
    const lastPage = Math.max(1, Math.ceil(flat.total / flatPageSize));
    if (flatPage > lastPage) setFlatPage(lastPage);
  }, [flat.loaded, flat.total, flatPage, flatPageSize]);

  // Whole-table aggregates (stat cards + vuln-class options) — independent of the
  // current page, so they stay exact.
  React.useEffect(() => {
    let alive = true;
    const load = () => {
      api
        .findingStats()
        .then((s) => {
          if (alive) {
            setStats(s);
            setStatsLoaded(true);
          }
        })
        .catch(() => {
          // Keep the previous aggregate snapshot until the next poll.
        });
    };
    load();
    const t = setInterval(load, 5000);
    return () => {
      alive = false;
      clearInterval(t);
    };
  }, []);

  React.useEffect(() => {
    if (!statsLoaded) return;
    if (vulnclass !== "all" && !stats.vulnclasses.includes(vulnclass)) setVulnclass("all");
    if (
      task !== "all" &&
      task !== UNASSIGNED_TASK &&
      !(stats.tasks ?? []).some((option) => String(option.id) === task)
    ) {
      setTask("all");
    }
  }, [stats, statsLoaded, task, vulnclass]);

  // deleteFinding 删除一个漏洞(需二次确认):删成功后从列表移除、收起行、刷新统计。
  const deleteFinding = React.useCallback(
    async (f: Finding, reason: string) => {
      if (!f.finding_id) return;
      await api.deleteFinding(f.finding_id, reason);
      setFindings((cur) => cur.filter((x) => !isSameFinding(x, f)));
      setSelectedIds((current) => {
        const next = new Set(current);
        next.delete(f.finding_id as string);
        return next;
      });
      setTotal((t) => Math.max(0, t - 1));
      setFlat((cur) => ({ ...cur, total: Math.max(0, cur.total - 1) }));
      const rowKey = findingRowKey(f);
      setExpanded((cur) => (cur === rowKey ? null : cur));
      api
        .findingStats()
        .then(setStats)
        .catch(() => {
          // The deletion remains valid even if the aggregate refresh fails.
        });
      refreshAfterMutation(f, true);
    },
    [refreshAfterMutation, setFindings],
  );

  const openDeepen = React.useCallback((f: Finding) => {
    setDeepenFinding(f);
    setDeepenDescription("");
  }, []);

  async function submitDeepen() {
    if (!deepenFinding?.finding_id || !deepenDescription.trim() || deepening) return;
    setDeepening(true);
    try {
      const result = await api.deepenFinding(deepenFinding.finding_id, deepenDescription.trim());
      toast.success(
        result.queued
          ? `深入意图 #${result.intent_id} 已进入任务队列`
          : `已创建高优先级 Worker 意图 #${result.intent_id}`,
      );
      refreshAfterMutation(deepenFinding);
      setDeepenFinding(null);
      setDeepenDescription("");
    } catch (error) {
      toast.error(`提交失败：${(error as Error).message}`);
    } finally {
      setDeepening(false);
    }
  }

  // 导出弹窗里「当前筛选」的条数:两个视图的筛选一致,只是统计口径来源不同。
  // 平铺与资产视图共用 flat 列表状态,分组视图的口径来自组接口的 finding_total。
  const filteredTotal = view === "grouped" ? total : flat.total;
  const assetPath = React.useMemo(
    () => (view === "asset" ? assetPathOf(assetTree.nodes, assetScope) : []),
    [assetScope, assetTree.nodes, view],
  );

  const visibleFindings = React.useMemo(
    () =>
      view === "grouped"
        ? groups
            .filter((group) => expandedGroups.has(findingGroupKey(group)))
            .flatMap((group) => groupFindings[findingGroupKey(group)]?.items ?? [])
        : flat.items,
    [view, groups, expandedGroups, groupFindings, flat.items],
  );
  const currentFinding = selectFinding(visibleFindings, expanded);
  React.useEffect(() => {
    setExpanded((key) => {
      const selected = selectFinding(visibleFindings, key);
      return selected ? findingRowKey(selected) : null;
    });
  }, [visibleFindings]);
  const onDetailChanged = React.useCallback(
    (finding: Finding) => {
      refreshAfterMutation(finding);
      void api
        .findingStats()
        .then(setStats)
        .catch((error: Error) => toast.error(error.message));
    },
    [refreshAfterMutation],
  );
  const selectorProps = {
    flat: presentation.layout === "flat",
    selectedIds,
    onToggleSelected: toggleSelected,
    onToggleSelectedPage: toggleSelectedPage,
    selectedKey:
      currentFinding && (presentation.layout === "split" || presentation.detailOpen)
        ? findingRowKey(currentFinding)
        : null,
    onSelect: (finding: Finding) => {
      setExpanded(findingRowKey(finding));
      presentation.setDetailOpen(true);
    },
  };

  // 平铺视图与资产视图右侧是同一张表 + 同一份分页,只是筛选条件不同。
  const flatListCard = (
    <div className="findings-list-panel">
      <div className="flex min-w-0 flex-1 flex-col">
        {flat.loading && !flat.loaded ? (
          <div className="flex min-h-36 items-center justify-center">
            <Spinner />
          </div>
        ) : (
          <>
            <FindingSelector items={flat.items} selectAllLabel="选择当前页全部" {...selectorProps} />
            <FindingPagination
              page={flatPage}
              pageSize={flatPageSize}
              total={flat.total}
              onPageChange={setFlatPage}
              onPageSizeChange={(nextSize) => {
                setFlatPageSize(nextSize);
                setFlatPage(1);
              }}
              pageSizeOptions={[10, 20, 50, 100]}
            />
          </>
        )}
      </div>
    </div>
  );

  return (
    <div className="findings-workspace" data-findings-surface>
      <FindingsToolbar
        total={(view === "grouped" ? !!groupsSnapshot : flat.loaded) ? filteredTotal : undefined}
        search={search}
        onSearch={setSearch}
        sort={sort}
        onSort={(value) => setSort(value as "severity" | "time")}
        sortOptions={[
          { value: "time", label: "最近发现" },
          { value: "severity", label: "严重度优先" },
        ]}
        view={view}
        onView={(value) => setView(value as FindingView)}
        viewOptions={[
          { value: "flat", label: "全部发现" },
          { value: "grouped", label: "按任务分组" },
          { value: "asset", label: "按资产" },
        ]}
        status={status}
        onStatus={(value) => setStatus(value as "all" | FindingStatus)}
        severity={severity}
        onSeverity={(value) => setSeverity(value as "all" | Severity)}
        activeFilters={[severity, status, vulnclass, task].filter((value) => value !== "all").length}
        onReset={() => {
          setSeverity("all");
          setStatus("all");
          setVulnclass("all");
          setTask("all");
          setSearch("");
        }}
        filters={
          <>
            <Field>
              <FieldLabel>漏洞类型</FieldLabel>
              <FindingFilterSelect
                label="漏洞类型"
                value={vulnclass}
                onChange={setVulnclass}
                options={[
                  { value: "all", label: "全部类型" },
                  ...stats.vulnclasses.map((value) => ({ value, label: value })),
                ]}
              />
            </Field>
            <Field>
              <FieldLabel>所属任务</FieldLabel>
              <FindingFilterSelect
                label="所属任务"
                value={task}
                onChange={setTask}
                options={[
                  { value: "all", label: "全部任务" },
                  { value: UNASSIGNED_TASK, label: "未关联 / 任务已删除" },
                  ...(stats.tasks ?? []).map((item) => ({
                    value: String(item.id),
                    label: item.name || item.description || `任务 #${item.id}`,
                  })),
                ]}
              />
            </Field>
            <Field>
              <FieldLabel>详情展示</FieldLabel>
              <FindingLayoutToggle value={presentation.layout} onChange={presentation.changeLayout} />
            </Field>
            <Field>
              <FieldLabel>全站统计</FieldLabel>
              <FieldDescription>
                共 {stats.total} 条 · 待处理 {stats.pending}
                <br />
                严重 {stats.critical} · 高危 {stats.high} · 中危 {stats.medium} · 低危 {stats.low}
              </FieldDescription>
            </Field>
          </>
        }
        actions={
          <>
            <Button size="icon" variant="ghost" onClick={openExport} aria-label="导出发现" title="导出发现">
              <DownloadIcon />
            </Button>
            {selectedIds.size > 0 && (
              <span className="text-muted-foreground text-xs tabular-nums">已选 {selectedIds.size} 条</span>
            )}
          </>
        }
      />
      <div className="flex min-w-0 flex-1 flex-col gap-4">
        <FindingPresentation
          {...presentation}
          onOpenChange={presentation.setDetailOpen}
          detail={
            <>
              {" "}
              {currentFinding ? (
                <FindingWorkspaceDetail
                  finding={currentFinding}
                  onChanged={onDetailChanged}
                  onDelete={deleteFinding}
                  onDeepen={openDeepen}
                />
              ) : (
                <p className="py-12 text-center text-muted-foreground">请选择漏洞查看详情、流量证据与复测记录。</p>
              )}{" "}
            </>
          }
        >
          <div className="flex min-w-0 flex-col gap-3">
            {view === "flat" && flatListCard}

            {view === "asset" && (
              <div className="flex min-w-0 flex-col gap-3">
                <Card className="gap-0 py-3">
                  <CardContent className="flex flex-col px-3">
                    {assetTree.loading && !assetTree.loaded ? (
                      <div className="flex min-h-36 items-center justify-center">
                        <Spinner />
                      </div>
                    ) : (
                      <AssetTree
                        nodes={assetTree.nodes}
                        selected={assetScope}
                        onSelect={setAssetScope}
                        loading={assetTree.loading}
                        truncated={assetTree.truncated}
                        droppedKinds={assetTree.droppedKinds}
                        findingTotal={assetTree.findingTotal}
                        onRefresh={() => void loadAssetTree()}
                      />
                    )}
                  </CardContent>
                </Card>
                <div className="flex min-w-0 flex-col gap-2">
                  <div className="flex min-w-0 flex-wrap items-center gap-1 text-muted-foreground text-sm">
                    <button
                      type="button"
                      className={cn("hover:text-foreground", assetScope === null && "font-medium text-foreground")}
                      onClick={() => setAssetScope(null)}
                    >
                      全部资产
                    </button>
                    {assetPath.map((node) => (
                      <React.Fragment key={node.key}>
                        <ChevronRightIcon className="size-3.5 shrink-0" aria-hidden="true" />
                        <button
                          type="button"
                          className={cn(
                            "max-w-[16rem] truncate hover:text-foreground",
                            node.key === assetScope && "font-medium text-foreground",
                          )}
                          title={node.label}
                          onClick={() => setAssetScope(node.key)}
                        >
                          {node.display}
                        </button>
                      </React.Fragment>
                    ))}
                    <span className="ml-auto shrink-0 text-xs tabular-nums">共 {flat.total} 条</span>
                  </div>
                  {flatListCard}
                </div>
              </div>
            )}

            {view === "grouped" && (
              <div className="flex flex-col gap-3">
                {groupsError && (
                  <Alert variant="destructive">
                    <AlertDescription>
                      分组加载失败：{groupsError}
                      <Button variant="link" onClick={() => void refreshGroups()}>
                        重试
                      </Button>
                    </AlertDescription>
                  </Alert>
                )}
                {!groupsSnapshot && !groupsError && (
                  <div className="flex justify-center p-8">
                    <Spinner />
                  </div>
                )}
                {groups.map((group) => {
                  const key = findingGroupKey(group);
                  const groupOpen = expandedGroups.has(key);
                  const state = groupFindings[key] ?? {
                    items: [],
                    total: group.count,
                    page: 1,
                    pageSize: 10,
                    loaded: false,
                    loading: false,
                  };
                  return (
                    <Card key={key} className="gap-0 py-0">
                      <CardHeader className="px-4 py-3">
                        <div className="flex min-w-0 flex-wrap items-center gap-3">
                          <button
                            type="button"
                            className="flex min-w-0 flex-1 items-center gap-3 text-left"
                            aria-expanded={groupOpen}
                            onClick={() => toggleGroup(key)}
                          >
                            <ChevronRightIcon
                              className={cn(
                                "size-4 shrink-0 text-muted-foreground transition-transform",
                                groupOpen && "rotate-90",
                              )}
                            />
                            <div className="flex min-w-0 flex-col gap-1">
                              <CardTitle className="truncate text-sm">
                                {group.task_id === null
                                  ? "未关联 / 任务已删除"
                                  : group.task_name
                                    ? `${group.task_name}（任务 #${group.task_id}）`
                                    : `任务 #${group.task_id}`}
                              </CardTitle>
                              <CardDescription className="truncate" title={group.task_description}>
                                {group.task_description || "来源任务不可用"}
                              </CardDescription>
                            </div>
                          </button>
                          <div className="flex flex-wrap items-center gap-2">
                            {group.task_status && <StatusBadge domain="task" value={group.task_status} dot />}
                            {SEVERITIES.map((level) => {
                              const count = group[level];
                              if (count === 0) return null;
                              return (
                                <span key={level} className="inline-flex items-center gap-1">
                                  <StatusBadge domain="severity" value={level} dot />
                                  <span className="text-muted-foreground text-xs tabular-nums">{count}</span>
                                </span>
                              );
                            })}
                            <span className="text-muted-foreground text-xs tabular-nums">
                              {fmtTime(group.last_found_at)}
                            </span>
                            {group.task_id !== null && (
                              <Button size="icon-sm" variant="ghost" asChild>
                                <Link
                                  href={`/function/tasks/detail?id=${group.task_id}`}
                                  aria-label={`查看任务 #${group.task_id}`}
                                >
                                  <ArrowUpRightIcon />
                                </Link>
                              </Button>
                            )}
                          </div>
                        </div>
                      </CardHeader>
                      {groupOpen && (
                        <CardContent className="px-0">
                          {state.error && (
                            <Alert variant="destructive">
                              <AlertDescription>
                                漏洞加载失败：{state.error}
                                <Button variant="link" onClick={() => void loadGroup(key, state.page, state.pageSize)}>
                                  重试
                                </Button>
                              </AlertDescription>
                            </Alert>
                          )}
                          {state.loading && !state.loaded ? (
                            <div className="flex min-h-36 items-center justify-center">
                              <Spinner />
                            </div>
                          ) : (
                            <>
                              <FindingSelector
                                items={state.items}
                                selectAllLabel="选择本组当前页全部"
                                {...selectorProps}
                              />
                              <FindingPagination
                                page={state.page}
                                pageSize={state.pageSize}
                                total={state.total}
                                onPageChange={(nextPage) => void loadGroup(key, nextPage, state.pageSize)}
                                onPageSizeChange={(nextSize) => void loadGroup(key, 1, nextSize)}
                              />
                            </>
                          )}
                        </CardContent>
                      )}
                    </Card>
                  );
                })}
                {groupsSnapshot && !groupsError && groups.length === 0 && (
                  <Card>
                    <CardContent className="py-12 text-center text-muted-foreground text-sm">
                      没有匹配的发现。
                    </CardContent>
                  </Card>
                )}
                <FindingPagination
                  page={page}
                  pageSize={pageSize}
                  total={groupTotal}
                  onPageChange={setPage}
                  onPageSizeChange={(nextPageSize) => {
                    setPageSize(nextPageSize);
                    setPage(1);
                  }}
                  pageSizeOptions={[5, 10, 20]}
                />
              </div>
            )}
          </div>
        </FindingPresentation>
      </div>

      <Dialog
        open={deepenFinding !== null}
        onOpenChange={(open) => {
          if (open || deepening) return;
          setDeepenFinding(null);
          setDeepenDescription("");
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>深入利用漏洞</DialogTitle>
            <DialogDescription className="break-words">
              将在原任务 #{deepenFinding?.task_id} 中创建优先级 10 的 Worker 意图，基于当前漏洞开展二次验证：
              {deepenFinding?.name || deepenFinding?.vulnclass || deepenFinding?.summary}
            </DialogDescription>
          </DialogHeader>
          <FieldGroup>
            <Field>
              <FieldLabel htmlFor="finding-deepen-description">利用描述</FieldLabel>
              <Textarea
                id="finding-deepen-description"
                value={deepenDescription}
                onChange={(event) => setDeepenDescription(event.target.value)}
                maxLength={4000}
                placeholder="描述需要验证的利用路径、边界条件、目标或期望证据"
                disabled={deepening}
              />
              <FieldDescription className="flex justify-between gap-3">
                <span>新意图会继承该漏洞的资产锚点。</span>
                <span className="shrink-0 tabular-nums">{deepenDescription.length} / 4000</span>
              </FieldDescription>
            </Field>
          </FieldGroup>
          <DialogFooter>
            <Button
              variant="outline"
              onClick={() => {
                setDeepenFinding(null);
                setDeepenDescription("");
              }}
              disabled={deepening}
            >
              取消
            </Button>
            <Button onClick={submitDeepen} disabled={deepening || !deepenDescription.trim()}>
              {deepening && <Spinner data-icon="inline-start" />}
              创建深入意图
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={exportOpen} onOpenChange={setExportOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>导出发现</DialogTitle>
            <DialogDescription>选择导出范围与格式,生成后浏览器会自动下载。</DialogDescription>
          </DialogHeader>

          <div className="flex flex-col gap-5 py-1">
            <div className="flex flex-col gap-2">
              <span className="text-muted-foreground text-xs">导出范围</span>
              <RadioGroup value={exportScope} onValueChange={(v) => setExportScope(v as typeof exportScope)}>
                <label htmlFor="export-scope-filtered" className="flex items-center gap-2 text-sm">
                  <RadioGroupItem id="export-scope-filtered" value="filtered" /> 导出当前筛选结果（共 {filteredTotal}{" "}
                  条）
                </label>
                <label htmlFor="export-scope-all" className="flex items-center gap-2 text-sm">
                  <RadioGroupItem id="export-scope-all" value="all" /> 导出全部
                </label>
                <label
                  htmlFor="export-scope-selected"
                  className={cn("flex items-center gap-2 text-sm", selectedIds.size === 0 && "text-muted-foreground")}
                >
                  <RadioGroupItem id="export-scope-selected" value="selected" disabled={selectedIds.size === 0} />
                  导出勾选的 {selectedIds.size} 条
                </label>
              </RadioGroup>
            </div>

            <div className="flex flex-col gap-2">
              <span className="text-muted-foreground text-xs">导出格式</span>
              <RadioGroup value={exportFormat} onValueChange={(v) => setExportFormat(v as typeof exportFormat)}>
                <label htmlFor="export-format-md-single" className="flex items-center gap-2 text-sm">
                  <RadioGroupItem id="export-format-md-single" value="md-single" /> Markdown 汇总报告（单个 .md 文件）
                </label>
                <label htmlFor="export-format-md-zip" className="flex items-center gap-2 text-sm">
                  <RadioGroupItem id="export-format-md-zip" value="md-zip" /> Markdown 分文件（一漏洞一 .md,打包 .zip）
                </label>
                <label htmlFor="export-format-csv" className="flex items-center gap-2 text-sm">
                  <RadioGroupItem id="export-format-csv" value="csv" /> CSV 表格（.csv）
                </label>
                <label htmlFor="export-format-json" className="flex items-center gap-2 text-sm">
                  <RadioGroupItem id="export-format-json" value="json" /> JSON（.json）
                </label>
              </RadioGroup>
            </div>
          </div>

          <DialogFooter>
            <Button variant="outline" onClick={() => setExportOpen(false)} disabled={exporting}>
              取消
            </Button>
            <Button onClick={doExport} disabled={exporting || (exportScope === "selected" && selectedIds.size === 0)}>
              <DownloadIcon /> {exporting ? "导出中…" : "导出"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
