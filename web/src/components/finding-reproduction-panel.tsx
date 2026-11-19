"use client";

import * as React from "react";

import { CopyButton } from "@/components/copy-button";
import { reportPacketPreview, reproductionPacketNotices } from "@/lib/finding-reproduction";
import type { FindingReproduction } from "@/lib/types";

export function FindingReproductionPanel({ reproduction }: { reproduction: FindingReproduction }) {
  return (
    <section className="flex min-w-0 flex-col gap-3" aria-label="复现记录">
      {reproduction.entry_point && <p className="whitespace-pre-wrap break-words">入口：{reproduction.entry_point}</p>}
      {!!reproduction.prerequisites?.length && (
        <p className="whitespace-pre-wrap break-words">前置条件：{reproduction.prerequisites.join("；")}</p>
      )}
      {reproduction.steps.map((step, i) => (
        // biome-ignore lint/suspicious/noArrayIndexKey: Saved steps retain discovery order.
        <React.Fragment key={`${i}:${step.title}`}>
          <p className="whitespace-pre-wrap break-words">{step.action}</p>
          {(
            [
              ["请求", step.request],
              ["响应", step.response],
              ["命令", step.command],
              ["命令输出", step.output],
            ] as const
          ).map(([label, text]) =>
            text ? (
              <div key={label} className="min-w-0">
                <div className="mb-1 flex items-center justify-between gap-2">
                  <span className="text-muted-foreground text-sm">{label}</span>
                  <CopyButton text={text} label="复制原文" />
                </div>
                <pre className="max-w-full overflow-x-auto whitespace-pre rounded-md bg-muted p-3 font-mono text-xs">
                  {reportPacketPreview(text)}
                </pre>
                {label === "请求" && reportPacketPreview(text) !== text && (
                  <p className="text-muted-foreground text-sm">请求正文展示已截断，复制原文可获取已保存的完整内容。</p>
                )}
              </div>
            ) : null,
          )}
          {reproductionPacketNotices(step.request, step.response).map((notice) => (
            <p className="text-muted-foreground text-sm" key={notice}>
              {notice}
            </p>
          ))}
          <p className="whitespace-pre-wrap break-words">{step.result}</p>
          {step.proves && <p className="whitespace-pre-wrap break-words">{step.proves}</p>}
        </React.Fragment>
      ))}
    </section>
  );
}
