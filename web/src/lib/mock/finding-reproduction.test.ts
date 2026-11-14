import { reproductionMarkdown } from "../finding-reproduction";
import type { Finding, FindingsPage } from "../types";
import { mockHandle } from "./handler";
import assert from "node:assert/strict";
import test from "node:test";

test("reproduction is detail-only and remains available in task context", async () => {
  const listed = await mockHandle<Finding[]>("GET", "/exploration/findings");
  assert.ok(listed.every((f) => !f.reproduction));
  const detail = await mockHandle<Finding>("GET", "/exploration/findings/f-2");
  assert.equal(detail.reproduction?.steps.length, 2);
  const page = await mockHandle<FindingsPage>("GET", "/exploration/findings?context_task=t-acme-api&limit=200");
  assert.ok(page.items.every((f) => !f.reproduction));
  const inherited = await mockHandle<Finding>("GET", "/exploration/findings/f-2?context_task=t-acme-api");
  assert.deepEqual(inherited.reproduction, detail.reproduction);
  await assert.rejects(mockHandle("GET", "/exploration/findings/f-2?context_task=unrelated"));
});

test("fallback markdown preserves real packets, step order and code delimiters", () => {
  const raw = "HTTP/1.1 200 OK\r\n\r\n```\n中文🙂";
  const md = reproductionMarkdown({
    steps: [
      { title: "对照", action: "读取已有响应", result: "成功", response: raw },
      { title: "验证", action: "修改订单编号", result: "已记录", request: "GET / HTTP/1.1" },
    ],
  });
  assert.ok(md.includes(raw));
  assert.ok(md.includes("````\n"));
  assert.ok(md.includes("## 复现记录"));
  assert.ok(md.includes("读取已有响应") && md.includes("修改订单编号"));
  assert.ok(md.indexOf("读取已有响应") < md.indexOf("修改订单编号"));
  assert.ok(!md.includes("### 步骤"));
  assert.ok(md.includes("响应原文未记录"));
  assert.ok(md.includes("请求原文未记录"));
  assert.equal(reproductionMarkdown(null), "");
});

test("recorded packet previews are labeled without changing the original text", () => {
  const raw = "HTTP/1.1 200 OK\n\n预览\n... [截断，共 12000 字节；完整在流量文件树] ...";
  const md = reproductionMarkdown({
    steps: [{ title: "已有记录", action: "核对响应", result: "正文仅有预览", response: raw }],
  });
  assert.ok(md.includes(raw));
  assert.ok(md.includes("响应报文为截断预览"));
});
