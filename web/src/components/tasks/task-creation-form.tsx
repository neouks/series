"use client";

import * as React from "react";

import { InputGroup, InputGroupAddon, InputGroupButton, InputGroupTextarea } from "@/components/ui/input-group";
import { ArrowUp, Settings2 } from "lucide-react";
import { useRouter } from "next/navigation";

import { ChevronRightIcon, PaperclipIcon, PlusIcon } from "lucide-react";
import { toast } from "sonner";

import { AssetApprovalTemplateField } from "@/components/asset-approval-template";
import { StatusBadge } from "@/components/status-badge";
import { TaskLLMProfileChain } from "@/components/task-llm-profile-chain";
import { TaskTemplateControls } from "@/components/task-template-controls";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import {
  Combobox,
  ComboboxChip,
  ComboboxChips,
  ComboboxChipsInput,
  ComboboxContent,
  ComboboxEmpty,
  ComboboxItem,
  ComboboxList,
  ComboboxValue,
} from "@/components/ui/combobox";
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Spinner } from "@/components/ui/spinner";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";
import type {
  Asset,
  AssetApprovalTemplate,
  ChatAttachment,
  Company,
  LLMProfile,
  Task,
  TaskCategory,
} from "@/lib/types";

// fmtBytes renders a human file size for the upload manifest.
function fmtBytes(n: number): string {
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MB`;
  if (n >= 1 << 10) return `${(n / (1 << 10)).toFixed(1)} KB`;
  return `${n} B`;
}

// UPLOAD_MARKER labels the auto-appended block of uploaded-file paths inside the task
// description, so re-uploads append under the same block instead of adding a new header.
const UPLOAD_MARKER = "【上传文件（绝对路径）】";

// appendUploads folds newly-uploaded files' ABSOLUTE paths into the description as a
// Read/Bash-friendly manifest — the worker opens them by path. Keeps one marked block:
// first upload adds the header, later uploads append bullets under it.
function appendUploads(desc: string, atts: ChatAttachment[]): string {
  const bullets = atts.map((a) => `- ${a.abs ?? a.path}（${fmtBytes(a.size)}）`).join("\n");
  if (desc.includes(UPLOAD_MARKER)) {
    return `${desc.replace(/\s*$/, "")}\n${bullets}\n`;
  }
  const head = desc.trim() ? `${desc.replace(/\s*$/, "")}\n\n` : "";
  return `${head}${UPLOAD_MARKER} worker 可用 Read/Bash 按路径打开：\n${bullets}\n`;
}

const MAX_SOURCE_TASKS = 8;
const MAX_TASK_ASSETS = 100;
const TASK_ASSET_TYPES = ["root_domain", "ip", "subdomain", "app", "service", "endpoint"] as const;
function SourceTaskPicker({
  tasks,
  value,
  onValueChange,
  portalContainer,
}: {
  tasks: Task[];
  value: string[];
  onValueChange: (value: string[]) => void;
  portalContainer?: React.RefObject<HTMLElement | null>;
}) {
  const tasksByID = React.useMemo(() => new Map(tasks.map((task) => [task.id, task])), [tasks]);
  const taskIDs = React.useMemo(() => tasks.map((task) => task.id), [tasks]);
  const atLimit = value.length >= MAX_SOURCE_TASKS;

  const handleValueChange = (next: string[]) => {
    onValueChange(next.slice(0, MAX_SOURCE_TASKS));
  };

  return (
    <Combobox
      items={taskIDs}
      itemToStringValue={(taskID) => {
        const task = tasksByID.get(taskID);
        return task ? `${task.id} ${task.description} ${task.goal}` : taskID;
      }}
      multiple
      value={value}
      onValueChange={handleValueChange}
    >
      <ComboboxChips>
        <ComboboxValue>
          {value.map((taskID) => (
            <ComboboxChip key={taskID}>#{taskID}</ComboboxChip>
          ))}
        </ComboboxValue>
        <ComboboxChipsInput
          id="source-tasks"
          placeholder={atLimit ? `最多关联 ${MAX_SOURCE_TASKS} 个任务` : "搜索任务 ID、描述或目标"}
          disabled={atLimit}
        />
      </ComboboxChips>
      <ComboboxContent portalContainer={portalContainer}>
        <ComboboxEmpty>没有匹配的任务</ComboboxEmpty>
        <ComboboxList>
          {(taskID) => {
            const task = tasksByID.get(taskID);
            return (
              <ComboboxItem key={taskID} value={taskID} disabled={atLimit && !value.includes(taskID)}>
                <div className="flex min-w-0 flex-1 items-center gap-2">
                  <div className="min-w-0 flex-1">
                    <p className="truncate text-sm font-medium">
                      #{taskID} · {task?.description ?? "未知任务"}
                    </p>
                    {task?.goal && <p className="text-muted-foreground truncate text-xs">{task.goal}</p>}
                  </div>
                  {task && <StatusBadge domain="task" value={task.status} />}
                </div>
              </ComboboxItem>
            );
          }}
        </ComboboxList>
      </ComboboxContent>
    </Combobox>
  );
}

// CategoryPicker 是新建任务里的单选分类选择器：可搜索已有分类；输入库里没有的名称后
// 回车（或点下拉里的「创建」）即时新建分类并选中，选中项以可移除的 tag 展示。分类是
// 全局资源，这里即时创建与「分类管理」里手动新建等价。只允许一个分类。
function CategoryPicker({
  categories,
  value,
  onValueChange,
  onCategoryCreated,
  portalContainer,
}: {
  categories: TaskCategory[];
  value?: number;
  onValueChange: (categoryID?: number) => void;
  onCategoryCreated: () => void;
  portalContainer?: React.RefObject<HTMLElement | null>;
}) {
  const [inputValue, setInputValue] = React.useState("");
  const [creating, setCreating] = React.useState(false);
  // 新建的分类要等父层重新拉取才回流到 categories，先本地留一份，避免选中的 chip 和
  // 下拉在这段窗口里显示成「未知分类」。
  const [localExtra, setLocalExtra] = React.useState<TaskCategory[]>([]);

  const allCategories = React.useMemo(() => {
    const byID = new Map<number, TaskCategory>();
    for (const category of categories) byID.set(category.id, category);
    for (const category of localExtra) if (!byID.has(category.id)) byID.set(category.id, category);
    return [...byID.values()];
  }, [categories, localExtra]);

  const byID = React.useMemo(() => new Map(allCategories.map((c) => [String(c.id), c])), [allCategories]);
  const categoryIDs = React.useMemo(() => allCategories.map((c) => String(c.id)), [allCategories]);
  const selectedIDs = value != null ? [String(value)] : [];

  const trimmed = inputValue.trim();
  const lower = trimmed.toLowerCase();
  // 与 base-ui 默认子串过滤保持一致，用来判断「有没有相关分类」。
  const matchCount = trimmed
    ? allCategories.filter((c) => c.name.toLowerCase().includes(lower)).length
    : allCategories.length;

  const createAndSelect = async () => {
    if (!trimmed || creating) return;
    // 精确同名已存在则直接选中，不重复创建。
    const existing = allCategories.find((c) => c.name.toLowerCase() === lower);
    if (existing) {
      onValueChange(existing.id);
      setInputValue("");
      return;
    }
    setCreating(true);
    try {
      const created = await api.createTaskCategory(trimmed);
      setLocalExtra((prev) => [...prev, created]);
      onValueChange(created.id);
      setInputValue("");
      onCategoryCreated();
      toast.success(`已创建分类「${created.name}」`);
    } catch (e) {
      toast.error(`创建分类失败：${(e as Error).message}`);
    } finally {
      setCreating(false);
    }
  };

  return (
    <Combobox
      items={categoryIDs}
      itemToStringValue={(id) => byID.get(id)?.name ?? id}
      multiple
      value={selectedIDs}
      onValueChange={(next: string[]) => {
        // 单选：取最新选中的一个；移除 chip（清空）则回到未分类。
        const last = next[next.length - 1];
        onValueChange(last ? Number(last) : undefined);
        setInputValue("");
      }}
      inputValue={inputValue}
      onInputValueChange={setInputValue}
    >
      <ComboboxChips>
        <ComboboxValue>
          {selectedIDs.map((id) => (
            <ComboboxChip key={id}>{byID.get(id)?.name ?? "未知分类"}</ComboboxChip>
          ))}
        </ComboboxValue>
        <ComboboxChipsInput
          id="task-category"
          placeholder={selectedIDs.length ? "" : "搜索分类，或输入新名称后回车创建"}
          onKeyDown={(e) => {
            // 完全无匹配时回车 = 创建；有匹配项时保留 base-ui 的「回车选中高亮项」。
            if (e.key === "Enter" && matchCount === 0 && trimmed) {
              e.preventDefault();
              void createAndSelect();
            }
          }}
        />
      </ComboboxChips>
      <ComboboxContent portalContainer={portalContainer}>
        <ComboboxList>
          {(id: string) => (
            <ComboboxItem key={id} value={id}>
              {byID.get(id)?.name ?? id}
            </ComboboxItem>
          )}
        </ComboboxList>
        {matchCount === 0 &&
          (trimmed ? (
            <button
              type="button"
              disabled={creating}
              onClick={() => void createAndSelect()}
              className="flex w-full items-center gap-2 px-2 py-2 text-left text-sm hover:bg-accent hover:text-accent-foreground disabled:opacity-50"
            >
              {creating ? <Spinner className="size-4" /> : <PlusIcon className="size-4" />}
              创建分类「{trimmed}」
            </button>
          ) : (
            <div className="px-2 py-2 text-sm text-muted-foreground">输入名称以搜索或创建分类</div>
          ))}
      </ComboboxContent>
    </Combobox>
  );
}

const COMPANY_SCOPE_LABELS: Record<string, string> = {
  domain: "域名",
  ip: "IP",
  cidr: "CIDR",
  icp: "ICP",
  keyword: "关键词",
};

function companyScopeSummary(company: Company): string {
  const rows = company.scope ?? [];
  if (rows.length === 0) return "未配置资产范围";
  const preview = rows.slice(0, 3).map((row) => {
    const value = row.raw || row.value || row.domain || row.net || "";
    return `${COMPANY_SCOPE_LABELS[row.kind] ?? row.kind}：${value}`;
  });
  return `${preview.join(" · ")}${rows.length > preview.length ? ` · 另 ${rows.length - preview.length} 条` : ""}`;
}

function CompanyPicker({
  companies,
  value,
  onValueChange,
  portalContainer,
}: {
  companies: Company[];
  value: number[];
  onValueChange: (value: number[]) => void;
  portalContainer?: React.RefObject<HTMLElement | null>;
}) {
  const companiesByID = React.useMemo(
    () => new Map(companies.map((company) => [String(company.id), company])),
    [companies],
  );
  const companyIDs = React.useMemo(() => companies.map((company) => String(company.id)), [companies]);
  const selectedIDs = React.useMemo(() => value.map(String), [value]);

  return (
    <Combobox
      items={companyIDs}
      itemToStringValue={(companyID) => {
        const company = companiesByID.get(companyID);
        return company ? `${company.name} ${companyScopeSummary(company)}` : companyID;
      }}
      multiple
      value={selectedIDs}
      onValueChange={(next) => onValueChange(next.map(Number).filter(Number.isFinite))}
    >
      <ComboboxChips>
        <ComboboxValue>
          {selectedIDs.map((companyID) => (
            <ComboboxChip key={companyID}>{companiesByID.get(companyID)?.name ?? `企业 #${companyID}`}</ComboboxChip>
          ))}
        </ComboboxValue>
        <ComboboxChipsInput id="task-companies" placeholder="搜索企业名称或资产范围" />
      </ComboboxChips>
      <ComboboxContent portalContainer={portalContainer}>
        <ComboboxEmpty>没有匹配的企业</ComboboxEmpty>
        <ComboboxList>
          {(companyID) => {
            const company = companiesByID.get(companyID);
            return (
              <ComboboxItem key={companyID} value={companyID}>
                <div className="flex min-w-0 flex-1 flex-col gap-0.5">
                  <div className="flex min-w-0 items-center gap-2">
                    <span className="min-w-0 flex-1 truncate font-medium">{company?.name ?? `企业 #${companyID}`}</span>
                    <span className="text-muted-foreground shrink-0 text-xs tabular-nums">
                      {company?.asset_count ?? 0} 个资产
                    </span>
                  </div>
                  {company && (
                    <span className="text-muted-foreground truncate text-xs" title={companyScopeSummary(company)}>
                      {companyScopeSummary(company)}
                    </span>
                  )}
                </div>
              </ComboboxItem>
            );
          }}
        </ComboboxList>
      </ComboboxContent>
    </Combobox>
  );
}

