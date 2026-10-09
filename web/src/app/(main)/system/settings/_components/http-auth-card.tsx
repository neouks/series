"use client";

import { useEffect, useState } from "react";
import { ShieldCheckIcon } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardFooter, CardHeader, CardTitle } from "@/components/ui/card";
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Switch } from "@/components/ui/switch";
import { api } from "@/lib/api";

export function HTTPAuthCard() {
  const [enabled, setEnabled] = useState(false);
  const [username, setUsername] = useState("entry");
  const [password, setPassword] = useState("");
  const [passwordSet, setPasswordSet] = useState(false);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [reload, setReload] = useState(0);
  useEffect(() => {
    let active = true;
    setLoading(true);
    api.httpAuthSettings().then((value) => {
      if (!active) return;
      setEnabled(value.enabled); setUsername(value.username); setPasswordSet(value.password_set); setError("");
    }).catch((e: Error) => { if (active) setError(e.message); })
      .finally(() => { if (active) setLoading(false); });
    return () => { active = false; };
  }, [reload]);

  async function save(event: React.FormEvent) {
    event.preventDefault();
    setSaving(true);
    try {
      const value = await api.saveHTTPAuthSettings({ enabled, username: username.trim(), password });
      setEnabled(value.enabled); setUsername(value.username); setPasswordSet(value.password_set); setPassword("");
      toast.success("访问验证已保存，立即生效");
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "保存失败");
    } finally { setSaving(false); }
  }
  const disabled = loading || saving || !!error;
  return (
    <Card className="mb-4 break-inside-avoid md:mb-6">
      <CardHeader>
        <CardTitle className="flex items-center gap-2"><ShieldCheckIcon className="size-4" />HTTP Basic Auth</CardTitle>
        <CardDescription>开启后，访客需先通过浏览器原生账号密码验证，再进入 SERIES 登录页面。默认关闭，保存后立即生效。</CardDescription>
      </CardHeader>
      <form onSubmit={save}>
        <CardContent>
          <FieldGroup>
            <Field orientation="horizontal">
              <FieldLabel htmlFor="http-entry-enabled">启用访问验证</FieldLabel>
              <Switch id="http-entry-enabled" checked={enabled} onCheckedChange={setEnabled} disabled={disabled} />
            </Field>
            <Field>
              <FieldLabel htmlFor="http-entry-username">验证用户名</FieldLabel>
              <Input id="http-entry-username" value={username} onChange={(e) => setUsername(e.target.value)} maxLength={128} autoComplete="off" disabled={disabled} required />
              <FieldDescription>不能包含冒号，最长 128 字节。</FieldDescription>
            </Field>
            <Field>
              <FieldLabel htmlFor="http-entry-password">验证密码</FieldLabel>
              <Input id="http-entry-password" type="password" value={password} onChange={(e) => setPassword(e.target.value)} placeholder={passwordSet ? "已设置，留空保持原密码" : "首次启用时设置验证密码"} autoComplete="new-password" disabled={disabled} />
              <FieldDescription>与 SERIES 登录密码独立，至少 8 个字符、最多 72 字节。公网访问请使用 HTTPS。</FieldDescription>
            </Field>
            <FieldDescription>保存不会中断当前已登录浏览器访问。可用无痕窗口检查验证弹窗；浏览器可能缓存凭据。关闭后保留账号密码，便于再次开启。</FieldDescription>
            {error && <FieldDescription role="alert">加载失败：{error} <Button type="button" variant="link" onClick={() => setReload((v) => v + 1)}>重试</Button></FieldDescription>}
          </FieldGroup>
        </CardContent>
        <CardFooter className="mt-5">
          <Button type="submit" disabled={disabled}>{saving ? "正在保存…" : loading ? "正在加载…" : "保存访问验证"}</Button>
        </CardFooter>
      </form>
    </Card>
  );
}
