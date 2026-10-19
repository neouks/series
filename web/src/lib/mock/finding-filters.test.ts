import type { FindingsPage, Task } from "../types";
import { mockHandle } from "./handler";
import assert from "node:assert/strict";
import test from "node:test";

test("task finding filters run before pagination and preserve source isolation", async () => {
  const { tasks } = await mockHandle<{ tasks: Task[] }>("GET", "/tasks");
  const owner = tasks.find((task) => task.source_task_ids?.length);
  assert.ok(owner);
  const base = `/exploration/findings?context_task=${owner.id}`;
  const full = await mockHandle<FindingsPage>("GET", `${base}&limit=200`);
  assert.ok(full.total > 0);
  assert.ok(full.items.some((item) => item.inherited));
  for (const scope of ["all", "local", "inherited"]) {
    const expected = full.items.filter(
      (item) => scope === "all" || (scope === "local" ? !item.inherited : item.inherited),
    );
    const seen: string[] = [];
    for (let page = 1; page <= expected.length + 1; page++) {
      const result = await mockHandle<FindingsPage>("GET", `${base}&scope=${scope}&page=${page}&limit=1`);
      assert.equal(result.total, expected.length);
      assert.equal(result.items.length, page <= expected.length ? 1 : 0);
      for (const item of result.items) {
        assert.ok(item.task_id === owner.id || owner.source_task_ids?.includes(item.task_id ?? ""));
        assert.equal(item.evidence, "");
        seen.push(item.id);
      }
    }
    assert.equal(new Set(seen).size, expected.length);
  }
  const target = full.items.find((item) => item.name && item.finding_id);
  assert.ok(target?.name);
  const result = await mockHandle<FindingsPage>(
    "GET",
    `${base}&q=${encodeURIComponent(target.name.toUpperCase())}&status=${target.status}&severity=${target.severity}&limit=1`,
  );
  assert.ok(result.total >= 1);
  assert.equal(result.items[0].id, target.id);
  const empty = await mockHandle<FindingsPage>("GET", `${base}&q=%25_not_a_wildcard`);
  assert.equal(empty.total, 0);
  for (const filter of [
    "scope=recursive",
    "severity=bogus",
    "status=bogus",
    `q=${encodeURIComponent("中".repeat(201))}`,
  ]) {
    await assert.rejects(mockHandle("GET", `${base}&${filter}`), /invalid filter/);
  }
  await assert.rejects(mockHandle("GET", "/exploration/findings?context_task=unknown"), /task not found/);
});
