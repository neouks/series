import type { FindingReproduction } from "./types";

// Deterministic mock-export fallback; the production server renders its own
// saved representation. This never infers steps from unstructured history.
export function reproductionMarkdown(reproduction?: FindingReproduction | null): string {
  if (!reproduction?.steps.length) return "";
  const lines = ["## 分步复现", ""];
  if (reproduction.entry_point) lines.push(`入口：${reproduction.entry_point}`, "");
  if (reproduction.prerequisites?.length)
    lines.push("前置条件：", ...reproduction.prerequisites.map((text) => `- ${text}`), "");
  reproduction.steps.forEach((step, index) => {
    lines.push(`### 步骤 ${index + 1}：${step.title}`, "", step.action, "");
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
    if (step.request && !step.response) lines.push("响应原文未记录。", "");
    lines.push(`实际结果：${step.result}`, "");
    if (step.proves) lines.push(`证明范围：${step.proves}`, "");
    lines.push(
      step.binding_ids?.length
        ? `关联流量证据编号：${step.binding_ids.join("、")}`
        : "未关联流量证据；以上为现场记录。",
      "",
    );
  });
  return lines.join("\n");
}
