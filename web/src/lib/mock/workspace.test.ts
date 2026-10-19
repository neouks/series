import assert from "node:assert/strict";
import test from "node:test";
import { mockHandle } from "./handler";

test("templates only create a task after explicit submission", async () => {
  const before = await mockHandle<{ tasks: { id: string }[] }>("GET", "/tasks");
  const templates = await mockHandle<{ templates: { id: number; description: string; goal: string }[] }>(
    "GET",
    "/task-templates",
  );
  assert.ok(templates.templates.length > 0);
  const template = templates.templates[0];
  assert.ok(template.description && template.goal);
  assert.equal((await mockHandle<{ tasks: unknown[] }>("GET", "/tasks")).tasks.length, before.tasks.length);
  const created = await mockHandle<{ id: string }>(
    "POST",
    "/tasks",
    JSON.stringify({
      description: template.description,
      goal: template.goal,
      asset_approval_template: "explicit_targets",
      name: "UI flow regression",
    }),
  );
  assert.ok(created.id);
  assert.deepEqual(await mockHandle("GET", `/exploration/intents?task=${created.id}`), []);
  assert.deepEqual(
    (await mockHandle<{ items: unknown[] }>("GET", `/exploration/activity?task=${created.id}`)).items,
    [],
  );
  const after = await mockHandle<{
    tasks: { id: string; description: string; goal: string; asset_approval_template: string }[];
  }>("GET", "/tasks");
  const task = after.tasks.find((t) => t.id === created.id)!;
  assert.equal(task.goal, template.goal);
  assert.equal(task.description, template.description);
  assert.equal(task.asset_approval_template, "explicit_targets");
  assert.equal(after.tasks.length, before.tasks.length + 1);
  await assert.rejects(
    mockHandle(
      "POST",
      "/tasks",
      JSON.stringify({ description: "draft remains", goal: "goal remains", asset_approval_template: "invalid" }),
    ),
    /审批模板/,
  );
  assert.equal((await mockHandle<{ tasks: unknown[] }>("GET", "/tasks")).tasks.length, after.tasks.length);
});
