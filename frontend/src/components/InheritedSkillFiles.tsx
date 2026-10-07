// InheritedSkillFiles — the PROJECT's contributed skill files, rendered READ-ONLY
// and labelled with their source, at a scope that INHERITS them.
//
// WHY IT EXISTS. Under pure union a conversation or a worker version silently
// gains the project's skill files (the server unions the conversation's list
// with its project's, and the worker composite prompt unions the version's with
// the project's), so without naming them an operator cannot answer the only
// question that matters when something unexpected lands in a prompt: "why is
// this file here?". Same rule as MCPServersPanel's inherited servers — the
// asymmetry is that MCP definitions live in the panel while skill files are
// plain paths on the project, so this is a list, not a picker.
//
// IT IS NOT A SECOND SELECTION CONTROL. There is nothing to toggle here: these
// paths belong to the project and are edited on the project page. At the
// project scope (the root) this component renders nothing, because the project
// shows only its own.
import { FolderTree } from "lucide-react";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";

export interface InheritedSkillFilesProps {
  /** The project whose skill files this scope inherits. */
  projectName: string;
  /** The project's skill_files paths (Project.skill_files). */
  files: string[];
}

export function InheritedSkillFiles({ projectName, files }: InheritedSkillFilesProps) {
  if (files.length === 0) return null;
  return (
    <Card>
      <CardHeader>
        <CardTitle>Inherited skill files from project {projectName}</CardTitle>
        <CardDescription>
          These skill files belong to the project and are rendered here
          automatically. They are read-only at this scope — edit them on the
          project page.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-1">
        {files.map((f) => (
          <div
            key={f}
            className="flex items-center gap-2 rounded-md border border-white/10 p-2 opacity-80"
          >
            <FolderTree aria-hidden="true" className="h-3 w-3 shrink-0 text-muted-foreground" />
            <span className="truncate font-mono text-xs">{f}</span>
            <span className="ml-auto shrink-0 text-xs text-muted-foreground">from project</span>
          </div>
        ))}
      </CardContent>
    </Card>
  );
}

export default InheritedSkillFiles;
