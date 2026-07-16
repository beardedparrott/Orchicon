import { useMemo, useState } from "react";
import type { StreamExecutionEventsResponse } from "@/api/gen/orchicon/api/v1/execution_pb";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { cn } from "@/lib/utils";

interface ParsedToolCall {
  id: string;
  toolName: string;
  input: string;
  output: string;
  occurredAt: Date;
}

interface ParsedText {
  id: string;
  text: string;
  occurredAt: Date;
}

interface RuntimeSessionPaneProps {
  events: StreamExecutionEventsResponse[];
}

export function RuntimeSessionPane({ events }: RuntimeSessionPaneProps) {
  const [activeTab, setActiveTab] = useState<string>("output");

  // Parse events into tool calls and text output
  const { toolCalls, textOutputs, resultText } = useMemo(() => {
    const toolCalls: ParsedToolCall[] = [];
    const textOutputs: ParsedText[] = [];
    let resultText = "";

    for (const resp of events) {
      const evt = resp.event;
      if (!evt) continue;
      const ts = evt.occurredAt
        ? new Date(Number(evt.occurredAt.seconds) * 1000)
        : new Date();
      const id = evt.eventId || `${resp.sequence}`;

      if (evt.eventType === 3 /* TOOL_CALL */ && evt.payload?.length) {
        try {
          const raw = new TextDecoder().decode(evt.payload);
          const data = JSON.parse(raw);
          const toolName = data.tool_name || data.tool || "unknown";
          const input = data.input || JSON.stringify(data.args || {}, null, 2);
          const output = data.output || data.result || "";
          toolCalls.push({ id, toolName, input, output, occurredAt: ts });
        } catch {
          // ignore parse errors
        }
      } else if (evt.eventType === 7 /* RESULT */ && evt.payload?.length) {
        try {
          const raw = new TextDecoder().decode(evt.payload);
          const data = JSON.parse(raw);
          if (data.text) resultText += data.text;
          if (data.status === "terminated" && data.aggregate_version) {
            // final result — could include accumulated output
          }
        } catch {
          // ignore
        }
      } else if (evt.eventType === 2 /* TELEMETRY */ && evt.payload?.length) {
        try {
          const raw = new TextDecoder().decode(evt.payload);
          const data = JSON.parse(raw);
          if (data.text) {
            textOutputs.push({ id, text: data.text, occurredAt: ts });
          }
        } catch {
          // ignore
        }
      }
    }

    return { toolCalls, textOutputs, resultText };
  }, [events]);

  const tabs = [
    { id: "output", label: "Output", count: textOutputs.length || (resultText ? 1 : 0) },
    { id: "tools", label: "Tool Calls", count: toolCalls.length },
  ];

  if (events.length === 0) return null;

  return (
    <Card>
      <CardHeader>
        <CardTitle>Runtime session</CardTitle>
      </CardHeader>
      <CardContent>
        {/* Tabs */}
        <div className="mb-3 flex gap-1 border-b">
          {tabs.map((tab) => (
            <button
              key={tab.id}
              type="button"
              onClick={() => setActiveTab(tab.id)}
              className={cn(
                "flex items-center gap-1.5 px-3 py-1.5 text-xs font-medium transition-colors",
                activeTab === tab.id
                  ? "border-b-2 border-primary text-primary"
                  : "text-muted-foreground hover:text-foreground",
              )}
            >
              {tab.label}
              {tab.count > 0 && (
                <span className="rounded-full bg-muted px-1.5 py-0.5 text-[10px]">
                  {tab.count}
                </span>
              )}
            </button>
          ))}
        </div>

        {/* Output tab */}
        {activeTab === "output" && (
          <div className="max-h-[400px] space-y-2 overflow-auto">
            {resultText ? (
              <pre className="whitespace-pre-wrap rounded bg-muted p-3 text-xs leading-relaxed">
                {resultText}
              </pre>
            ) : textOutputs.length > 0 ? (
              textOutputs.map((t) => (
                <div key={t.id} className="rounded bg-muted p-2 text-xs">
                  <span className="text-[10px] text-muted-foreground">
                    {t.occurredAt.toLocaleTimeString()}
                  </span>
                  <pre className="mt-1 whitespace-pre-wrap leading-relaxed">
                    {t.text}
                  </pre>
                </div>
              ))
            ) : (
              <p className="text-sm text-muted-foreground">
                Waiting for model output…
              </p>
            )}
          </div>
        )}

        {/* Tool calls tab */}
        {activeTab === "tools" && (
          <div className="max-h-[400px] space-y-2 overflow-auto">
            {toolCalls.length === 0 ? (
              <p className="text-sm text-muted-foreground">No tool calls yet.</p>
            ) : (
              toolCalls.map((tc) => (
                <div
                  key={tc.id}
                  className="rounded-md border p-3 text-sm"
                >
                  <div className="flex items-center justify-between">
                    <span className="font-medium text-amber-600 dark:text-amber-400">
                      {tc.toolName}
                    </span>
                    <span className="text-[10px] text-muted-foreground">
                      {tc.occurredAt.toLocaleTimeString()}
                    </span>
                  </div>
                  {tc.input && (
                    <div className="mt-1">
                      <span className="text-[10px] font-medium uppercase text-muted-foreground">
                        Input
                      </span>
                      <pre className="mt-0.5 max-h-24 overflow-auto rounded bg-muted p-2 text-xs">
                        {tc.input.length > 500
                          ? tc.input.slice(0, 500) + "…"
                          : tc.input}
                      </pre>
                    </div>
                  )}
                  {tc.output && (
                    <div className="mt-1">
                      <span className="text-[10px] font-medium uppercase text-muted-foreground">
                        Result
                      </span>
                      <pre className="mt-0.5 max-h-24 overflow-auto rounded bg-muted p-2 text-xs text-green-700 dark:text-green-300">
                        {tc.output.length > 500
                          ? tc.output.slice(0, 500) + "…"
                          : tc.output}
                      </pre>
                    </div>
                  )}
                </div>
              ))
            )}
          </div>
        )}
      </CardContent>
    </Card>
  );
}
