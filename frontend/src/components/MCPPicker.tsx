import { useState, useMemo, useRef, useEffect } from "react";

import { useListOpenCodeMCPs } from "@/api/aigateway";
import type { OpenCodeMCP } from "@/api/gen/orchicon/api/v1/ai_gateway_pb";

interface MCPPickerProps {
  value: string[];        // selected MCP server ids
  onChange: (ids: string[]) => void;
}

export function MCPPicker({ value, onChange }: MCPPickerProps) {
  const { data: servers, isLoading, error } = useListOpenCodeMCPs();
  const [search, setSearch] = useState("");
  const [showDropdown, setShowDropdown] = useState(false);
  const [focusedIdx, setFocusedIdx] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);
  const dropdownRef = useRef<HTMLDivElement>(null);

  const selected = useMemo(() => {
    if (!servers) return [] as OpenCodeMCP[];
    return servers.filter((s) => value.includes(s.id));
  }, [servers, value]);

  const filtered = useMemo(() => {
    if (!servers) return [] as OpenCodeMCP[];
    let result = servers;
    if (search) {
      const q = search.toLowerCase();
      result = result.filter(
        (s) =>
          s.id.toLowerCase().includes(q) ||
          s.command.toLowerCase().includes(q),
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

  function toggleServer(srv: OpenCodeMCP) {
    const updated = value.includes(srv.id)
      ? value.filter((id) => id !== srv.id)
      : [...value, srv.id];
    onChange(updated);
  }

  function removeServer(id: string) {
    onChange(value.filter((v) => v !== id));
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
          {selected.map((srv) => (
            <span
              key={srv.id}
              className="inline-flex items-center gap-1 rounded bg-primary/10 px-2 py-0.5 text-xs font-medium"
            >
              {srv.id}
              <button
                type="button"
                className="hover:text-destructive"
                onClick={() => removeServer(srv.id)}
              >
                &times;
              </button>
            </span>
          ))}
        </div>
      )}

      <input
        ref={inputRef}
        type="text"
        className="flex h-9 w-full rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-sm transition-colors placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
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
          className="absolute z-50 mt-1 w-full rounded-md border bg-popover shadow-md"
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
                const isSelected = value.includes(srv.id);
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
                        {srv.id}
                        {isSelected && (
                          <span className="text-xs text-primary">Selected</span>
                        )}
                      </div>
                      <div className="text-xs text-muted-foreground truncate font-mono">
                        {srv.command}
                      </div>
                    </div>
                    <div className="shrink-0">
                      <span
                        className={`inline-block h-2 w-2 rounded-full ${
                          srv.status === "connected" ? "bg-green-500" : "bg-yellow-500"
                        }`}
                        title={srv.status}
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
