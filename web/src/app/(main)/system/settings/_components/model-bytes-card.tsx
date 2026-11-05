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
import type { Settings } from "@/lib/types";

const fields = [
  {
    key: "request_mib",
    label: "请求输入上限",
    description: "默认16 MiB。预检输入文本与参数，并核对最终HTTP请求正文；超过上限不会继续发送。",
  },
  {
    key: "response_mib",
    label: "累计响应上限",
    description: "默认32 MiB。累计读取解码后的HTTP正文，包含SSE帧、心跳及重试响应；待确认工具批次另受相同额度限制。",
  },
] as const;
type Draft = Record<(typeof fields)[number]["key"], string>;
const empty: Draft = { request_mib: "", response_mib: "" };

export function ModelBytesCard({
  loaded,
  limits,
  onSaved,
}: {
  loaded: boolean;
  limits?: Settings["model_request_bytes"];
  onSaved: (settings: Settings) => void;
}) {
  const id = React.useId();
  const [draft, setDraft] = React.useState({ values: empty, saved: empty, ready: false });
  const [saving, setSaving] = React.useState(false);
  const [error, setError] = React.useState("");
  const dirty = draft.ready && fields.some(({ key }) => draft.values[key] !== draft.saved[key]);
  useSettingsDirty(dirty);
  const request = limits?.request_mib,
    response = limits?.response_mib;
  React.useEffect(() => {
    if (request === undefined || response === undefined) return;
    const saved = { request_mib: String(request), response_mib: String(response) };
    setDraft((previous) => ({
      values:
        previous.ready && fields.some(({ key }) => previous.values[key] !== previous.saved[key])
          ? previous.values
          : saved,
      saved,
      ready: true,
    }));
  }, [request, response]);
  const invalid = (key: keyof Draft) =>
    draft.ready &&
    (!Number.isInteger(Number(draft.values[key])) || Number(draft.values[key]) < 1 || Number(draft.values[key]) > 256);
  const disabled = !loaded || !limits || !draft.ready || saving;
  const save = async (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (disabled) return;
    if (fields.some(({ key }) => invalid(key))) {
      setError("请填写1–256之间的整数MiB。");
      return;
    }
    setSaving(true);
    setError("");
    try {
      const result = await api.setSettings({
        model_request_bytes: {
          request_mib: Number(draft.values.request_mib),
          response_mib: Number(draft.values.response_mib),
        },
      });
      if (!result.model_request_bytes) throw new Error("服务未返回模型字节限制，请检查后端版本");
      const saved = {
        request_mib: String(result.model_request_bytes.request_mib),
        response_mib: String(result.model_request_bytes.response_mib),
      };
      setDraft({ values: saved, saved, ready: true });
      onSaved(result);
      toast.success("已保存模型字节限制，对后续请求生效");
    } catch (failure) {
      setError(`保存失败：${(failure as Error).message}`);
    } finally {
      setSaving(false);
    }
  };
  return (
    <Card className="mb-4 break-inside-avoid md:mb-6">
      <CardHeader>
        <CardTitle>模型字节上限</CardTitle>
        <CardDescription>
          单位为MiB（1,048,576字节）。触限会停止本次请求，不自动重跑Worker；已收到的前缀按不完整结果记录。正在执行或排队的请求沿用原值。这不是模型Token窗口或进程RSS上限。
        </CardDescription>
      </CardHeader>
      <CardContent>
        <form id={id} onSubmit={save}>
          <FieldGroup>
            {fields.map(({ key, label, description }) => (
              <Field key={key} data-disabled={disabled} data-invalid={invalid(key)}>
                <FieldLabel htmlFor={`${id}-${key}`}>{label}（MiB）</FieldLabel>
                <Input
                  id={`${id}-${key}`}
                  type="number"
                  inputMode="numeric"
                  min={1}
                  max={256}
                  step={1}
                  required
                  disabled={disabled}
                  aria-invalid={invalid(key)}
                  value={draft.values[key]}
                  onChange={(event) =>
                    setDraft((previous) => ({ ...previous, values: { ...previous.values, [key]: event.target.value } }))
                  }
                />
                <FieldDescription>{description}</FieldDescription>
              </Field>
            ))}
            {loaded && !limits && <FieldDescription>当前服务未提供模型字节限制配置。</FieldDescription>}
            {error && <FieldError>{error}</FieldError>}
          </FieldGroup>
        </form>
      </CardContent>
      <CardFooter>
        <Button type="submit" form={id} disabled={disabled || !dirty}>
          {saving && <Loader2Icon data-icon="inline-start" className="animate-spin" />}保存字节限制
        </Button>
      </CardFooter>
    </Card>
  );
}
