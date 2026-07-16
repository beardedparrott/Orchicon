import { useState, useMemo, useRef, useEffect } from "react";

import { useListOpenCodeModels } from "@/api/aigateway";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import type { OpenCodeModel } from "@/api/gen/orchicon/api/v1/ai_gateway_pb";

interface ModelPickerProps {
  value: string;
  onChange: (value: string) => void;
}

export function ModelPicker({ value, onChange }: ModelPickerProps) {
  const { data: models, isLoading, error } = useListOpenCodeModels();
  const [search, setSearch] = useState("");
  const [providerFilter, setProviderFilter] = useState<string>("");
  const [focusedIdx, setFocusedIdx] = useState(0);
  const [showDropdown, setShowDropdown] = useState(false);
  const [infoModel, setInfoModel] = useState<OpenCodeModel | null>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const dropdownRef = useRef<HTMLDivElement>(null);

  const selectedModel = useMemo(() => {
    if (!models || !value) return null;
    return models.find((m) => m.modelRef === value) ?? null;
  }, [models, value]);

  const providers = useMemo(() => {
    if (!models) return [] as string[];
    const set = new Set(models.map((m) => m.providerId));
    return Array.from(set).sort();
  }, [models]);

  const filtered = useMemo(() => {
    if (!models) return [] as OpenCodeModel[];
    let result = models;
    if (search) {
      const q = search.toLowerCase();
      result = result.filter(
        (m) =>
          m.id.toLowerCase().includes(q) ||
          m.name.toLowerCase().includes(q) ||
          m.providerId.toLowerCase().includes(q) ||
          m.modelRef.toLowerCase().includes(q) ||
          m.family.toLowerCase().includes(q),
      );
    }
    if (providerFilter) {
      result = result.filter((m) => m.providerId === providerFilter);
    }
    return result.sort((a, b) => {
      if (a.providerId !== b.providerId) return a.providerId.localeCompare(b.providerId);
      return (a.cost?.input ?? 0) - (b.cost?.input ?? 0);
    });
  }, [models, search, providerFilter]);

  // Reset focused index when filtered list changes
  useEffect(() => setFocusedIdx(0), [filtered.length]);

  // Close dropdown on outside click
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

  function selectModel(model: OpenCodeModel) {
    onChange(model.modelRef);
    setShowDropdown(false);
    setSearch("");
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
        if (filtered[focusedIdx]) selectModel(filtered[focusedIdx]);
        break;
      case "Escape":
        setShowDropdown(false);
        break;
    }
  }

  // Format cost display
  function formatCost(cost?: { input: number; output: number }) {
    if (!cost) return "";
    if (cost.input === 0 && cost.output === 0) return "Free";
    return `$${cost.input}/${cost.output} per 1M tokens`;
  }

  // Format limit display
  function formatLimit(val?: bigint | number | string) {
    if (!val) return "";
    const n = Number(val);
    if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
    if (n >= 1_000) return `${(n / 1_000).toFixed(0)}K`;
    return String(n);
  }

  if (infoModel) {
    return (
      <div className="space-y-2">
        <div className="flex items-center gap-2">
          <span className="text-sm font-medium">Selected model:</span>
          <span className="text-sm text-muted-foreground">{value}</span>
          <Button variant="outline" size="sm" onClick={() => setInfoModel(null)}>
            Change
          </Button>
        </div>
        <ModelInfoCard model={infoModel} onClose={() => setInfoModel(null)} />
      </div>
    );
  }

  return (
    <div className="relative space-y-2">
      {selectedModel ? (
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-sm font-medium">Selected model:</span>
          <span className="min-w-0 flex-1 truncate text-sm font-mono text-muted-foreground">
            {selectedModel.modelRef}
          </span>
          <Button
            variant="ghost"
            size="sm"
            className="text-xs"
            onClick={() => setInfoModel(selectedModel)}
          >
            Info
          </Button>
          <Button
            variant="outline"
            size="sm"
            onClick={() => {
              onChange("");
              setProviderFilter("");
              setSearch("");
            }}
          >
            Change
          </Button>
        </div>
      ) : (
        <>
          <Input
            ref={inputRef}
            placeholder="Search models (type to filter)..."
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
              style={{ maxHeight: "400px", overflow: "hidden", display: "flex", flexDirection: "column" }}
            >
              {/* Provider filter bar */}
              <div className="flex gap-1 border-b p-2 overflow-x-auto shrink-0">
                <button
                  type="button"
                  className={`rounded px-2 py-0.5 text-xs whitespace-nowrap ${
                    !providerFilter ? "bg-primary text-primary-foreground" : "bg-muted hover:bg-muted/80"
                  }`}
                  onClick={() => setProviderFilter("")}
                >
                  All
                </button>
                {providers.map((p) => (
                  <button
                    key={p}
                    type="button"
                    className={`rounded px-2 py-0.5 text-xs whitespace-nowrap ${
                      providerFilter === p ? "bg-primary text-primary-foreground" : "bg-muted hover:bg-muted/80"
                    }`}
                    onClick={() => setProviderFilter(p)}
                  >
                    {p}
                  </button>
                ))}
              </div>

              {/* Model list */}
              <div className="overflow-y-auto" style={{ maxHeight: "320px" }}>
                {isLoading && (
                  <p className="p-4 text-xs text-muted-foreground text-center">Loading models...</p>
                )}
                {error && (
                  <p className="p-4 text-xs text-destructive text-center">
                    Failed to load models: {String(error)}
                  </p>
                )}
                {!isLoading && !error && filtered.length === 0 && (
                  <p className="p-4 text-xs text-muted-foreground text-center">No models match your search</p>
                )}
                {!isLoading &&
                  filtered.map((model, idx) => (
                    <button
                      key={model.modelRef}
                      type="button"
                      className={`w-full px-3 py-2 text-left text-sm hover:bg-accent flex items-center justify-between gap-2 ${
                        idx === focusedIdx ? "bg-accent" : ""
                      } ${model.modelRef === value ? "bg-primary/10" : ""}`}
                      onMouseEnter={() => setFocusedIdx(idx)}
                      onClick={() => selectModel(model)}
                      onDoubleClick={() => {
                        selectModel(model);
                        setInfoModel(model);
                      }}
                    >
                      <div className="min-w-0 flex-1">
                        <div className="font-medium truncate">{model.name}</div>
                        <div className="text-xs text-muted-foreground truncate">
                          <span className="font-mono">{model.providerId}</span>
                          {" / "}
                          <span className="font-mono">{model.id}</span>
                        </div>
                      </div>
                      <div className="text-right shrink-0">
                        <div className="text-xs font-mono">{formatCost(model.cost)}</div>
                        <div className="text-xs text-muted-foreground">
                          {model.limits ? `${formatLimit(model.limits.context)} ctx` : ""}
                        </div>
                      </div>
                    </button>
                  ))}
              </div>
            </div>
          )}
        </>
      )}
    </div>
  );
}

