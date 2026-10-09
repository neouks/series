import assert from "node:assert/strict";
import test from "node:test";
import { mockHandle } from "./handler";
import type { HTTPAuthSettings } from "../types";

test("HTTP entry settings preserve independent credentials and never echo passwords", async () => {
  const read = () => mockHandle<HTTPAuthSettings>("GET", "/settings/http-auth");
  const save = (value: object) => mockHandle<HTTPAuthSettings>("PUT", "/settings/http-auth", JSON.stringify(value));
  assert.deepEqual(await read(), { enabled: false, username: "entry", password_set: false });
  await assert.rejects(save({ enabled: true, username: "entry", password: "" }));
  for (const username of ["bad:name", "中".repeat(43)]) {
    await assert.rejects(save({ enabled: true, username, password: "test-password" }));
  }
  await assert.rejects(save({ enabled: true, username: "entry", password: "short" }));
  const enabled = await save({ enabled: true, username: "入口", password: "测试独立密码123" });
  assert.deepEqual(enabled, { enabled: true, username: "入口", password_set: true });
  assert.equal(JSON.stringify(await read()).includes("测试独立密码"), false);
  assert.deepEqual(await save({ enabled: false, username: "入口", password: "" }), { ...enabled, enabled: false });
  assert.deepEqual(await save({ enabled: true, username: "入口", password: "" }), enabled);
});
