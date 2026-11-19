import { reportPacketPreview, reproductionMarkdown } from "../finding-reproduction";
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
  assert.ok(md.includes("````http\n"));
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

test("packet previews retain headers, Unicode, and the original copy source", () => {
  for (const newline of ["\n", "\r\n"]) {
    const headers = `POST /api HTTP/1.1${newline}Host: example.test${newline}Cookie: ${"c".repeat(9000)}${newline}${newline}`;
    const packet = headers + "中🙂".repeat(4500);
    const preview = reportPacketPreview(packet);
    assert.equal(reportPacketPreview(preview), preview);
    assert.equal(reportPacketPreview(headers.trimEnd()), headers.trimEnd());
    assert.ok(preview.startsWith(headers + "中🙂".repeat(3000)));
    assert.ok(preview.endsWith("中🙂".repeat(1000)));
    assert.ok(preview.includes("[正文已截断，省略 1000 字符]"));
    const short = headers + "🙂".repeat(8000);
    assert.equal(reportPacketPreview(short), short);
    const step = { title: "请求", action: "发送请求", result: "完成", request: packet, binding_ids: ["42"] };
    const md = reproductionMarkdown({ steps: [step] });
    assert.ok(md.includes("需从已保存原文补齐后复现"));
    assert.ok(!md.includes("关联流量证据编号"));
    assert.equal(step.request, packet);
  }
});
