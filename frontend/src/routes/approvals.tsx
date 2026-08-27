import { useMemo, useRef, useState } from "react";
import { Link, createRoute } from "@tanstack/react-router";
import { CheckCircle2, XCircle, Clock, ExternalLink, Paperclip, ImagePlus, X } from "lucide-react";

import { useApproveStep, useListPendingStepApprovals } from "@/api/approvals";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Route as rootRoute } from "@/routes/__root";
import { ApprovalAttachment } from "@/api/gen/orchicon/api/v1/approval_service_pb";

export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/approvals",
  component: ApprovalsPage,
});

const STATUS_OPTIONS = [
  { value: "pending", label: "Pending" },
  { value: "approved", label: "Approved" },
  { value: "rejected", label: "Rejected" },
  { value: "all", label: "All" },
];

const SORT_OPTIONS = [
  { value: "created_at", label: "Created" },
  { value: "project_name", label: "Project" },
  { value: "workflow_name", label: "Workflow" },
];

// AttachmentDraft is an attachment staged for an approval decision, with an
// optional object URL for image preview.
interface AttachmentDraft {
  att: ApprovalAttachment;
  url?: string;
}

// fileToAttachment converts a File (picked or pasted) into the proto
// ApprovalAttachment carrying its bytes.
function fileToAttachment(file: File): Promise<ApprovalAttachment> {
  return file.arrayBuffer().then(
    (buf) =>
      new ApprovalAttachment({
        filename: file.name || `screenshot-${Date.now()}.png`,
        contentType: file.type || "application/octet-stream",
        data: new Uint8Array(buf),
      }),
  );
}

