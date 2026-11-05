"use client";

import * as React from "react";
import { ExplorationGraph } from "@/components/exploration-graph";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { api } from "@/lib/api";
import { GraphWindowPoller } from "@/lib/graph-window";
import type { GraphWindow, GraphWindowQuery } from "@/lib/types";

export function FindingLineageView({ findingId }: { findingId: string }) {
  return <LineageWindow key={findingId} findingId={findingId} />;
}
function LineageWindow({ findingId }: { findingId: string }) {
  const [range, setRange] = React.useState<GraphWindowQuery>({});
  const [history, setHistory] = React.useState<GraphWindowQuery[]>([]);
  const [data, setData] = React.useState<(GraphWindow & { task_id?: string }) | null>(null);
  const [error, setError] = React.useState("");
  const poller = React.useRef<GraphWindowPoller | null>(null);
  React.useEffect(() => {
    setData(null);
    setError("");
    let task: string | undefined;
    const loader = new GraphWindowPoller(
      async (revision, signal) => {
        const result = await api.findingLineage(findingId, { ...range, revision }, signal);
        task = result.task_id;
        return result;
      },
      (page) => {
        setData({ ...page, task_id: task });
        setError("");
      },
      (e, invalid) => {
        if (invalid) setData(null);
        setError(e.message);
      },
      60000,
    );
    poller.current = loader;
    const visible = () => loader.setVisible(!document.hidden);
    visible();
    document.addEventListener("visibilitychange", visible);
    return () => {
      loader.stop();
      document.removeEventListener("visibilitychange", visible);
      if (poller.current === loader) poller.current = null;
    };
  }, [findingId, range]);
  return (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
        <span>漏洞祖先摘要窗口 · {data?.nodes?.length ?? 0} 个节点。未展示不代表没有链路。</span>
        <Button
          size="sm"
          variant="outline"
          disabled={!history.length}
          onClick={() => {
            setRange(history.at(-1)!);
            setHistory((h) => h.slice(0, -1));
          }}
        >
          上一窗口
        </Button>
        {data?.has_more && (
          <Button
            size="sm"
            variant="outline"
            onClick={() => {
              setHistory((h) => [...h.slice(-19), range]);
              setRange({ before: data.next_before, upper: data.upper });
            }}
          >
            查看较早祖先
          </Button>
        )}
        <Button size="sm" variant="ghost" onClick={() => void poller.current?.refresh()}>
          刷新
        </Button>
        {data?.edges_truncated && <span>当前关系有省略，可缩小窗口查看。</span>}
      </div>
      {error && (
        <Alert variant="destructive">
          <AlertDescription>
            {error}
            <Button size="sm" variant="link" onClick={() => void poller.current?.refresh()}>
              重试
            </Button>
          </AlertDescription>
        </Alert>
      )}
      <ExplorationGraph
        key={`${findingId}:${range.before ?? ""}`}
        nodes={data?.nodes ?? []}
        edges={data?.edges ?? []}
        taskId={data?.task_id}
        unfold
        className="h-[68vh]"
        emptyHint={error ? "链路加载失败" : data ? "无链路可展示（节点或任务可能已删除）" : "加载中…"}
      />
    </div>
  );
}
