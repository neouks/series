"use client";

import * as React from "react";

import { ChevronDownIcon } from "lucide-react";

import { CopyButton } from "@/components/copy-button";
import { TrafficEvidenceViewer } from "@/components/traffic-evidence-viewer";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { reproductionPacketNotices } from "@/lib/finding-reproduction";
import type { FindingReproduction } from "@/lib/types";

export function FindingReproductionPanel({
  reproduction,
  findingId,
  contextTask,
}: {
  reproduction: FindingReproduction;
  findingId: string;
  contextTask?: string;
}) {
  const [bindingId, setBindingId] = React.useState<string | null>(null);
  return (
    <section className="flex min-w-0 flex-col gap-3" aria-label="复现记录">
      <h3 className="font-medium">复现记录</h3>
      {reproduction.entry_point && <p className="whitespace-pre-wrap break-words">入口：{reproduction.entry_point}</p>}
      {!!reproduction.prerequisites?.length && (
        <div className="flex flex-col gap-2">
          <h4 className="font-medium">前置条件</h4>
          <ul className="flex list-disc flex-col gap-1 pl-5">
            {reproduction.prerequisites.map((text, i) => (
              // biome-ignore lint/suspicious/noArrayIndexKey: Discovery order is immutable; repeated prerequisite text is allowed.
              <li className="whitespace-pre-wrap break-words" key={`${i}:${text}`}>
                {text}
              </li>
            ))}
          </ul>
        </div>
      )}
      {reproduction.steps.map((step, i) => (
        // biome-ignore lint/suspicious/noArrayIndexKey: Steps are immutable and their original order is their identity.
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
              <Collapsible key={label} className="min-w-0 rounded-lg border">
                <div className="flex items-center justify-between gap-2 p-2">
                  <CollapsibleTrigger asChild>
                    <Button variant="ghost" size="sm">
                      <ChevronDownIcon data-icon="inline-start" />
                      {label}
                    </Button>
                  </CollapsibleTrigger>
                  <CopyButton text={text} />
                </div>
                <CollapsibleContent>
                  <pre className="max-h-96 max-w-full overflow-auto whitespace-pre border-t bg-muted p-3 font-mono text-xs">
                    {text}
                  </pre>
                </CollapsibleContent>
              </Collapsible>
            ) : null,
          )}
          {reproductionPacketNotices(step.request, step.response).map((notice) => (
            <p className="text-muted-foreground text-sm" key={notice}>
              {notice}
            </p>
          ))}
          <p className="whitespace-pre-wrap break-words">{step.result}</p>
          {step.proves && <p className="whitespace-pre-wrap break-words">{step.proves}</p>}
          {findingId && step.binding_ids?.length ? (
            <div className="flex flex-wrap gap-2">
              {step.binding_ids.map((id) => (
                <Button key={id} variant="outline" size="sm" onClick={() => setBindingId(id)}>
                  查看流量证据 #{id}
                </Button>
              ))}
            </div>
          ) : null}
        </React.Fragment>
      ))}
      {bindingId && (
        <TrafficEvidenceViewer
          findingId={findingId}
          bindingId={bindingId}
          contextTask={contextTask}
          onClose={() => setBindingId(null)}
        />
      )}
    </section>
  );
}