function ModelInfoCard({ model, onClose }: { model: OpenCodeModel; onClose: () => void }) {
  return (
    <Card className="border-primary/20">
      <CardHeader className="pb-2 flex flex-row items-center justify-between">
        <div>
          <CardTitle className="text-base">{model.name}</CardTitle>
          <p className="text-xs text-muted-foreground font-mono">{model.modelRef}</p>
        </div>
        <Button variant="ghost" size="sm" onClick={onClose}>
          Close
        </Button>
      </CardHeader>
      <CardContent className="space-y-3 text-sm">
        <div className="grid grid-cols-2 gap-3">
          {/* Cost */}
          <div className="space-y-1">
            <span className="text-xs font-medium text-muted-foreground">Cost per 1M tokens</span>
            {model.cost ? (
              <div className="text-xs space-y-0.5">
                <div className="flex justify-between">
                  <span>Input</span>
                  <span className="font-mono">${model.cost.input}</span>
                </div>
                <div className="flex justify-between">
                  <span>Output</span>
                  <span className="font-mono">${model.cost.output}</span>
                </div>
                {(model.cost.cacheRead > 0 || model.cost.cacheWrite > 0) && (
                  <>
                    <div className="flex justify-between text-muted-foreground">
                      <span>Cache read</span>
                      <span className="font-mono">${model.cost.cacheRead}</span>
                    </div>
                    <div className="flex justify-between text-muted-foreground">
                      <span>Cache write</span>
                      <span className="font-mono">${model.cost.cacheWrite}</span>
                    </div>
                  </>
                )}
              </div>
            ) : (
              <span className="text-xs text-muted-foreground">N/A</span>
            )}
          </div>

          {/* Limits */}
          <div className="space-y-1">
            <span className="text-xs font-medium text-muted-foreground">Token limits</span>
            {model.limits ? (
              <div className="text-xs space-y-0.5">
                <div className="flex justify-between">
                  <span>Context</span>
                  <span className="font-mono">{Number(model.limits.context).toLocaleString()}</span>
                </div>
                <div className="flex justify-between">
                  <span>Max input</span>
                  <span className="font-mono">{Number(model.limits.input || 0).toLocaleString() || "N/A"}</span>
                </div>
                <div className="flex justify-between">
                  <span>Max output</span>
                  <span className="font-mono">{Number(model.limits.output).toLocaleString()}</span>
                </div>
              </div>
            ) : (
              <span className="text-xs text-muted-foreground">N/A</span>
            )}
          </div>
        </div>

        {/* Capabilities */}
        {model.capabilities && (
          <div>
            <span className="text-xs font-medium text-muted-foreground">Capabilities</span>
            <div className="flex flex-wrap gap-1 mt-1">
              {model.capabilities.reasoning && <CapBadge label="Reasoning" />}
              {model.capabilities.temperature && <CapBadge label="Temperature" />}
              {model.capabilities.toolcall && <CapBadge label="Tool calls" />}
              {model.capabilities.attachment && <CapBadge label="Attachments" />}
              {model.capabilities.inputImage && <CapBadge label="Image input" />}
              {model.capabilities.inputPdf && <CapBadge label="PDF input" />}
              {model.capabilities.inputAudio && <CapBadge label="Audio input" />}
              {model.capabilities.interleaved && <CapBadge label="Stream reasoning" />}
            </div>
          </div>
        )}

        {/* Variants (reasoning effort) */}
        {model.variants.length > 0 && (
          <div>
            <span className="text-xs font-medium text-muted-foreground">Reasoning effort variants</span>
            <div className="flex flex-wrap gap-1 mt-1">
              {model.variants.map((v) => (
                <span
                  key={v}
                  className="inline-block rounded bg-muted px-1.5 py-0.5 text-xs font-mono"
                >
                  {v}
                </span>
              ))}
            </div>
          </div>
        )}

        {model.releaseDate && (
          <p className="text-xs text-muted-foreground">Released: {model.releaseDate}</p>
        )}
      </CardContent>
    </Card>
  );
}

function CapBadge({ label }: { label: string }) {
  return (
    <span className="inline-block rounded bg-primary/10 px-1.5 py-0.5 text-xs text-primary">
      {label}
    </span>
  );
}
