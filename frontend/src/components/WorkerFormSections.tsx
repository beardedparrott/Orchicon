import { useMemo } from "react";

import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

// --- Shared types ---

interface SectionProps {
  value: string;
  onChange: (value: string) => void;
}

// --- Permissions Section ---

const AVAILABLE_TOOLS = [
  { id: "file_edit", label: "File Edit", description: "Read and write files" },
  { id: "terminal", label: "Terminal", description: "Execute shell commands" },
  { id: "web_fetch", label: "Web Fetch", description: "Fetch URLs and web content" },
  { id: "git", label: "Git", description: "Git operations (commit, push, etc.)" },
  { id: "glob", label: "Glob", description: "Search files by pattern" },
  { id: "grep", label: "Grep", description: "Search file contents" },
  { id: "mcp", label: "MCP", description: "Model Context Protocol tools" },
];

interface PermissionsData {
  tools: string[];
  mcp_servers: string[];
  model_providers: string[];
  context: string[];
  network: string[];
  filesystem: string[];
}

function parsePermissions(raw: string): PermissionsData {
  try {
    const p = JSON.parse(raw);
    return {
      tools: Array.isArray(p.tools) ? p.tools : [],
      mcp_servers: Array.isArray(p.mcp_servers) ? p.mcp_servers : [],
      model_providers: Array.isArray(p.model_providers) ? p.model_providers : [],
      context: Array.isArray(p.context) ? p.context : [],
      network: Array.isArray(p.network) ? p.network : [],
      filesystem: Array.isArray(p.filesystem) ? p.filesystem : [],
    };
  } catch {
    return { tools: [], mcp_servers: [], model_providers: [], context: [], network: [], filesystem: [] };
  }
}

function serializePermissions(p: PermissionsData): string {
  return JSON.stringify(p, null, 2);
}

export function PermissionsSection({ value, onChange }: SectionProps) {
  const data = useMemo(() => parsePermissions(value), [value]);

  function update(fn: (d: PermissionsData) => PermissionsData) {
    onChange(serializePermissions(fn(structuredClone(data))));
  }

  function toggleTool(toolId: string) {
    update((d) => {
      const idx = d.tools.indexOf(toolId);
      if (idx >= 0) d.tools.splice(idx, 1);
      else d.tools.push(toolId);
      return d;
    });
  }

  return (
    <div className="space-y-3">
      <div>
        <span className="text-xs font-medium text-muted-foreground">Allowed tools</span>
        <p className="text-xs text-muted-foreground mb-2">
          Select which tools this worker may use.
        </p>
        <div className="grid grid-cols-2 gap-2">
          {AVAILABLE_TOOLS.map((tool) => (
            <label
              key={tool.id}
              className="flex items-start gap-2 rounded border p-2 cursor-pointer hover:bg-accent text-sm"
            >
              <input
                type="checkbox"
                className="mt-0.5"
                checked={data.tools.includes(tool.id)}
                onChange={() => toggleTool(tool.id)}
              />
              <div>
                <div className="font-medium text-xs">{tool.label}</div>
                <div className="text-xs text-muted-foreground">{tool.description}</div>
              </div>
            </label>
          ))}
        </div>
      </div>

      <div className="space-y-1">
        <Label className="text-xs">MCP servers</Label>
        <p className="text-xs text-muted-foreground">
          Comma-separated MCP server IDs the worker may use
        </p>
        <Input
          value={data.mcp_servers.join(", ")}
          onChange={(e) =>
            update((d) => {
              d.mcp_servers = e.target.value
                .split(",")
                .map((s) => s.trim())
                .filter(Boolean);
              return d;
            })
          }
          placeholder="filesystem, github, postgres"
          className="font-mono text-xs"
        />
      </div>

      <details className="text-xs">
        <summary className="cursor-pointer text-muted-foreground hover:text-foreground">
          Raw JSON
        </summary>
        <pre className="mt-1 rounded bg-muted p-2 text-xs font-mono overflow-x-auto">{value}</pre>
      </details>
    </div>
  );
}

// --- Gated Tools Section ---

const GATED_TOOL_OPTIONS = [
  { id: "terminal", label: "Terminal", description: "Require approval for shell commands" },
  { id: "web_fetch", label: "Web Fetch", description: "Require approval for URL fetches" },
  { id: "git", label: "Git", description: "Require approval for git operations" },
  { id: "file_edit", label: "File Edit", description: "Require approval for file changes" },
];

export function GatedToolsSection({ value, onChange }: SectionProps) {
  const tools = useMemo(() => {
    try {
      const parsed = JSON.parse(value);
      return Array.isArray(parsed) ? parsed : [];
    } catch {
      return [];
    }
  }, [value]);

  function toggle(id: string) {
    const updated = tools.includes(id) ? tools.filter((t: string) => t !== id) : [...tools, id];
    onChange(JSON.stringify(updated));
  }

  return (
    <div className="space-y-2">
      <p className="text-xs text-muted-foreground">
        Tools that require per-call (Tier 2) approval before execution.
      </p>
      <div className="grid grid-cols-2 gap-2">
        {GATED_TOOL_OPTIONS.map((tool) => (
          <label
            key={tool.id}
            className="flex items-start gap-2 rounded border p-2 cursor-pointer hover:bg-accent text-sm"
          >
            <input
              type="checkbox"
              className="mt-0.5"
              checked={tools.includes(tool.id)}
              onChange={() => toggle(tool.id)}
            />
            <div>
              <div className="font-medium text-xs">{tool.label}</div>
              <div className="text-xs text-muted-foreground">{tool.description}</div>
            </div>
          </label>
        ))}
      </div>
    </div>
  );
}

// --- Budget Overrides Section ---

