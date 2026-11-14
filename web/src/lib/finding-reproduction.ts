import type { FindingReproduction } from "./types";

const packetPreviewFooter =
  /^(?:…\[truncated\] @blob sha256:[0-9a-f]{64} \(len=[0-9]+\)|\.\.\. \[截断，共 [0-9]+ 字节；完整在流量文件树\] \.\.\.)$/;

export function reproductionPacketNotices(request?: string, response?: string): string[] {
  if (!request && !response) return [];
  const notices = [];
  for (const [label, raw] of [
    ["请求", request],
    ["响应", response],
  ] as const) {
    const packet = raw?.trim();
    if (!packet) notices.push(`${label}原文未记录。`);
    else if (packetPreviewFooter.test(packet.slice(packet.lastIndexOf("\n") + 1))) {
      notices.push(`${label}报文为截断预览，请核对已保存的流量证据。`);
    }
  }
  return notices;
}

// Deterministic mock-export fallback; the production server renders its own
// saved representation. This never infers steps from unstructured history.
export function reproductionMarkdown(reproduction?: FindingReproduction | null): string {
  if (!reproduction?.steps.length) return "";
  const lines = ["## 复现记录", ""];
  if (reproduction.entry_point) lines.push(`入口：${reproduction.entry_point}`, "");
  if (reproduction.prerequisites?.length)
    lines.push("前置条件：", ...reproduction.prerequisites.map((text) => `- ${text}`), "");
  for (const step of reproduction.steps) {
    lines.push(step.action, "");
    for (const [label, text] of [
      ["请求", step.request],
      ["响应", step.response],
      ["命令", step.command],
      ["命令输出", step.output],
    ]) {
      if (!text) continue;
      let fence = "```";
      while (text.includes(fence)) fence += "`";
      lines.push(`**${label}**`, "", fence, text, fence, "");
    }
    for (const notice of reproductionPacketNotices(step.request, step.response)) lines.push(notice, "");
    lines.push(step.result, "");
    if (step.proves) lines.push(step.proves, "");
    if (step.binding_ids?.length) lines.push(`关联流量证据编号：${step.binding_ids.join("、")}`, "");
  }
  return lines.join("\n");
}