function assetPickerLabel(asset: Asset): string {
  const service = [asset.service_name, asset.port ? String(asset.port) : ""].filter(Boolean).join(":");
  return (
    [asset.domain, asset.ip, asset.url, asset.app_name, service].find((value) => Boolean(value?.trim()))?.trim() ??
    `资产 #${asset.id}`
  );
}

function TaskAssetPicker({
  value,
  onValueChange,
  portalContainer,
}: {
  value: number[];
  onValueChange: (value: number[]) => void;
  portalContainer?: React.RefObject<HTMLElement | null>;
}) {
  const [assets, setAssets] = React.useState<Asset[]>([]);
  const [loading, setLoading] = React.useState(false);
  React.useEffect(() => {
    let active = true;
    setLoading(true);
    const loadType = async (type: (typeof TASK_ASSET_TYPES)[number]) => {
      const first = await api.assets(type, 200, 0);
      const result = [...first.assets];
      for (let offset = result.length; offset < first.total; offset += 200) {
        const next = await api.assets(type, 200, offset);
        result.push(...next.assets);
        if (next.assets.length === 0) break;
      }
      return result;
    };
    Promise.all(TASK_ASSET_TYPES.map(loadType))
      .then((pages) => {
        if (!active) return;
        const byID = new Map<number, Asset>();
        for (const page of pages) for (const asset of page) byID.set(asset.id, asset);
        setAssets([...byID.values()]);
      })
      .catch(() => {
        if (active) setAssets([]);
      })
      .finally(() => active && setLoading(false));
    return () => {
      active = false;
    };
  }, []);
  const byID = React.useMemo(() => new Map(assets.map((asset) => [String(asset.id), asset])), [assets]);
  const ids = React.useMemo(() => assets.map((asset) => String(asset.id)), [assets]);
  const selectedIDs = React.useMemo(() => value.map(String), [value]);
  const atLimit = value.length >= MAX_TASK_ASSETS;
  return (
    <Combobox
      items={ids}
      multiple
      value={selectedIDs}
      itemToStringValue={(id) => {
        const asset = byID.get(id);
        return asset ? `${assetPickerLabel(asset)} ${asset.type} ${asset.url ?? ""}` : id;
      }}
      onValueChange={(next) => onValueChange(next.slice(0, MAX_TASK_ASSETS).map(Number))}
    >
      <ComboboxChips>
        <ComboboxValue>
          {selectedIDs.map((id) => (
            <ComboboxChip key={id}>{assetPickerLabel(byID.get(id) ?? ({ id: Number(id) } as Asset))}</ComboboxChip>
          ))}
        </ComboboxValue>
        <ComboboxChipsInput
          id="task-assets"
          placeholder={
            loading
              ? "加载资产中…"
              : atLimit
                ? `最多关联 ${MAX_TASK_ASSETS} 项资产`
                : "搜索名称、域名、IP、URL 或应用名"
          }
          disabled={loading || atLimit}
        />
      </ComboboxChips>
      <ComboboxContent portalContainer={portalContainer}>
        <ComboboxEmpty>{loading ? "加载资产中…" : "没有匹配的资产"}</ComboboxEmpty>
        <ComboboxList>
          {(id: string) => {
            const asset = byID.get(id);
            if (!asset) return null;
            return (
              <ComboboxItem key={id} value={id} disabled={atLimit && !selectedIDs.includes(id)}>
                <div className="flex min-w-0 flex-1 items-center gap-2">
                  <span className="min-w-0 flex-1 truncate">{assetPickerLabel(asset)}</span>
                  <Badge variant="outline" className="shrink-0">
                    {asset.type}
                  </Badge>
                  <code className="text-muted-foreground shrink-0 text-xs">#{asset.id}</code>
                </div>
              </ComboboxItem>
            );
          }}
        </ComboboxList>
      </ComboboxContent>
    </Combobox>
  );
}

