import type { FindingReproduction } from "./types";

const packetPreviewFooter =
  /^(?:…\[truncated\] @blob sha256:[0-9a-f]{64} \(len=[0-9]+\)|\.\.\. \[截断，共 [0-9]+ 字节；完整在流量文件树\] \.\.\.)$/;

// Preserve the complete header section and Unicode characters. Only presentation
// is shortened; callers keep the saved packet for copying and replay.
export function reportPacketPreview(packet: string): string {
  const separator = /\r?\n\r?\n/.exec(packet);
  const bodyStart = separator ? separator.index + separator[0].length : 0;
  // An incomplete/header-only HTTP packet has no body we can safely shorten.
  if (!separator && /^(?:HTTP\/\d|[A-Z]+ \S+ HTTP\/\d)/.test(packet)) return packet;
  const rawBody = packet.slice(bodyStart);
  const withoutMarker = rawBody.replace(/\n\[正文已截断，省略 \d+ 字符\]\n/, "");
  if (withoutMarker !== rawBody && Array.from(withoutMarker).length <= 8000) return packet;
  const body = Array.from(rawBody);
  if (body.length <= 8000) return packet;
  return `${packet.slice(0, bodyStart)}${body.slice(0, 6000).join("")}\n[正文已截断，省略 ${body.length - 8000} 字符]\n${body.slice(-2000).join("")}`;
}

export function reproductionPacketNotices(request?: string, response?: string): string[] {
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
      lines.push(
        `**${label}**`,
        "",
        `${fence}${label === "请求" || label === "响应" ? "http" : "text"}`,
        reportPacketPreview(text),
        fence,
        "",
      );
      if (label === "请求" && reportPacketPreview(text) !== text)
        lines.push("请求正文已截断，需从已保存原文补齐后复现。", "");
    }
    for (const notice of reproductionPacketNotices(step.request, step.response)) lines.push(notice, "");
    lines.push(step.result, "");
    if (step.proves) lines.push(step.proves, "");
  }
  return lines.join("\n");
}
