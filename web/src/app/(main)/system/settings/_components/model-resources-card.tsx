"use client";

import * as React from "react";

import { Loader2Icon } from "lucide-react";
import { toast } from "sonner";

import { useSettingsDirty } from "@/components/settings/settings-context";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardFooter, CardHeader, CardTitle } from "@/components/ui/card";
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { api } from "@/lib/api";
import type { ModelRequestLimits, Settings } from "@/lib/types";

const configs = {
  model: {
    key: "model_request_limits",
    target: "模型请求",
    label: "同时执行的模型请求",
    defaultConcurrency: 8,
    description:
      "所有模型配置和 Agent 共用请求上限，后台摘要与报告也计入。超额请求按任务轮流等待；队列满时明确返回错误。",
    queueDescription: "等待可取消；模型配置自身的速率限制仍然生效。",
  },
  worker: {
    key: "worker_execution_limits",
    target: "Worker 执行",
    label: "同时运行的 Worker",
    defaultConcurrency: 16,
    description:
      "自动领取和人工恢复共用本进程上限，同时受每任务并发设置限制。取得资源后才领取意图；降低上限不会中断正在运行的 Worker。",
    queueDescription: "暂停或取消会停止等待；人工消息最多等待30秒，失败时保留输入供重试。",
  },
  process: {
    key: "external_process_limits",
    target: "外部进程",
    label: "同时运行的外部作业",
    defaultConcurrency: 32,
    description:
      "Bash、Monitor、Grep、交互Shell、自定义command／Python脚本、stdio MCP及Shell检测／测试共用本进程上限。同步命令和脚本最多10分钟、输出32 MiB；Grep与依赖安装共用120秒取消期限。交互Shell输出最多32 MiB，保留256 KiB滚动内容。进程实际退出后释放许可，MCP在连接关闭后释放；该上限不等于操作系统子进程数或RSS限制。",
    queueDescription: "等待可取消，最多30秒；调用方更短的期限优先。测试和检测也计入资源占用，不计入Agent工具调用次数。",
  },
} as const;

export function ModelResourcesCard({
  kind = "model",
  loaded,
  limits,
  usage,
  onSaved,
}: {
  kind?: keyof typeof configs;
  loaded: boolean;
  limits?: ModelRequestLimits;
  usage?: Settings["model_request_usage"];
  onSaved: (settings: Settings) => void;
}) {
  const id = React.useId();
  const config = configs[kind];
  const limitsKey = config.key;
  const target = config.target;
  const [draft, setDraft] = React.useState({
    concurrency: "",
    queue: "",
    savedConcurrency: "",
    savedQueue: "",
    ready: false,
  });
  const [saving, setSaving] = React.useState(false);
  const [error, setError] = React.useState("");
  const dirty = draft.ready && (draft.concurrency !== draft.savedConcurrency || draft.queue !== draft.savedQueue);
  useSettingsDirty(dirty);
  const savedConcurrency = limits?.concurrency;
  const savedQueue = limits?.queue;

  React.useEffect(() => {
    if (savedConcurrency === undefined || savedQueue === undefined) return;
    const concurrency = String(savedConcurrency);
    const queue = String(savedQueue);
    setDraft((previous) => {
      const edited =
        previous.ready &&
        (previous.concurrency !== previous.savedConcurrency || previous.queue !== previous.savedQueue);
      return {
        concurrency: edited ? previous.concurrency : concurrency,
        queue: edited ? previous.queue : queue,
        savedConcurrency: concurrency,
        savedQueue: queue,
        ready: true,
      };
    });
  }, [savedConcurrency, savedQueue]);

  const concurrency = Number(draft.concurrency);
  const queue = Number(draft.queue);
  const invalidConcurrency = draft.ready && (!Number.isInteger(concurrency) || concurrency < 1 || concurrency > 128);
  const invalidQueue = draft.ready && (!Number.isInteger(queue) || queue < 1 || queue > 4096);
  const disabled = !loaded || !limits || saving;

  const save = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (disabled) return;
    if (invalidConcurrency || invalidQueue) {
      setError(`${target}并发数须为 1–128，等待队列须为 1–4096。`);
      return;
    }
    setSaving(true);
    setError("");
    try {
      const result = await api.setSettings({ [limitsKey]: { concurrency, queue } });
      const saved = result[limitsKey];
      if (!saved) throw new Error(`服务未返回${target}配置，请检查后端版本`);
      setDraft({
        concurrency: String(saved.concurrency),
        queue: String(saved.queue),
        savedConcurrency: String(saved.concurrency),
        savedQueue: String(saved.queue),
        ready: true,
      });
      onSaved(result);
      toast.success(`已保存${target}上限，立即生效`);
    } catch (failure) {
      setError(`保存失败：${(failure as Error).message}`);
    } finally {
      setSaving(false);
    }
  };

  return (
    <Card className="mb-4 break-inside-avoid md:mb-6">
      <CardHeader>
        <CardTitle>{target}资源</CardTitle>
        <CardDescription>{config.description}</CardDescription>
      </CardHeader>
      <CardContent>
        <form id={id} onSubmit={save}>
          <FieldGroup>
            <Field data-disabled={disabled} data-invalid={invalidConcurrency}>
              <FieldLabel htmlFor={`${id}-concurrency`}>{config.label}</FieldLabel>
              <Input
                id={`${id}-concurrency`}
                type="number"
                inputMode="numeric"
                min={1}
                max={128}
                step={1}
                required
                value={draft.concurrency}
                disabled={disabled}
                aria-invalid={invalidConcurrency}
                onChange={(event) => setDraft((previous) => ({ ...previous, concurrency: event.target.value }))}
              />
              <FieldDescription>
                默认 {config.defaultConcurrency}。降低上限后，已有执行自然结束，再按新上限安排后续工作。
              </FieldDescription>
            </Field>
            <Field data-disabled={disabled} data-invalid={invalidQueue}>
              <FieldLabel htmlFor={`${id}-queue`}>最多等待的请求</FieldLabel>
              <Input
                id={`${id}-queue`}
                type="number"
                inputMode="numeric"
                min={1}
                max={4096}
                step={1}
                required
                value={draft.queue}
                disabled={disabled}
                aria-invalid={invalidQueue}
                onChange={(event) => setDraft((previous) => ({ ...previous, queue: event.target.value }))}
              />
              <FieldDescription>
                默认 256。
                {config.queueDescription}
              </FieldDescription>
            </Field>
            {usage && (
              <FieldDescription>
                本次采样：执行中 {usage.active}，等待中 {usage.queued}。保存或重新打开设置时更新。
              </FieldDescription>
            )}
            {loaded && !limits && <FieldDescription>当前服务未提供{target}准入配置。</FieldDescription>}
            {error && <FieldError>{error}</FieldError>}
          </FieldGroup>
        </form>
      </CardContent>
      <CardFooter>
        <Button type="submit" form={id} disabled={disabled || !dirty}>
          {saving && <Loader2Icon data-icon="inline-start" className="animate-spin" />}
          保存{target}上限
        </Button>
      </CardFooter>
    </Card>
  );
}