export function TaskCreationForm() {
  const router = useRouter();
  const [details, setDetails] = React.useState(false);
  const [tasks, setTasks] = React.useState<Task[]>([]);
  const [categories, setCategories] = React.useState<TaskCategory[]>([]);
  const [optionsError, setOptionsError] = React.useState("");
  const [optionsLoading, setOptionsLoading] = React.useState(false);
  const onCategoriesChanged = () => {
    void api
      .taskCategories()
      .then(setCategories)
      .catch((e) => toast.error(e.message));
  };
  const [name, setName] = React.useState("");
  const [categoryID, setCategoryID] = React.useState<number | undefined>(undefined);
  const [description, setDescription] = React.useState("");
  const [goal, setGoal] = React.useState("");
  const [selectedTemplateID, setSelectedTemplateID] = React.useState<number | null>(null);
  const [assetApprovalTemplate, setAssetApprovalTemplate] = React.useState<AssetApprovalTemplate>("all_assets");
  const [profiles, setProfiles] = React.useState<LLMProfile[]>([]);
  const [companies, setCompanies] = React.useState<Company[]>([]);
  const [sourceTaskIDs, setSourceTaskIDs] = React.useState<string[]>([]);
  const [companyIDs, setCompanyIDs] = React.useState<number[]>([]);
  const [assetIDs, setAssetIDs] = React.useState<number[]>([]);
  const [llmProfileIDs, setLLMProfileIDs] = React.useState<string[]>([]);
  const [creating, setCreating] = React.useState(false);
  const [timeoutMin, setTimeoutMin] = React.useState(""); // 任务级超时(分钟);空/0 = 不限时
  const [heartbeatMin, setHeartbeatMin] = React.useState("10"); // planner 心跳(分钟);默认10,下限10(与后端一致)
  const [seedFirstIntent, setSeedFirstIntent] = React.useState(false); // 创建时下发种子意图,worker 免等首轮 planner 直接开跑;默认关闭,走标准先规划再执行
  const [coverageEnabled, setCoverageEnabled] = React.useState(true); // 资产覆盖度功能;默认开。关闭=不计算/展示覆盖度、不累积范围、隐藏范围类工具(company 关联不受影响)
  // 方式1 文件上传:建任务前把文件暂存到 drafts/<draftId>/uploads/,拿回绝对路径追加进描述。
  const [uploading, setUploading] = React.useState(false);
  const [uploadCount, setUploadCount] = React.useState(0);
  const draftIdRef = React.useRef<string>("");
  const fileInputRef = React.useRef<HTMLInputElement>(null);
  const sheetContentRef = React.useRef<HTMLDivElement>(null);

  const optionsLoaded = React.useRef(false);
  const loadOptions = React.useCallback(async () => {
    setOptionsLoading(true);
    setOptionsError("");
    try {
      const [ps, cs, ts, cats] = await Promise.all([
        api.llmProfiles(),
        api.companies(),
        api.tasks(),
        api.taskCategories(),
      ]);
      optionsLoaded.current = true;
      setProfiles(ps);
      setCompanies(cs);
      setTasks(ts.tasks);
      setCategories(cats);
    } catch (e) {
      setOptionsError((e as Error).message);
    } finally {
      setOptionsLoading(false);
    }
  }, []);
  React.useEffect(() => {
    if (details && !optionsLoaded.current) void loadOptions();
  }, [details, loadOptions]);

  // pickFiles uploads the chosen files into this draft's staging dir and appends their
  // absolute paths to the description; the task's agents open them by path via Read/Bash.
  async function pickFiles(files: FileList | null) {
    if (!files || files.length === 0) return;
    // crypto.randomUUID 仅在安全上下文可用(https/localhost);经 IP+http 访问时降级。
    if (!draftIdRef.current) {
      draftIdRef.current =
        globalThis.crypto?.randomUUID?.() ?? `d${Date.now().toString(36)}${Math.random().toString(36).slice(2, 10)}`;
    }
    setUploading(true);
    try {
      const r = await api.chatUpload("staging", draftIdRef.current, Array.from(files));
      setDescription((prev) => appendUploads(prev, r.attachments));
      setUploadCount((n) => n + r.attachments.length);
    } catch (e) {
      toast.error("上传失败：" + (e as Error).message);
    } finally {
      setUploading(false);
      if (fileInputRef.current) fileInputRef.current.value = ""; // allow re-picking the same file
    }
  }

  async function createTask() {
    if (creating || uploading) return;
    if (!description.trim() || !goal.trim()) {
      toast.error("请填写描述与目标");
      return;
    }
    if (sourceTaskIDs.length > MAX_SOURCE_TASKS) {
      toast.error(`最多关联 ${MAX_SOURCE_TASKS} 个来源任务`);
      return;
    }
    setCreating(true);
    try {
      const timeoutSec = Math.max(0, Math.floor(Number(timeoutMin) || 0)) * 60;
      const heartbeatSec = Math.max(10, Math.floor(Number(heartbeatMin) || 10)) * 60; // 下限 10min，与后端归一一致
      const created = await api.createTask({
        name: name.trim(),
        categoryId: categoryID,
        description: description.trim(),
        goal: goal.trim(),
        llmProfileIds: llmProfileIDs.map(Number),
        sourceTaskIds: sourceTaskIDs,
        companyIds: companyIDs,
        assetIds: assetIDs,
        assetApprovalTemplate,
        timeoutSeconds: timeoutSec,
        seedFirstIntent,
        planHeartbeatSeconds: heartbeatSec,
        coverageEnabled,
      });
      toast.success("任务已创建");
      router.push(`/function/tasks/detail?id=${encodeURIComponent(created.id)}`);
    } catch (e) {
      toast.error("创建失败：" + (e as Error).message);
    } finally {
      setCreating(false);
    }
  }

  return (
    <div ref={sheetContentRef} className="mx-auto flex w-full max-w-[900px] flex-col gap-8 pb-12 pt-10 md:pt-[10vh]">
      <header className="flex flex-col gap-3 text-center">
        <h1 className="text-3xl! font-semibold tracking-tight">开始一项新的安全任务</h1>
        <p className="text-sm text-muted-foreground">描述测试对象与背景，接下来确认目标和执行配置。</p>
      </header>
      <InputGroup className="task-composer rounded-3xl bg-card has-disabled:bg-card has-disabled:opacity-100 dark:bg-card">
        <InputGroupTextarea
          id="description"
          aria-label="任务描述"
          placeholder="描述你希望完成的任务…"
          value={description}
          onChange={(e) => setDescription(e.target.value)}
          className="min-h-36 max-h-72 px-5 pt-5 text-base"
        />
        <InputGroupAddon align="block-end">
          <input
            ref={fileInputRef}
            type="file"
            multiple
            className="hidden"
            onChange={(e) => void pickFiles(e.target.files)}
          />
          <InputGroupButton
            aria-label="上传附件"
            title="上传附件"
            size="icon-sm"
            onClick={() => fileInputRef.current?.click()}
            disabled={uploading || creating}
          >
            {uploading ? <Spinner /> : <PaperclipIcon />}
          </InputGroupButton>
          <InputGroupButton aria-label="任务配置" title="任务配置" size="icon-sm" onClick={() => setDetails(true)}>
            <Settings2 />
          </InputGroupButton>
          <span className="text-xs text-muted-foreground">
            {uploadCount > 0 ? `已上传 ${uploadCount} 个文件` : "支持上传测试资料"}
          </span>
          {!details && (
            <InputGroupButton
              className="ml-auto"
              variant="default"
              size="icon-sm"
              aria-label="继续，填写任务目标"
              onClick={() => setDetails(true)}
              disabled={!description.trim() || uploading}
            >
              <ArrowUp />
            </InputGroupButton>
          )}
        </InputGroupAddon>
      </InputGroup>
      {details && (
        <Card>
          <CardHeader>
            <CardTitle>确认任务目标</CardTitle>
            <CardDescription>确认后创建任务；高级选项沿用当前系统默认值。</CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-5">
            {optionsLoading && (
              <p role="status" className="text-muted-foreground">
                加载任务配置…
              </p>
            )}
            {optionsError && (
              <div role="alert" className="flex items-center gap-2 text-destructive">
                配置加载失败：{optionsError}
                <Button variant="outline" onClick={() => void loadOptions()}>
                  重试
                </Button>
              </div>
            )}
            <div className="grid gap-2">
              <Label htmlFor="name">名称（可选）</Label>
              <Input
                id="name"
                placeholder="给任务起个便于识别的名字，例如：Acme 官网渗透"
                value={name}
                onChange={(e) => setName(e.target.value)}
              />
            </div>
            <Field>
              <FieldLabel htmlFor="task-category">任务分类</FieldLabel>
              <CategoryPicker
                categories={categories}
                value={categoryID}
                onValueChange={setCategoryID}
                onCategoryCreated={onCategoriesChanged}
                portalContainer={sheetContentRef}
              />
              <FieldDescription>可选，单个分类；用于任务列表筛选和归档，不影响 Agent 执行。</FieldDescription>
            </Field>
            <AssetApprovalTemplateField
              value={assetApprovalTemplate}
              onChange={setAssetApprovalTemplate}
              disabled={creating}
            />
            <div className="grid gap-2">
              <Label htmlFor="goal">目标</Label>
              <Textarea
                id="goal"
                className="min-h-32"
                placeholder="要达成什么，例如：拿下后台管理权限、获取服务器权限"
                value={goal}
                onChange={(e) => setGoal(e.target.value)}
              />
            </div>
            <Collapsible>
              <CollapsibleTrigger asChild>
                <Button variant="ghost" className="justify-start">
                  <ChevronRightIcon data-icon="inline-start" />
                  关联任务与资产
                </Button>
              </CollapsibleTrigger>
              <CollapsibleContent className="flex flex-col gap-5 pt-4">
                <Field>
                  <FieldLabel htmlFor="source-tasks">关联任务</FieldLabel>
                  <SourceTaskPicker
                    tasks={tasks}
                    value={sourceTaskIDs}
                    onValueChange={setSourceTaskIDs}
                    portalContainer={sheetContentRef}
                  />
                  <FieldDescription>
                    最多关联 {MAX_SOURCE_TASKS}{" "}
                    个任务。实时只读继承所选任务的持久化黑板、资产范围及相关流量；新任务写入独立黑板。
                  </FieldDescription>
                </Field>
                <Field>
                  <FieldLabel htmlFor="task-companies">关联企业资产范围</FieldLabel>
                  <CompanyPicker
                    companies={companies}
                    value={companyIDs}
                    onValueChange={setCompanyIDs}
                    portalContainer={sheetContentRef}
                  />
                  <FieldDescription>
                    创建任务时会将所选企业当前已有资产加入“测试资产”，并将域名、IP、CIDR、ICP 和企业关键词提供给 Agent
                    作为范围上下文；不会自动生成意图或强制改变执行目标。
                  </FieldDescription>
                </Field>
                <Field>
                  <FieldLabel htmlFor="task-assets">关联测试资产</FieldLabel>
                  <TaskAssetPicker value={assetIDs} onValueChange={setAssetIDs} portalContainer={sheetContentRef} />
                  <FieldDescription>
                    {assetIDs.length > 0 ? `已选择 ${assetIDs.length} 项资产。` : "尚未选择独立资产。"}{" "}
                    从全局资产库选择的资产会与企业快照一起加入任务范围，最多 {MAX_TASK_ASSETS} 项。
                  </FieldDescription>
                </Field>
              </CollapsibleContent>
            </Collapsible>
            <Field>
              <FieldLabel htmlFor="llm-profiles">LLM 配置链</FieldLabel>
              <TaskLLMProfileChain
                profiles={profiles}
                value={llmProfileIDs}
                onValueChange={setLLMProfileIDs}
                inputId="llm-profiles"
                portalContainer={sheetContentRef}
              />
              <FieldDescription>按列表顺序故障转移；第一项为当前配置，仅在明确额度不足时切换下一项。</FieldDescription>
            </Field>

            {/* 高级参数默认折叠:超时/心跳/首个意图,展开才占空间,常用路径保持清爽。 */}
            <Collapsible>
              <CollapsibleTrigger className="group flex w-full items-center gap-2 border-t pt-4 text-sm font-medium">
                <ChevronRightIcon className="text-muted-foreground size-4 transition-transform group-data-[state=open]:rotate-90" />
                高级设置
                <span className="text-muted-foreground ml-auto text-xs font-normal">超时 · 心跳 · 首个意图</span>
              </CollapsibleTrigger>
              <CollapsibleContent className="grid gap-5 pt-5">
                <div className="grid gap-2">
                  <Label htmlFor="timeout-min">任务超时（分钟，可选）</Label>
                  <Input
                    id="timeout-min"
                    type="number"
                    min={0}
                    className="w-40"
                    placeholder="留空 = 不限时"
                    value={timeoutMin}
                    onChange={(e) => setTimeoutMin(e.target.value)}
                  />
                  <p className="text-muted-foreground text-xs">
                    到点后触发优雅收尾（各 agent 写回 + planner 终局判定），任务进入 timeout 终态。
                  </p>
                </div>
                <div className="grid gap-2">
                  <Label htmlFor="heartbeat-min">planner 心跳（分钟）</Label>
                  <Input
                    id="heartbeat-min"
                    type="number"
                    min={10}
                    className="w-40"
                    placeholder="默认 10"
                    value={heartbeatMin}
                    onChange={(e) => setHeartbeatMin(e.target.value)}
                  />
                  <p className="text-muted-foreground text-xs">
                    距上轮规划结束/任务开始满该时长且期间无触发，自动触发一轮规划（兜底卡死 + 唤醒去监督在跑的
                    worker）。下限 10 分钟。
                  </p>
                </div>
                <div className="grid gap-2">
                  <label htmlFor="seed-first-intent" className="flex items-center gap-2 text-sm">
                    <Checkbox
                      id="seed-first-intent"
                      checked={seedFirstIntent}
                      onCheckedChange={(v) => setSeedFirstIntent(!!v)}
                    />
                    直接下发首个意图（描述+目标）
                  </label>
                  <p className="text-muted-foreground text-xs">
                    开启后创建即把「描述+目标」作为一条意图下发，worker 免等首轮规划直接开跑，跑完再由 planner
                    接手判定/补充。CTF 等常一个 work 直接解决的场景推荐开启；关闭则走标准的先规划再执行。
                  </p>
                </div>
                <div className="grid gap-2">
                  <label htmlFor="coverage-enabled" className="flex items-center gap-2 text-sm">
                    <Checkbox
                      id="coverage-enabled"
                      checked={coverageEnabled}
                      onCheckedChange={(v) => setCoverageEnabled(!!v)}
                    />
                    资产覆盖度功能
                  </label>
                  <p className="text-muted-foreground text-xs">
                    默认开启：计算并展示测试覆盖度、态势图显示测试进度、自动累积测试范围。关闭后不再计算/展示覆盖度，
                    态势图仅展示资产不显示进度，agent 也不再获得范围类工具。关闭不影响「关联企业资产范围」。
                  </p>
                </div>
              </CollapsibleContent>
            </Collapsible>

            <div className="flex justify-end gap-2 border-t pt-5">
              <Button variant="ghost" onClick={() => setDetails(false)} disabled={creating}>
                返回描述
              </Button>
              <Button
                onClick={() => void createTask()}
                disabled={
                  creating || uploading || optionsLoading || !!optionsError || !description.trim() || !goal.trim()
                }
              >
                {creating && <Spinner data-icon="inline-start" />}
                {creating ? "创建中…" : "创建任务"}
              </Button>
            </div>
          </CardContent>
        </Card>
      )}
      <TaskTemplateControls
        presentation="cards"
        description={description}
        goal={goal}
        selectedTemplateID={selectedTemplateID}
        onSelectedTemplateIDChange={setSelectedTemplateID}
        onApply={(template) => {
          setDescription(template.description);
          setGoal(template.goal);
          setUploadCount(0);
          setDetails(true);
        }}
        portalContainer={sheetContentRef}
      />
    </div>
  );
}
