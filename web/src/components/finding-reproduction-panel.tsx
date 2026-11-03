"use client";

import * as React from "react";

import { ChevronDownIcon } from "lucide-react";

import { CopyButton } from "@/components/copy-button";
import { TrafficEvidenceViewer } from "@/components/traffic-evidence-viewer";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
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
    <section className="flex min-w-0 flex-col gap-4" aria-label="分步复现">
      <h3 className="font-medium">分步复现</h3>
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
        <Card key={`${i}:${step.title}`} className="min-w-0">
          <CardHeader>
            <Badge variant="outline">步骤 {i + 1}</Badge>
            <CardTitle className="break-words">{step.title}</CardTitle>
            <CardDescription className="whitespace-pre-wrap break-words">{step.action}</CardDescription>
          </CardHeader>
          <CardContent className="flex min-w-0 flex-col gap-3">
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
            {step.request && !step.response && (
              <Alert>
                <AlertDescription>响应原文未记录；结论以已有证据为准。</AlertDescription>
              </Alert>
            )}
            <p className="whitespace-pre-wrap break-words">
              <strong>实际结果：</strong>
              {step.result}
            </p>
            {step.proves && (
              <p className="whitespace-pre-wrap break-words">
                <strong>证明范围：</strong>
                {step.proves}
              </p>
            )}
            {findingId && step.binding_ids?.length ? (
              <div className="flex flex-wrap gap-2">
                {step.binding_ids.map((id) => (
                  <Button key={id} variant="outline" size="sm" onClick={() => setBindingId(id)}>
                    查看流量证据 #{id}
                  </Button>
                ))}
              </div>
            ) : (
              <p className="text-muted-foreground text-sm">未关联流量证据；以上为现场记录。</p>
            )}
          </CardContent>
        </Card>
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
