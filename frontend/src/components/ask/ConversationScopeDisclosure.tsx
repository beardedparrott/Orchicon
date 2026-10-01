import { useEffect, useRef, useState } from "react";
import { Cable } from "lucide-react";
import { cn } from "@/lib/utils";
import {
  useGetConversation,
  useSetConversationSkillFiles,
} from "@/api/askOrchicon";
import { useListProjects } from "@/api/projects";
import { MCPServersPanel } from "@/components/MCPServersPanel";
import { FileBrowser } from "@/components/FileBrowser";
import { popoverNudge } from "@/lib/ask-consent";

// ConversationScopeDisclosure — this conversation's OWN MCP servers + skill
// files, as a header DISCLOSURE beside the existing Grants disclosure.
//
// It is the conversation half of the platform model: a conversation owns its
// own MCP definitions (MCPServer.conversation_id) and its own skill_files path
// list (SetConversationSkillFiles), exactly as a project does. The panel it
// renders is the SAME MCPServersPanel used at the project and worker-version
// scopes — the GUI has one MCP surface, not a second picker.
//
// INHERITANCE IS VISIBLE: under pure union a conversation silently gains the
// project's servers and skill files, so the panel renders the project's MCP
// contributions READ-ONLY, labelled with the source, above the conversation's
// own additions. The skill files are similar: the FileBrowser browses the
// conversation's project tree, and the stored values are absolute paths.
//
// Modelled on SessionGrants (same trigger/panel/nudge/Escape-close/reset-on-
// conversation-change), because it sits in the same header cluster and must
// behave identically.
export interface ConversationScopeDisclosureProps {
  conversationId: string;
  className?: string;
}

export function ConversationScopeDisclosure({ conversationId, className }: ConversationScopeDisclosureProps) {
  const [open, setOpen] = useState(false);
  const [nudge, setNudge] = useState(0);
  const anchorRef = useRef<HTMLDivElement | null>(null);
  const panelRef = useRef<HTMLDivElement | null>(null);

  const { data: conv } = useGetConversation(conversationId);
  const { data: projects } = useListProjects();
  const setSkillFiles = useSetConversationSkillFiles(conversationId);

  const projectId = conv?.projectId ?? "";
  const project = projects?.find((p) => p.id === projectId);

  useEffect(() => {
    if (!open) return;
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, [open]);

  // Switching conversation closes it: the scope belongs to one conversation.
  useEffect(() => {
    setOpen(false);
  }, [conversationId]);

  useEffect(() => {
    if (!open) {
      setNudge(0);
      return;
    }
    const measure = () => {
      const anchor = anchorRef.current;
      const panel = panelRef.current;
      if (!anchor || !panel) return;
      setNudge(popoverNudge(anchor.getBoundingClientRect().right, panel.offsetWidth));
    };
    measure();
    window.addEventListener("resize", measure);
    return () => window.removeEventListener("resize", measure);
  }, [open, conversationId]);

  return (
    <div ref={anchorRef} className={cn("relative shrink-0", className)}>
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
        aria-label="Conversation MCP servers and skill files"
        data-testid="conversation-scope-trigger"
        className="flex h-11 items-center gap-1.5 rounded-xl border border-black/10 px-2.5 text-xs text-muted-foreground hover:text-foreground glass-panel dark:border-white/10"
      >
        <Cable aria-hidden="true" className="h-4 w-4" />
        Scope
      </button>

      {open && (
        <div
          ref={panelRef}
          data-testid="conversation-scope-panel"
          style={nudge ? { right: -nudge } : undefined}
          className="absolute right-0 top-full z-40 mt-2 w-[36rem] max-w-[calc(100vw-1rem)] max-h-[70vh] overflow-y-auto rounded-xl border border-border bg-popover p-3 text-sm shadow-lg"
        >
          <div className="mb-2 text-xs font-medium uppercase tracking-wide text-muted-foreground">
            MCP servers + skill files for this conversation
          </div>

          {conversationId === "" ? (
            <p className="text-xs text-muted-foreground">No conversation open.</p>
          ) : (
            <div className="space-y-4">
              <MCPServersPanel
                scope={{ kind: "conversation", conversationId }}
                inheritedFrom={
                  project ? { projectId: project.id, projectName: project.name } : undefined
                }
              />

              <div className="space-y-2">
                <h3 className="text-sm font-semibold">Skill files</h3>
                {projectId === "" ? (
                  <p className="text-xs text-muted-foreground">
                    Assign a project to this conversation to browse its tree for
                    skill files (SetConversationSkillFiles refuses a path outside
                    the conversation's project directory).
                  </p>
                ) : (
                  <FileBrowser
                    projectId={projectId}
                    projectDir={project?.projectDir || ""}
                    initialSelectedFiles={conv?.skillFiles ?? []}
                    onChange={(next) => setSkillFiles.mutate(next)}
                    title="Skill files"
                    description="Skill artifacts (files or directories) rendered into this conversation's prompt."
                  />
                )}
              </div>
            </div>
          )}
        </div>
      )}
    </div>
  );
}

export default ConversationScopeDisclosure;
