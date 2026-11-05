"use client";

import * as React from "react";
import { ArrowLeft, RefreshCw } from "lucide-react";

import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Card, CardContent } from "@/components/ui/card";
import { ExplorationGraph } from "@/components/exploration-graph";
import { api } from "@/lib/api";
import { GraphWindowPoller } from "@/lib/graph-window";
import type { GraphWindow, GraphWindowQuery } from "@/lib/types";

export function GraphTab({ taskId }: { taskId: string }) {
  return <GraphWindowPanel key={taskId} taskId={taskId} />;
}

function GraphWindowPanel({ taskId }: { taskId: string }) {
  const [range, setRange] = React.useState<GraphWindowQuery>({});
  const [history, setHistory] = React.useState<GraphWindowQuery[]>([]);
  const [data, setData] = React.useState<GraphWindow | null>(null);
  const [error, setError] = React.useState("");
  const [input, setInput] = React.useState("");
  const poller = React.useRef<GraphWindowPoller | null>(null);

  React.useEffect(() => {
    setData(null);
    setError("");
    const loader = new GraphWindowPoller(
      (revision, signal) => api.explorationGraphWindow(taskId, { ...range, revision }, signal),
      (page) => {
        setData(page);
        setError("");
      },
      (e, invalidated) => {
        if (invalidated) setData(null);
        setError(e.message);
      },
    );
    poller.current = loader;
    const visibility = () => loader.setVisible(!document.hidden);
    visibility();
    document.addEventListener("visibilitychange", visibility);
    return () => {
      loader.stop();
      document.removeEventListener("visibilitychange", visibility);
      if (poller.current === loader) poller.current = null;
    };
  }, [taskId, range]);

  const navigate = (next: GraphWindowQuery) => {
    setHistory((h) => [...h.slice(-19), range]);
    setRange(next);
  };

  return (
    <Card>
      <CardContent className="flex flex-col gap-3 p-3">
        <div className="flex flex-wrap items-center gap-2">
          <Button
            variant="outline"
            size="sm"
            disabled={history.length === 0}
            onClick={() => {
              const prior = history.at(-1);
              if (prior) {
                setRange(prior);
                setHistory((h) => h.slice(0, -1));
              }
            }}
          >
            <ArrowLeft data-icon="inline-start" />
            返回范围
          </Button>
          <Button variant="outline" size="sm" onClick={() => void poller.current?.refresh()}>
            <RefreshCw data-icon="inline-start" />
            刷新
          </Button>
          {(range.focus || range.before) && (
            <Button variant="ghost" size="sm" onClick={() => navigate({})}>
              返回最新
            </Button>
          )}
          <form
            className="ml-auto flex items-center gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              if (/^[1-9]\d*$/.test(input)) navigate({ focus: input });
            }}
          >
            <Input
              aria-label="定位探索节点 ID"
              placeholder="节点 ID"
              value={input}
              onChange={(e) => setInput(e.target.value)}
              className="w-32"
            />
            <Button type="submit" variant="outline" size="sm" disabled={!/^[1-9]\d*$/.test(input)}>
              查看相邻节点
            </Button>
          </form>
        </div>
        {error && (
          <Alert variant="destructive">
            <AlertDescription>
              {error}。{data ? "保留上次成功窗口。" : "未加载此范围。"}
              <Button variant="link" size="sm" onClick={() => void poller.current?.refresh()}>
                重试
              </Button>
            </AlertDescription>
          </Alert>
        )}
        <div className="text-muted-foreground flex flex-wrap items-center gap-2 text-xs" aria-live="polite">
          <span>
            {data
              ? `${data.nodes?.length ?? 0} 个节点 · ${data.edges?.length ?? 0} 条关系`
              : error
                ? "图窗口不可用"
                : "正在加载图窗口…"}
          </span>
          <span>
            当前为{range.focus ? `节点 #${range.focus} 的相邻` : "最近节点"}
            摘要窗口，不代表完整探索图。点击节点按需读取正文。
          </span>
          {data?.edges_truncated && <span>当前关系已截断，可选中节点缩小范围。</span>}
          {data?.has_more && (
            <Button
              variant="outline"
              size="sm"
              onClick={() => navigate({ focus: range.focus, before: data.next_before, upper: data.upper })}
            >
              查看较早节点
            </Button>
          )}
        </div>
        <ExplorationGraph
          key={`${taskId}:${range.focus ?? ""}:${range.before ?? ""}`}
          taskId={taskId}
          unfold={Boolean(range.focus)}
          nodes={data?.nodes ?? []}
          edges={data?.edges ?? []}
          onExplore={(node) => navigate({ focus: node.id })}
          className="h-[68vh]"
          emptyHint={error ? "图窗口加载失败，可重试" : data ? "当前范围暂无节点" : "正在加载"}
        />
      </CardContent>
    </Card>
  );
}
