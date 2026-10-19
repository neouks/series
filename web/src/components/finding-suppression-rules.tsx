"use client";

import * as React from "react";
import { ShieldBanIcon } from "lucide-react";
import { api } from "@/lib/api";
import type { FindingSuppressionRule } from "@/lib/types";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Card, CardHeader, CardTitle, CardDescription, CardContent, CardFooter } from "@/components/ui/card";
import { Sheet, SheetTrigger, SheetContent, SheetHeader, SheetTitle, SheetDescription } from "@/components/ui/sheet";
import {
  AlertDialog,
  AlertDialogContent,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogCancel,
} from "@/components/ui/alert-dialog";
import { Spinner } from "@/components/ui/spinner";

export function FindingSuppressionRules({ taskId, readOnly = false }: { taskId: string; readOnly?: boolean }) {
  const [open, setOpen] = React.useState(false);
  const [items, setItems] = React.useState<FindingSuppressionRule[]>([]);
  const [next, setNext] = React.useState<number | null>(null);
  const [loading, setLoading] = React.useState(false);
  const [error, setError] = React.useState("");
  const [confirm, setConfirm] = React.useState<FindingSuppressionRule | null>(null);
  const [saving, setSaving] = React.useState(false);
  const request = React.useRef(0);
  const scope = React.useRef(0);
  const submitting = React.useRef(false);
  const retryBefore = React.useRef(0);
  const load = React.useCallback(
    async (before = 0) => {
      const seq = ++request.current;
      retryBefore.current = before;
      setLoading(true);
      setError("");
      try {
        const page = await api.findingSuppressionRules(taskId, before);
        if (seq !== request.current) return;
        setItems((old) =>
          before ? [...old, ...page.items.filter((item) => !old.some((r) => r.id === item.id))] : page.items,
        );
        setNext(page.has_more ? page.next_before : null);
      } catch (e) {
        if (seq === request.current) setError((e as Error).message);
      } finally {
        if (seq === request.current) setLoading(false);
      }
    },
    [taskId],
  );
  React.useEffect(() => {
    scope.current++;
    submitting.current = false;
    setSaving(false);
    setConfirm(null);
    if (open) {
      setItems([]);
      setNext(null);
      void load();
    }
    return () => {
      scope.current++;
      request.current++;
    };
  }, [open, load]);
  async function revoke() {
    if (!confirm || submitting.current || readOnly) return;
    submitting.current = true;
    setSaving(true);
    setError("");
    const id = confirm.id;
    const scopeId = scope.current;
    try {
      await api.revokeFindingSuppression(taskId, id);
      if (scopeId !== scope.current) return;
      setItems((old) => old.map((r) => (r.id === id ? { ...r, revoked_at: new Date().toISOString() } : r)));
      setConfirm(null);
    } catch (e) {
      if (scopeId === scope.current) setError((e as Error).message);
    } finally {
      if (scopeId === scope.current) {
        submitting.current = false;
        setSaving(false);
      }
    }
  }
  return (
    <Sheet
      open={open}
      onOpenChange={(value) => {
        if (!saving) setOpen(value);
      }}
    >
      <SheetTrigger asChild>
        <Button variant="outline" size="sm">
          <ShieldBanIcon data-icon="inline-start" />
          禁报规则
        </Button>
      </SheetTrigger>
      <SheetContent className="w-full sm:max-w-xl">
        <SheetHeader>
          <SheetTitle>漏洞禁报规则</SheetTitle>
          <SheetDescription>
            仅影响当前任务。删除原因同步给 Planner、Worker 和主
            Agent；审核异常时按约定允许上报并记录异常。只有用户可以解除规则。
          </SheetDescription>
        </SheetHeader>
        <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto px-4 pb-4">
          {readOnly && (
            <Alert>
              <AlertDescription>关联任务视图只读；此处展示当前任务规则，切换到当前任务视图后可解除。</AlertDescription>
            </Alert>
          )}
          {error && !confirm && (
            <Alert variant="destructive">
              <AlertDescription>
                {error}
                <Button variant="outline" size="sm" disabled={loading} onClick={() => void load(retryBefore.current)}>
                  重试
                </Button>
              </AlertDescription>
            </Alert>
          )}
          {!loading && !error && items.length === 0 && (
            <Alert>
              <AlertDescription>暂无禁报规则。删除当前任务的漏洞后将在这里显示。</AlertDescription>
            </Alert>
          )}
          {items.map((rule) => (
            <Card key={rule.id}>
              <CardHeader>
                <div className="flex items-start justify-between gap-2">
                  <CardTitle className="break-words">{rule.title || rule.vulnclass || "未命名漏洞"}</CardTitle>
                  <Badge variant={rule.revoked_at ? "secondary" : "outline"}>
                    {rule.revoked_at ? "已解除" : "有效"}
                  </Badge>
                </div>
                <CardDescription>
                  {rule.vulnclass} · {new Date(rule.created_at).toLocaleString()}
                </CardDescription>
              </CardHeader>
              <CardContent>
                <p className="whitespace-pre-wrap break-words">
                  {rule.reason || "未填写原因：禁止同一目标上的同一漏洞重复上报，不扩大到整类漏洞。"}
                </p>
              </CardContent>
              <CardFooter>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={readOnly || Boolean(rule.revoked_at) || saving}
                  onClick={() => {
                    setError("");
                    setConfirm(rule);
                  }}
                >
                  解除禁报
                </Button>
              </CardFooter>
            </Card>
          ))}
          {loading && <Spinner className="self-center" />}
          {next !== null && (
            <Button variant="outline" disabled={loading} onClick={() => void load(next)}>
              加载更多
            </Button>
          )}
        </div>
        <AlertDialog
          open={confirm !== null}
          onOpenChange={(value) => {
            if (!value && !saving) {
              setConfirm(null);
              setError("");
            }
          }}
        >
          <AlertDialogContent>
            <AlertDialogHeader>
              <AlertDialogTitle>解除此禁报规则？</AlertDialogTitle>
              <AlertDialogDescription>
                解除“{confirm?.title || confirm?.vulnclass}
                ”的规则后，允许后续重新登记。不会恢复已删除漏洞，也不会自动启动任务。
              </AlertDialogDescription>
            </AlertDialogHeader>
            {error && (
              <Alert variant="destructive">
                <AlertDescription>{error}</AlertDescription>
              </Alert>
            )}
            <AlertDialogFooter>
              <AlertDialogCancel disabled={saving}>取消</AlertDialogCancel>
              <Button disabled={saving} onClick={() => void revoke()}>
                {saving && <Spinner data-icon="inline-start" />}确认解除
              </Button>
            </AlertDialogFooter>
          </AlertDialogContent>
        </AlertDialog>
      </SheetContent>
    </Sheet>
  );
}
