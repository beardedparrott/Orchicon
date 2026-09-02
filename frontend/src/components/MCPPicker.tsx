import { useState, useMemo, useRef, useEffect } from "react";

import { useMCPServerList } from "@/api/mcpServers";
import type { MCPServer } from "@/api/gen/orchicon/api/v1/mcp_server_pb";

// MCPConfig is the reference-shaped selection entry persisted into the
// worker permissions JSON (`permissions.mcp_servers`): bare server ids
// (references, never copies — the tenant's mcp_servers row is the source
// of truth). `command` is retained for display/back-compat with legacy
// {id, command} shapes; new selections carry id only.
export interface MCPConfig {
  id: string;
  command?: string;
}

interface MCPPickerProps {
  value: MCPConfig[];        // selected MCP server ids (references)
  onChange: (configs: MCPConfig[]) => void;
}

// Tenant-backed MCP server picker (ADR-0008): lists the tenant's
// configured MCP servers (Settings → Adapters → MCP) and persists
// reference ids. The opencode well-known list was the pre-storage
// stand-in; the real surface is the tenant registry, and selections are
// references so editing one server entry updates every consumer.
export function MCPPicker({ value, onChange }: MCPPickerProps) {
  const { data: servers, isLoading, error } = useMCPServerList();
  const [search, setSearch] = useState("");
  const [showDropdown, setShowDropdown] = useState(false);
  const [focusedIdx, setFocusedIdx] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);
  const dropdownRef = useRef<HTMLDivElement>(null);

  const selectedIds = useMemo(() => new Set(value.map((c) => c.id)), [value]);

  // Resolve the selected ids against the tenant list so chips show the
  // live server name/command (auto-refresh: any server edit re-renders).
  const selected = useMemo(() => {
    if (!servers) return value.map((c) => c.id);
    const byId = new Map(servers.map((s) => [s.id, s]));
    return value.map((c) => byId.get(c.id)?.id ?? c.id);
  }, [servers, value]);

  const filtered = useMemo(() => {
    if (!servers) return [] as MCPServer[];
    let result = servers;
    if (search) {
      const q = search.toLowerCase();
      result = result.filter(
        (s) =>
          s.name.toLowerCase().includes(q) ||
          s.command.toLowerCase().includes(q) ||
          s.url.toLowerCase().includes(q),
      );
    }
    return result;
  }, [servers, search]);

  useEffect(() => setFocusedIdx(0), [filtered.length]);

  useEffect(() => {
    function handleClick(e: MouseEvent) {
      if (
        dropdownRef.current &&
        !dropdownRef.current.contains(e.target as Node) &&
        inputRef.current &&
        !inputRef.current.contains(e.target as Node)
      ) {
        setShowDropdown(false);
      }
    }
    document.addEventListener("mousedown", handleClick);
    return () => document.removeEventListener("mousedown", handleClick);
  }, []);

  function toggleServer(srv: MCPServer) {
    const already = value.findIndex((c) => c.id === srv.id);
    const updated =
      already >= 0
        ? value.filter((_, i) => i !== already)
        : [...value, { id: srv.id, command: srv.command }];
    onChange(updated);
  }

  function removeServer(id: string) {
    onChange(value.filter((c) => c.id !== id));
  }

  function handleKeyDown(e: React.KeyboardEvent) {
    if (!showDropdown) {
      if (e.key === "ArrowDown" || e.key === "Enter") {
        setShowDropdown(true);
        e.preventDefault();
      }
      return;
    }
    switch (e.key) {
      case "ArrowDown":
        e.preventDefault();
        setFocusedIdx((i) => Math.min(i + 1, filtered.length - 1));
        break;
      case "ArrowUp":
        e.preventDefault();
        setFocusedIdx((i) => Math.max(i - 1, 0));
        break;
      case "Enter":
        e.preventDefault();
        if (filtered[focusedIdx]) toggleServer(filtered[focusedIdx]);
        break;
      case "Escape":
        setShowDropdown(false);
        break;
    }
  }

  return (
    <div className="relative space-y-2">
      {/* Selected MCP server chips */}
      {selected.length > 0 && (
        <div className="flex flex-wrap gap-1">
          {selected.map((id) => {
            const srv = servers?.find((s) => s.id === id);
            return (
              <span
                key={id}
                className="inline-flex items-center gap-1 rounded bg-primary/10 px-2 py-0.5 text-xs font-medium"
              >
                {srv?.name ?? id}
                <button
                  type="button"
                  className="hover:text-destructive"
                  onClick={() => removeServer(id)}
                >
                  &times;
                </button>
              </span>
            );
          })}
        </div>
      )}

      <input
        ref={inputRef}
        type="text"
        className="flex h-11 sm:h-9 min-h-[44px] w-full rounded-xl glass-input px-3 py-1 text-sm shadow-sm transition-colors placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
        placeholder={selected.length > 0 ? "Add more MCP servers..." : "Search MCP servers..."}
        value={search}
        onChange={(e) => {
          setSearch(e.target.value);
          setShowDropdown(true);
        }}
        onFocus={() => setShowDropdown(true)}
        onKeyDown={handleKeyDown}
      />

      {showDropdown && (
        <div
          ref={dropdownRef}
          className="absolute z-50 mt-1 w-full rounded-xl glass-menu shadow-xl"
          style={{ maxHeight: "300px", overflow: "hidden", display: "flex", flexDirection: "column" }}
        >
          <div className="overflow-y-auto" style={{ maxHeight: "300px" }}>
            {isLoading && (
              <p className="p-3 text-xs text-muted-foreground text-center">Loading MCP servers...</p>
            )}
            {error && (
              <p className="p-3 text-xs text-destructive text-center">
                Failed to load: {String(error)}
              </p>
            )}
            {!isLoading && !error && filtered.length === 0 && (
              <p className="p-3 text-xs text-muted-foreground text-center">No MCP servers match your search</p>
            )}
            {!isLoading &&
              filtered.map((srv, idx) => {
                const isSelected = selectedIds.has(srv.id);
                return (
                  <button
                    key={srv.id}
                    type="button"
                    className={`w-full px-3 py-2 text-left text-sm hover:bg-accent flex items-center justify-between gap-2 ${
                      idx === focusedIdx ? "bg-accent" : ""
                    } ${isSelected ? "bg-primary/10" : ""}`}
                    onMouseEnter={() => setFocusedIdx(idx)}
                    onClick={() => toggleServer(srv)}
                  >
                    <div className="min-w-0 flex-1">
                      <div className="font-medium truncate flex items-center gap-2">
                        {srv.name}
                        {isSelected && (
                          <span className="text-xs text-primary">Selected</span>
                        )}
                      </div>
                      <div className="text-xs text-muted-foreground truncate font-mono">
                        {srv.transport === 1 ? srv.command : srv.url}
                      </div>
                    </div>
                    <div className="shrink-0">
                      <span
                        className={`inline-block h-2 w-2 rounded-full ${
                          srv.enabled ? "bg-green-500" : "bg-gray-400"
                        }`}
                        title={srv.enabled ? "Enabled" : "Disabled"}
                      />
                    </div>
                  </button>
                );
              })}
          </div>
        </div>
      )}
    </div>
  );
}
