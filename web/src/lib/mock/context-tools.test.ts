import assert from "node:assert/strict";
import test from "node:test";
import { tools } from "./data";

test("context catalog replaces retired callable names without changing history fixtures", () => {
  const retired = ["list_facts", "list_findings", "list_goals", "node_detail", "get_worker_output", "get_worker_trace", "search_all_worker_traces", "list_worker_traces", "list_assets", "list_untested_assets", "traffic_blob", "expand_digest", "goal_met", "get_task_graph"];
  for (const name of retired) assert.equal(tools.some((t) => t.key === name), false, name);
  for (const name of ["context_search", "context_get", "asset_search", "asset_get", "traffic_search", "traffic_get", "graph_overview"]) {
    const tool = tools.find((t) => t.key === name);
    assert.ok(tool?.system, name);
    assert.ok(tool?.schema.properties, name);
  }
  assert.deepEqual(tools.find((t) => t.key === "context_get")?.schema.required, ["items"]);
  assert.deepEqual(tools.find((t) => t.key === "asset_get")?.schema.required, ["ids"]);
});

test("context catalog advertises item budgets and mixed activity search", () => {
  const get = tools.find((t) => t.key === "context_get")!;
  const cap = get.schema.properties.items.items.properties.max_chars;
  assert.equal(cap.type, "integer");
  assert.equal(cap.minimum, 1);
  assert.equal(cap.maximum, 24000);
  assert.equal(get.schema.properties.max_chars.default, 8000);
  const search = tools.find((t) => t.key === "context_search")!;
  assert.match(search.description, /可与节点类型混合/);
  assert.ok(search.schema.properties.types.items.enum.includes("activity"));
});
