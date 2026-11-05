import type { GraphWindow, TaskNode } from "./types";
import { HTTPError } from "./http-error";

// Preserve unchanged node identities and canvas positions without stringifying
// full payloads. Windows never contain bodies; a node's complete view is lazy.
export function mergeGraphWindow(previous: GraphWindow | null, incoming: GraphWindow): GraphWindow {
  if (incoming.unchanged) {
    if (!previous || previous.revision !== incoming.revision) throw new Error("图版本未命中，请重试");
    return previous;
  }
  if (incoming.delta) {
    if (!previous || previous.revision !== incoming.base_revision) throw new Error("图增量版本不匹配，请刷新");
    const d = incoming.delta;
    const nodes = new Map((previous.nodes ?? []).map((n) => [n.id, n]));
    for (const id of d.removed_nodes) nodes.delete(id);
    for (const n of d.nodes) nodes.set(n.id, n);
    const edgeKey = (e: { src: string; dst: string; rel: string }) => JSON.stringify([e.src, e.dst, e.rel]);
    const edges = new Map((previous.edges ?? []).map((e) => [edgeKey(e), e]));
    for (const e of d.removed_edges) edges.delete(edgeKey(e));
    for (const e of d.edges) edges.set(edgeKey(e), e);
    if (
      d.node_order.length > 400 ||
      d.edge_order.length > 800 ||
      d.node_order.some((id) => !nodes.has(id)) ||
      d.edge_order.some((e) => !edges.has(edgeKey(e)))
    )
      throw new Error("图增量不完整");
    incoming = {
      ...incoming,
      delta: undefined,
      nodes: d.node_order.map((id) => nodes.get(id)!),
      edges: d.edge_order.map((e) => edges.get(edgeKey(e))!),
    };
  }
  const old = new Map((previous?.nodes ?? []).map((n) => [n.id, n]));
  const fields: Array<keyof TaskNode> = [
    "id",
    "type",
    "state",
    "priority",
    "origin",
    "ts",
    "summary",
    "inherited",
    "source_task_id",
    "preview",
  ];
  const nodes = (incoming.nodes ?? []).map((n) => {
    const p = old.get(n.id);
    return p && fields.every((f) => p[f] === n[f]) ? p : n;
  });
  const sameNodes = previous?.nodes?.length === nodes.length && previous.nodes.every((n, i) => n === nodes[i]);
  const edges = incoming.edges ?? [];
  const sameEdges =
    previous?.edges?.length === edges.length &&
    previous.edges.every((e, i) => e.src === edges[i].src && e.dst === edges[i].dst && e.rel === edges[i].rel);
  return {
    ...incoming,
    nodes: sameNodes ? previous.nodes : nodes,
    edges: sameEdges ? previous.edges : edges,
  };
}

// One in-flight request per mounted/visible window; stale completions cannot
// publish or schedule another request. Poll after completion, not on intervals.
export class GraphWindowPoller {
  private controller: AbortController | null = null;
  private timer: ReturnType<typeof setTimeout> | null = null;
  private stopped = false;
  private visible = true;
  private generation = 0;
  private data: GraphWindow | null = null;
  private unavailable = false;

  constructor(
    private readonly fetch: (revision: string, signal: AbortSignal) => Promise<GraphWindow>,
    private readonly publish: (data: GraphWindow) => void,
    private readonly fail: (error: Error, invalidated: boolean) => void,
    private readonly period = 20000,
  ) {}

  setVisible(visible: boolean) {
    if (this.stopped) return;
    this.visible = visible;
    this.cancel();
    if (visible) void this.refresh();
  }

  private cancel() {
    this.generation++;
    this.controller?.abort();
    this.controller = null;
    if (this.timer !== null) clearTimeout(this.timer);
    this.timer = null;
    this.unavailable = false;
  }

  async refresh() {
    if (this.stopped || !this.visible || this.controller) return;
    if (this.timer !== null) clearTimeout(this.timer);
    this.timer = null;
    this.unavailable = false;
    const controller = new AbortController(),
      generation = ++this.generation;
    this.controller = controller;
    try {
      const page = await this.fetch(this.data?.revision ?? "", controller.signal);
      if (this.stopped || controller.signal.aborted || generation !== this.generation) return;
      const next = mergeGraphWindow(this.data, page);
      this.data = next;
      // Publish even unchanged confirmation to clear transient error feedback;
      // React retains the exact snapshot object and does not rebuild the graph.
      this.publish(next);
    } catch (error) {
      if (!this.stopped && !controller.signal.aborted && generation === this.generation) {
        this.unavailable = error instanceof HTTPError && [400, 403, 404, 410].includes(error.status);
        if (this.unavailable) this.data = null;
        this.fail(error instanceof Error ? error : new Error("探索图加载失败"), this.unavailable);
      }
    } finally {
      if (generation === this.generation) {
        this.controller = null;
        if (!this.stopped && this.visible && !this.unavailable)
          this.timer = setTimeout(() => void this.refresh(), this.period);
      }
    }
  }

  stop() {
    this.stopped = true;
    this.cancel();
  }
}
