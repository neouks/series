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
import type { ModelRequestDeadlines, Settings } from "@/lib/types";

const fields = [
  {
    key: "queue_seconds",
    label: "排队时限",
    max: 3600,
    description: "默认 180 秒。取得模型并发槽前最多等待多久；排队超时不会发送请求。",
  },
  {
    key: "header_seconds",
    label: "等待响应头",
    max: 3600,
    description: "默认 180 秒。包含连接和等待 HTTP 响应头的时间，不等于等待首个回答 Token。",
  },
  {
    key: "read_idle_seconds",
    label: "响应读取空闲时限",
    max: 3600,
    description: "默认 180 秒。阻塞读取响应时多久没有数据；处理已收到内容的时间不算网络空闲。",
  },
  {
    key: "total_seconds",
    label: "请求执行总时限",
    max: 7200,
    description: "默认 900 秒。取得并发槽后计时，包含限速、SDK 重试和读取；SSE 心跳不会重置总时限。",
  },
] as const;

type Draft = Record<keyof ModelRequestDeadlines, string>;
const empty: Draft = { queue_seconds: "", header_seconds: "", read_idle_seconds: "", total_seconds: "" };

export function ModelDeadlinesCard({
  loaded,
  deadlines,
  onSaved,
}: {
  loaded: boolean;
  deadlines?: ModelRequestDeadlines;
  onSaved: (settings: Settings) => void;
}) {
  const id = React.useId();
  const [draft, setDraft] = React.useState({ values: empty, saved: empty, ready: false });
  const [saving, setSaving] = React.useState(false);
  const [error, setError] = React.useState("");
  const dirty = draft.ready && fields.some(({ key }) => draft.values[key] !== draft.saved[key]);
  useSettingsDirty(dirty);
  const queue = deadlines?.queue_seconds;
  const headers = deadlines?.header_seconds;
  const idle = deadlines?.read_idle_seconds;
  const total = deadlines?.total_seconds;

  React.useEffect(() => {
    if (queue === undefined || headers === undefined || idle === undefined || total === undefined) return;
    const saved: Draft = {
      queue_seconds: String(queue),
      header_seconds: String(headers),
      read_idle_seconds: String(idle),
      total_seconds: String(total),
    };
    setDraft((previous) => ({
      values:
        previous.ready && fields.some(({ key }) => previous.values[key] !== previous.saved[key])
          ? previous.values
          : saved,
      saved,
      ready: true,
    }));
  }, [queue, headers, idle, total]);

  const invalid = (key: keyof ModelRequestDeadlines, max: number) =>
    draft.ready &&
    (!Number.isInteger(Number(draft.values[key])) || Number(draft.values[key]) < 1 || Number(draft.values[key]) > max);
  const totalTooSmall =
    draft.ready &&
    (Number(draft.values.total_seconds) < Number(draft.values.header_seconds) ||
      Number(draft.values.total_seconds) < Number(draft.values.read_idle_seconds));
  const disabled = !loaded || !deadlines || !draft.ready || saving;

  const save = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (disabled) return;
    if (fields.some(({ key, max }) => invalid(key, max)) || totalTooSmall) {
      setError("请填写有效秒数，总时限不能小于响应头或读取空闲时限。");
      return;
    }
    const value: ModelRequestDeadlines = {
      queue_seconds: Number(draft.values.queue_seconds),
      header_seconds: Number(draft.values.header_seconds),
      read_idle_seconds: Number(draft.values.read_idle_seconds),
      total_seconds: Number(draft.values.total_seconds),
    };
    setSaving(true);
    setError("");
    try {
      const result = await api.setSettings({ model_request_deadlines: value });
      const saved = result.model_request_deadlines;
      if (!saved) throw new Error("服务未返回模型请求时限，请检查后端版本");
      const next = Object.fromEntries(fields.map(({ key }) => [key, String(saved[key])])) as Draft;
      setDraft({ values: next, saved: next, ready: true });
      onSaved(result);
      toast.success("已保存模型请求时限，对后续请求生效");
    } catch (failure) {
      setError(`保存失败：${(failure as Error).message}`);
    } finally {
      setSaving(false);
    }
  };

  return (
    <Card className="mb-4 break-inside-avoid md:mb-6">
      <CardHeader>
        <CardTitle>模型请求时限</CardTitle>
        <CardDescription>
          单位为秒。新请求使用保存后的时限，正在执行的请求沿用原值。较慢的推理模型可适当增加时限；超时会停止请求，不自动重跑
          Worker。
        </CardDescription>
      </CardHeader>
      <CardContent>
        <form id={id} onSubmit={save}>
          <FieldGroup>
            {fields.map(({ key, label, max, description }) => {
              const hasError = invalid(key, max) || (key === "total_seconds" && totalTooSmall);
              return (
                <Field key={key} data-disabled={disabled} data-invalid={hasError}>
                  <FieldLabel htmlFor={`${id}-${key}`}>{label}（秒）</FieldLabel>
                  <Input
                    id={`${id}-${key}`}
                    type="number"
                    inputMode="numeric"
                    min={1}
                    max={max}
                    step={1}
                    required
                    disabled={disabled}
                    aria-invalid={hasError}
                    value={draft.values[key]}
                    onChange={(event) =>
                      setDraft((previous) => ({
                        ...previous,
                        values: { ...previous.values, [key]: event.target.value },
                      }))
                    }
                  />
                  <FieldDescription>{description}</FieldDescription>
                </Field>
              );
            })}
            {loaded && !deadlines && <FieldDescription>当前服务未提供模型请求时限配置。</FieldDescription>}
            {totalTooSmall && <FieldError>总时限不能小于响应头或读取空闲时限。</FieldError>}
            {error && <FieldError>{error}</FieldError>}
          </FieldGroup>
        </form>
      </CardContent>
      <CardFooter>
        <Button type="submit" form={id} disabled={disabled || !dirty}>
          {saving && <Loader2Icon data-icon="inline-start" className="animate-spin" />}
          保存模型请求时限
        </Button>
      </CardFooter>
    </Card>
  );
}