interface BudgetData {
  tokens?: number;
  cost_usd?: number;
  wall_clock_seconds?: number;
  tool_call_count?: number;
}

function parseBudget(raw: string): BudgetData {
  try {
    const p = JSON.parse(raw);
    return {
      tokens: typeof p.tokens === "number" ? p.tokens : undefined,
      cost_usd: typeof p.cost_usd === "number" ? p.cost_usd : undefined,
      wall_clock_seconds: typeof p.wall_clock_seconds === "number" ? p.wall_clock_seconds : undefined,
      tool_call_count: typeof p.tool_call_count === "number" ? p.tool_call_count : undefined,
    };
  } catch {
    return {};
  }
}

function serializeBudget(b: BudgetData): string {
  const out: Record<string, number> = {};
  if (b.tokens !== undefined) out.tokens = b.tokens;
  if (b.cost_usd !== undefined) out.cost_usd = b.cost_usd;
  if (b.wall_clock_seconds !== undefined) out.wall_clock_seconds = b.wall_clock_seconds;
  if (b.tool_call_count !== undefined) out.tool_call_count = b.tool_call_count;
  return JSON.stringify(out, null, 2);
}

export function BudgetSection({ value, onChange }: SectionProps) {
  const data = useMemo(() => parseBudget(value), [value]);

  function update(fn: (d: BudgetData) => BudgetData) {
    onChange(serializeBudget(fn(structuredClone(data))));
  }

  return (
    <div className="space-y-3">
      <p className="text-xs text-muted-foreground">
        Per-execution budget limits. Leave blank for server defaults; enter 0 for unlimited.
      </p>
      <div className="grid grid-cols-2 gap-3">
        <div className="space-y-1">
          <Label className="text-xs">Max tokens</Label>
          <Input
            type="number"
            min={0}
            value={data.tokens ?? ""}
            onChange={(e) =>
              update((d) => {
                d.tokens = e.target.value !== "" ? Number(e.target.value) : undefined;
                return d;
              })
            }
            placeholder="1000000"
            className="font-mono text-xs"
            title="0 = unlimited (blank = server default)"
          />
        </div>
        <div className="space-y-1">
          <Label className="text-xs">Max cost (USD)</Label>
          <Input
            type="number"
            min={0}
            step={0.01}
            value={data.cost_usd ?? ""}
            onChange={(e) =>
              update((d) => {
                d.cost_usd = e.target.value !== "" ? Number(e.target.value) : undefined;
                return d;
              })
            }
            placeholder="10"
            className="font-mono text-xs"
            title="0 = unlimited (blank = server default)"
          />
        </div>
        <div className="space-y-1">
          <Label className="text-xs">Wall clock (seconds)</Label>
          <Input
            type="number"
            min={0}
            value={data.wall_clock_seconds ?? ""}
            onChange={(e) =>
              update((d) => {
                d.wall_clock_seconds = e.target.value !== "" ? Number(e.target.value) : undefined;
                return d;
              })
            }
            placeholder="3600"
            className="font-mono text-xs"
            title="0 = unlimited (blank = server default)"
          />
        </div>
        <div className="space-y-1">
          <Label className="text-xs">Max tool calls</Label>
          <Input
            type="number"
            min={0}
            value={data.tool_call_count ?? ""}
            onChange={(e) =>
              update((d) => {
                d.tool_call_count = e.target.value !== "" ? Number(e.target.value) : undefined;
                return d;
              })
            }
            placeholder="100"
            className="font-mono text-xs"
            title="0 = unlimited (blank = server default)"
          />
        </div>
      </div>

      <details className="text-xs">
        <summary className="cursor-pointer text-muted-foreground hover:text-foreground">
          Raw JSON
        </summary>
        <pre className="mt-1 rounded bg-muted p-2 text-xs font-mono overflow-x-auto">{value}</pre>
      </details>
    </div>
  );
}

// --- Context Sources Section ---

const CONTEXT_SOURCE_OPTIONS = [
  { id: "project_docs", label: "Project Docs", description: "Documentation files in the project" },
  { id: "file_tree", label: "File Tree", description: "Project file structure" },
  { id: "git_history", label: "Git History", description: "Recent git commits and diffs" },
  { id: "schema", label: "Schema", description: "Database and API schemas" },
  { id: "design_docs", label: "Design Docs", description: "Architecture and design documents" },
  { id: "error_logs", label: "Error Logs", description: "Recent error traces" },
  { id: "test_results", label: "Test Results", description: "Latest test output" },
];

export function ContextSourcesSection({ value, onChange }: SectionProps) {
  const sources = useMemo(() => {
    try {
      const parsed = JSON.parse(value);
      return Array.isArray(parsed) ? parsed : [];
    } catch {
      return [];
    }
  }, [value]);

  function toggle(id: string) {
    const updated = sources.includes(id) ? sources.filter((s: string) => s !== id) : [...sources, id];
    onChange(JSON.stringify(updated));
  }

  return (
    <div className="space-y-2">
      <p className="text-xs text-muted-foreground">
        Context sources the worker loads automatically on each execution.
      </p>
      <div className="grid grid-cols-2 gap-2">
        {CONTEXT_SOURCE_OPTIONS.map((src) => (
          <label
            key={src.id}
            className="flex items-start gap-2 rounded border p-2 cursor-pointer hover:bg-accent text-sm"
          >
            <input
              type="checkbox"
              className="mt-0.5"
              checked={sources.includes(src.id)}
              onChange={() => toggle(src.id)}
            />
            <div>
              <div className="font-medium text-xs">{src.label}</div>
              <div className="text-xs text-muted-foreground">{src.description}</div>
            </div>
          </label>
        ))}
      </div>
    </div>
  );
}
