import assert from "node:assert/strict";
import test from "node:test";
import { entryAwareFetch } from "./http-auth";

test("入口认证 challenge 不被当成 JWT 过期，也不读取或修改浏览器凭据", async (t) => {
  let received: RequestInit | undefined;
  t.mock.method(globalThis, "fetch", async (_url: unknown, init?: RequestInit) => {
    received = init;
    return new Response("", { status: 401, headers: { "WWW-Authenticate": 'Basic realm="SERIES"' } });
  });
  await assert.rejects(entryAwareFetch("/api/test", { headers: { "X-Series-Token": "jwt" } }), /HTTP 入口认证失败/);
  assert.deepEqual(received?.headers, { "X-Series-Token": "jwt" });
});

test("JWT 401 和正常响应保留给现有调用逻辑", async (t) => {
  for (const status of [200, 401, 429, 503]) {
    t.mock.method(globalThis, "fetch", async () => new Response("", { status }));
    assert.equal((await entryAwareFetch("/api/test")).status, status);
    t.mock.restoreAll();
  }
});