function ApprovalsPage() {
  const [search, setSearch] = useState("");
  const [statusFilter, setStatusFilter] = useState("pending");
  const [sortBy, setSortBy] = useState("created_at");
  const [sortOrder, setSortOrder] = useState("desc");
  const [reasonText, setReasonText] = useState<Record<string, string>>({});
  const [attachmentsByRun, setAttachmentsByRun] = useState<Record<string, AttachmentDraft[]>>({});
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const fileInputs = useRef<Record<string, HTMLInputElement | null>>({});

  const statusParam = statusFilter === "all" ? undefined : statusFilter;
  const { data: items, isLoading, error } = useListPendingStepApprovals({
    search,
    status: statusParam,
    sortBy,
    sortOrder,
  });

  const approvals = useMemo(() => {
    if (!items) return undefined;
    if (statusFilter === "pending") {
      return items.filter((a) => a.status === "pending");
    }
    return items;
  }, [items, statusFilter]);

  const approveMutation = useApproveStep();

  const toggleSelect = (id: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const toggleSelectAll = () => {
    if (!approvals) return;
    if (selected.size === approvals.length) {
      setSelected(new Set());
    } else {
      setSelected(new Set(approvals.map((a) => a.stepRunId)));
    }
  };

  const handleBulkAction = (approved: boolean) => {
    if (selected.size === 0) return;
    const label = approved ? "approve" : "reject";
    if (!window.confirm(`${label.charAt(0).toUpperCase() + label.slice(1)} ${selected.size} selected approval${selected.size === 1 ? "" : "s"}?`)) return;
    Promise.allSettled(
      Array.from(selected).map((id) =>
        approveMutation.mutateAsync({ stepRunId: id, approved, reason: "", reviewedBy: "" }),
      ),
    ).then(() => setSelected(new Set()));
  };

  const handleApprove = (stepRunId: string, approved: boolean) => {
    const reason = reasonText[stepRunId] ?? "";
    const attachments = (attachmentsByRun[stepRunId] ?? []).map((d) => d.att);
    approveMutation.mutate(
      { stepRunId, approved, reason, reviewedBy: "", attachments },
      {
        onSuccess: () => {
          setReasonText((prev) => {
            const next = { ...prev };
            delete next[stepRunId];
            return next;
          });
          setAttachmentsByRun((prev) => {
            const next = { ...prev };
            for (const d of next[stepRunId] ?? []) if (d.url) URL.revokeObjectURL(d.url);
            delete next[stepRunId];
            return next;
          });
        },
      },
    );
  };

  const addFiles = (stepRunId: string, files: File[]) => {
    if (!files.length) return;
    Promise.all(files.map((f) => fileToAttachment(f))).then((converted) => {
      setAttachmentsByRun((prev) => {
        const current = prev[stepRunId] ?? [];
        const drafts: AttachmentDraft[] = converted.map((att, i) => ({
          att,
          url: files[i]?.type?.startsWith("image/")
            ? URL.createObjectURL(files[i])
            : undefined,
        }));
        return { ...prev, [stepRunId]: [...current, ...drafts].slice(0, 20) };
      });
    });
  };

  // onPaste on the reason box captures screenshots pasted straight from the
  // clipboard (e.g. cmd+shift+4 / snip), like in opencode.
  const handlePaste = (stepRunId: string, e: React.ClipboardEvent) => {
    const images = Array.from(e.clipboardData?.items ?? [])
      .filter((it) => it.kind === "file" && it.type.startsWith("image/"))
      .map((it) => it.getAsFile())
      .filter((f): f is File => !!f);
    if (images.length) {
      e.preventDefault();
      addFiles(stepRunId, images);
    }
  };

  const removeAttachment = (stepRunId: string, index: number) => {
    setAttachmentsByRun((prev) => {
      const current = prev[stepRunId] ?? [];
      if (current[index]?.url) URL.revokeObjectURL(current[index].url!);
      const next = current.filter((_, i) => i !== index);
      const out = { ...prev };
      if (next.length) out[stepRunId] = next;
      else delete out[stepRunId];
      return out;
    });
  };

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold tracking-tight flex items-center gap-2"><span className="inline-flex h-2 w-2 rounded-full bg-emerald-400 animate-pulse motion-reduce:animate-none" /> Approvals</h1>
        <p className="mt-1 text-sm text-muted-foreground">
          Human-in-the-loop approval gates for workflow steps.
        </p>
      </div>

      {/* Filter bar */}
      <div className="flex flex-wrap items-center gap-3 rounded-2xl glass-panel p-3 border border-white/10">
        <Input
          placeholder="Search projects, work items, workflows..."
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          className="max-w-xs"
        />
        <select
          className="h-9 rounded-xl glass-input px-3 text-sm"
          value={statusFilter}
          onChange={(e) => setStatusFilter(e.target.value)}
        >
          {STATUS_OPTIONS.map((o) => (
            <option key={o.value} value={o.value}>{o.label}</option>
          ))}
        </select>
        <select
          className="h-9 rounded-xl glass-input px-3 text-sm"
          value={sortBy}
          onChange={(e) => setSortBy(e.target.value)}
        >
          {SORT_OPTIONS.map((o) => (
            <option key={o.value} value={o.value}>{o.label}</option>
          ))}
        </select>
        <select
          className="h-9 rounded-xl glass-input px-3 text-sm"
          value={sortOrder}
          onChange={(e) => setSortOrder(e.target.value)}
        >
          <option value="desc">Newest</option>
          <option value="asc">Oldest</option>
        </select>

        {selected.size > 0 && (
          <>
            <Button
              size="sm"
              variant="default"
              onClick={() => handleBulkAction(true)}
              disabled={approveMutation.isPending}
            >
              <CheckCircle2 className="mr-1 h-3.5 w-3.5" />
              Approve {selected.size} selected
            </Button>
            <Button
              size="sm"
              variant="destructive"
              onClick={() => handleBulkAction(false)}
              disabled={approveMutation.isPending}
            >
              <XCircle className="mr-1 h-3.5 w-3.5" />
              Reject {selected.size} selected
            </Button>
          </>
        )}
      </div>

      {/* Error state */}
      {error && (
        <div className="rounded-md border border-red-300 bg-red-50 p-4 text-sm text-red-800 dark:border-red-800 dark:bg-red-950/40 dark:text-red-200">
          Failed to load approvals: {(error as Error).message}
        </div>
      )}

      {/* Loading state */}
      {isLoading && (
        <div className="text-sm text-muted-foreground">Loading approvals...</div>
      )}

      {/* Empty state */}
      {!isLoading && approvals?.length === 0 && (
        <div className="py-12 text-center">
          <p className="text-muted-foreground">No approvals found.</p>
        </div>
      )}

      {/* Select-all header */}
      {approvals && approvals.length > 0 && (
        <div className="flex items-center gap-2 px-1">
          <input
            type="checkbox"
            checked={approvals.length > 0 && selected.size === approvals.length}
            onChange={toggleSelectAll}
            className="h-4 w-4 rounded border-input"
          />
          <span className="text-xs text-muted-foreground">
            {selected.size > 0
              ? `${selected.size} of ${approvals.length} selected`
              : `${approvals.length} approval${approvals.length === 1 ? "" : "s"}`}
          </span>
        </div>
      )}

      {/* Approval cards */}
      <div className="space-y-4">
        {approvals?.map((item) => {
          const isPending = item.status === "pending";
          const isApproved = item.status === "approved";
          const isRejected = item.status === "rejected";

          let StatusIcon = Clock;
          let statusColor = "text-amber-600 bg-amber-50 dark:bg-amber-950/40";
          if (isApproved) {
            StatusIcon = CheckCircle2;
            statusColor = "text-emerald-600 bg-emerald-50 dark:bg-emerald-950/40";
          } else if (isRejected) {
            StatusIcon = XCircle;
            statusColor = "text-red-600 bg-red-50 dark:bg-red-950/40";
          }

          return (
            <Card key={item.stepRunId}>
              <CardHeader className="pb-3">
                <div className="flex items-start justify-between">
                  <div className="flex items-start gap-3">
                    <input
                      type="checkbox"
                      checked={selected.has(item.stepRunId)}
                      onChange={() => toggleSelect(item.stepRunId)}
                      className="mt-1 h-4 w-4 rounded border-input"
                    />
                    <div className="space-y-1">
                      <CardTitle className="text-base">
                        <Link
                          to="/workflows/$id/runs/$runId"
                          params={{ id: item.workflowId, runId: item.workflowRunId }}
                          className="hover:underline"
                        >
                          {item.projectName && `${item.projectName} — `}
                          {item.workItemName}
                        </Link>
                      </CardTitle>
                      <div className="flex flex-wrap gap-2 text-xs text-muted-foreground">
                        <span>Workflow: {item.workflowName}</span>
                        {item.upstreamWorker && (
                          <span>From: {item.upstreamWorker}</span>
                        )}
                      </div>
                    </div>
                  </div>
                  <div className="flex items-center gap-2">
                    <Badge variant="outline" className={statusColor}>
                      <StatusIcon className="mr-1 h-3 w-3" />
                      {isPending ? "Pending" : item.status === "approved" ? "Approved" : item.status === "rejected" ? "Rejected" : item.status}
                    </Badge>
                    <Link
                      to="/workflows/$id/runs/$runId"
                      params={{ id: item.workflowId, runId: item.workflowRunId }}
                    >
                      <Button variant="ghost" size="icon" className="h-8 w-8">
                        <ExternalLink className="h-4 w-4" />
                      </Button>
                    </Link>
                  </div>
                </div>
              </CardHeader>
              <CardContent className="space-y-3">
                {/* Upstream summary */}
                {item.upstreamSummary && (
                  <div className="rounded-2xl glass-panel p-3">
                    <p className="text-xs font-medium text-muted-foreground">Summary</p>
                    <p className="mt-1 text-sm whitespace-pre-wrap">{item.upstreamSummary}</p>
                  </div>
                )}

                {/* Touched files */}
                {item.touchedFiles.length > 0 && (
                  <div>
                    <p className="text-xs font-medium text-muted-foreground">Touched files</p>
                    <div className="mt-1 flex flex-wrap gap-1">
                      {item.touchedFiles.map((f) => (
                        <code key={f} className="rounded bg-muted px-1.5 py-0.5 text-xs">{f}</code>
                      ))}
                    </div>
                  </div>
                )}

                {/* Acceptance criteria */}
                {item.acceptanceCriteria && (
                  <div>
                    <p className="text-xs font-medium text-muted-foreground">Acceptance criteria</p>
                    <p className="mt-1 text-sm whitespace-pre-wrap">{item.acceptanceCriteria}</p>
                  </div>
                )}

                {/* Action buttons */}
                {isPending && (
                  <div className="space-y-2 border-t pt-3">
                    <textarea
                      placeholder="Reason / feedback (optional) — written to .orchicon/ for the downstream worker. Paste a screenshot (cmd+shift+4 / snip) into this box to attach it."
                      className="w-full rounded-xl glass-input px-3 py-2 text-sm"
                      rows={2}
                      value={reasonText[item.stepRunId] ?? ""}
                      onChange={(e) =>
                        setReasonText((prev) => ({ ...prev, [item.stepRunId]: e.target.value }))
                      }
                      onPaste={(e) => handlePaste(item.stepRunId, e)}
                    />

                    {/* Attachments */}
                    <div className="flex flex-wrap items-center gap-2 rounded-2xl glass-panel p-3 border border-white/10">
                      <Button
                        size="sm"
                        variant="outline"
                        type="button"
                        onClick={() => fileInputs.current[item.stepRunId]?.click()}
                      >
                        <Paperclip className="mr-1 h-3.5 w-3.5" />
                        Attach files
                      </Button>
                      <span className="text-xs text-muted-foreground">
                        or paste a screenshot into the feedback box
                      </span>
                      <input
                        ref={(el) => { fileInputs.current[item.stepRunId] = el; }}
                        type="file"
                        multiple
                        className="hidden"
                        onChange={(e) => {
                          if (e.target.files) addFiles(item.stepRunId, Array.from(e.target.files));
                          e.target.value = "";
                        }}
                      />
                    </div>
                    {(attachmentsByRun[item.stepRunId] ?? []).length > 0 && (
                      <div className="flex flex-wrap gap-2">
                        {(attachmentsByRun[item.stepRunId] ?? []).map((d, i) => (
                          <div
                            key={i}
                            className="group relative flex items-center gap-1.5 rounded-md border bg-muted/40 px-2 py-1 text-xs"
                          >
                            {d.url ? (
                              <img
                                src={d.url}
                                alt={d.att.filename}
                                className="h-10 w-16 rounded object-cover"
                              />
                            ) : (
                              <ImagePlus className="h-3.5 w-3.5 text-muted-foreground" />
                            )}
                            <span className="max-w-40 truncate">{d.att.filename}</span>
                            <button
                              type="button"
                              onClick={() => removeAttachment(item.stepRunId, i)}
                              className="rounded p-0.5 text-muted-foreground hover:bg-muted hover:text-foreground"
                              aria-label={`Remove ${d.att.filename}`}
                            >
                              <X className="h-3 w-3" />
                            </button>
                          </div>
                        ))}
                      </div>
                    )}
                    <div className="flex gap-2">
                      <Button
                        size="sm"
                        variant="default"
                        onClick={() => handleApprove(item.stepRunId, true)}
                        disabled={approveMutation.isPending}
                      >
                        <CheckCircle2 className="mr-1 h-4 w-4" />
                        Approve
                      </Button>
                      <Button
                        size="sm"
                        variant="destructive"
                        onClick={() => handleApprove(item.stepRunId, false)}
                        disabled={approveMutation.isPending}
                      >
                        <XCircle className="mr-1 h-4 w-4" />
                        Reject
                      </Button>
                    </div>
                  </div>
                )}

                {/* Resolved feedback + attachments */}
                {!isPending && item.reason && (
                  <div className="rounded-2xl glass-panel p-3">
                    <p className="text-xs font-medium text-muted-foreground">Review feedback</p>
                    <p className="mt-1 text-sm whitespace-pre-wrap">{item.reason}</p>
                  </div>
                )}
                {!isPending && item.attachmentNames.length > 0 && (
                  <div>
                    <p className="text-xs font-medium text-muted-foreground">
                      Attachments shared with the worker
                    </p>
                    <div className="mt-1 flex flex-wrap gap-1">
                      {item.attachmentNames.map((n) => (
                        <code key={n} className="rounded bg-muted px-1.5 py-0.5 text-xs">{n}</code>
                      ))}
                    </div>
                  </div>
                )}
              </CardContent>
            </Card>
          );
        })}
      </div>
    </div>
  );
}
