import assert from "node:assert/strict";
import test from "node:test";

import type { Finding, FindingSuppressionRule, Task } from "../types";
import { mockFindingSuppressionRules, mockHandle } from "./handler";

type RulesPage = { items: FindingSuppressionRule[]; has_more: boolean; next_before: number };

test("删除反馈创建任务禁报规则，空原因、分页、解除与继承隔离", async () => {
  const { tasks } = await mockHandle<{ tasks: Task[] }>("GET", "/tasks");
  const findings = await mockHandle<Finding[]>("GET", "/exploration/findings");
  const owner = tasks.find((task) => findings.filter((f) => f.task_id === task.id).length >= 2);
  assert.ok(owner);
  const selected = findings.filter((f) => f.task_id === owner.id).slice(0, 2);
  const base = `/tasks/${owner.id}/finding-suppression-rules`;
  for (const [i, finding] of selected.entries()) {
    await mockHandle(
      "DELETE",
      `/exploration/findings/${finding.id}`,
      JSON.stringify({ reason: i ? " \n " : "仅此接口，证据不足" }),
    );
  }
  const first = await mockHandle<RulesPage>("GET", `${base}?limit=1`);
  assert.equal(first.items.length, 1);
  assert.equal(first.items[0].reason, "");
  assert.equal(first.has_more, true);
  const second = await mockHandle<RulesPage>("GET", `${base}?limit=1&before=${first.next_before}`);
  assert.equal(second.items[0].reason, "仅此接口，证据不足");
  assert.equal(second.has_more, false);
  const rule = first.items[0];
  const other = tasks.find((t) => t.id !== owner.id);
  assert.ok(other);
  const otherBase = `/tasks/${other.id}/finding-suppression-rules`;
  assert.equal((await mockHandle<RulesPage>("GET", otherBase)).items.length, 0);
  await assert.rejects(mockHandle("POST", `${otherBase}/${rule.id}/revoke`), /rule not found/);
  await mockHandle("POST", `${base}/${rule.id}/revoke`);
  const revokedAt = mockFindingSuppressionRules.find((r) => r.id === rule.id)?.revoked_at;
  assert.ok(revokedAt);
  await mockHandle("POST", `${base}/${rule.id}/revoke`);
  assert.equal(mockFindingSuppressionRules.find((r) => r.id === rule.id)?.revoked_at, revokedAt);
  await assert.rejects(mockHandle("DELETE", `/exploration/findings/${selected[0].id}`));
  assert.equal((await mockHandle<RulesPage>("GET", base)).items.length, 2);
  for (const query of ["limit=51", "limit=0", "before=-1", "limit=NaN"]) {
    await assert.rejects(mockHandle("GET", `${base}?${query}`));
  }
});
