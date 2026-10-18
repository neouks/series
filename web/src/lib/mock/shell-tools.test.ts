import assert from "node:assert/strict";
import test from "node:test";
import { mockHandle } from "./handler";
import type { Tool } from "../types";

test("Shell interpreter is read-only and legacy writes fail before saving", async () => {
  const settings = await mockHandle<Record<string, unknown>>("GET", "/settings");
  assert.equal(Object.hasOwn(settings, "shell_mode"), false);
  const proxy = settings.global_proxy;
  for (const shell_mode of ["cmd", null, 42]) {
    await assert.rejects(mockHandle("PUT", "/settings", JSON.stringify({ shell_mode, global_proxy: "invalid" })), /shell_mode/);
    assert.equal((await mockHandle<Record<string, unknown>>("GET", "/settings")).global_proxy, proxy);
  }
  assert.equal((settings.shell_detected as { mode: string }).mode, "bash");
});

test("shell editor metadata roundtrips and testing does not increment usage", async () => {
  const key = "shell_probe_test";
  await mockHandle(
    "POST",
    "/tools/custom",
    JSON.stringify({ key, kind: "shell", executable: " sh ", enabled: true, agents: ["worker"] }),
  );
  const before = await mockHandle<{ tools: Tool[] }>("GET", "/tools");
  const tool = before.tools.find((tool) => tool.key === key)!;
  assert.equal(tool.executable, "sh");
  const success = await mockHandle<{ is_error: boolean; output: string }>(
    "POST",
    "/tools/custom/test",
    JSON.stringify({ kind: "shell", executable: "sh", action: "check" }),
  );
  assert.equal(success.is_error, false);
  assert.match(success.output, /sh/);
  const fail = await mockHandle<{ is_error: boolean }>(
    "POST",
    "/tools/custom/test",
    JSON.stringify({ kind: "shell", executable: "missing", action: "check" }),
  );
  assert.equal(fail.is_error, true);
  const after = await mockHandle<{ tools: Tool[] }>("GET", "/tools");
  assert.equal(after.tools.find((tool) => tool.key === key)!.calls, tool.calls);
  await mockHandle("DELETE", `/tools/custom/${key}`);
});
