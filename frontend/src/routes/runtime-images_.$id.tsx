import { createRoute, Link, useNavigate } from "@tanstack/react-router";
import { useMemo, useState } from "react";
import { ArrowLeft, Hammer, Trash2, Save } from "lucide-react";

import {
  useGetRuntimeImage,
  useBuildRuntimeImage,
  useUpdateRuntimeImage,
  useDeleteRuntimeImage,
} from "@/api/runtimeImages";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { Route as rootRoute } from "@/routes/__root";
import type { RuntimeImage as RuntimeImageProto } from "@/api/gen/orchicon/api/v1/runtime_image_pb";

// Runtime image detail + edit + deploy page. Shows the spec, the live
// Dockerfile, the build log, and the Deploy button. Ready images revert
// to draft on edit so they must be rebuilt before reuse.
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/runtime-images/$id",
  component: RuntimeImageDetailPage,
});

function parseList(s: string): string[] {
  return s.split("\n").map((x) => x.trim()).filter(Boolean);
}

function parseEnv(s: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const line of parseList(s)) {
    const i = line.indexOf("=");
    if (i > 0) out[line.slice(0, i)] = line.slice(i + 1);
  }
  return out;
}

function RuntimeImageDetailPage() {
  const { id } = Route.useParams();
  const navigate = useNavigate();
  const { data: img, isLoading, error } = useGetRuntimeImage(id);
  const buildImage = useBuildRuntimeImage();
  const updateImage = useUpdateRuntimeImage();
  const deleteImage = useDeleteRuntimeImage();

  const [editMode, setEditMode] = useState(false);
  const [name, setName] = useState("");
  const [slug, setSlug] = useState("");
  const [description, setDescription] = useState("");
  const [apt, setApt] = useState("");
  const [toolchains, setToolchains] = useState("");
  const [env, setEnv] = useState("");
  const [tag, setTag] = useState("");
  const [advanced, setAdvanced] = useState(false);
  const [dockerfileOverride, setDockerfileOverride] = useState("");
  const [buildLog, setBuildLog] = useState("");
  const [buildError, setBuildError] = useState("");

  // Seed edit fields when the image loads (or when entering edit mode).
  const draft = useMemo(() => {
    if (!img) return null;
    const aptArr = JSON.parse(img.aptPackages || "[]") as string[];
    const toolArr = JSON.parse(img.toolchains || "[]") as string[];
    const envObj = JSON.parse(img.env || "{}") as Record<string, string>;
    return {
      name: img.name,
      slug: img.slug,
      description: img.description,
      apt: aptArr.join("\n"),
      toolchains: toolArr.join("\n"),
      env: Object.entries(envObj).map(([k, v]) => `${k}=${v}`).join("\n"),
      tag: img.tag,
      advanced: !!img.dockerfileOverride,
      dockerfileOverride: img.dockerfileOverride || "",
    };
  }, [img]);

  const preview = useMemo(() => {
    if (advanced) return dockerfileOverride;
    return generateDockerfile(img?.baseImageRef || "", apt, toolchains, env);
  }, [advanced, dockerfileOverride, img?.baseImageRef, apt, toolchains, env]);

  const startEdit = () => {
    if (!draft) return;
    setName(draft.name);
    setSlug(draft.slug);
    setDescription(draft.description);
    setApt(draft.apt);
    setToolchains(draft.toolchains);
    setEnv(draft.env);
    setTag(draft.tag);
    setAdvanced(draft.advanced);
    setDockerfileOverride(draft.dockerfileOverride);
    setEditMode(true);
  };

  const handleSave = async () => {
    if (!img) return;
    setBuildError("");
    try {
      await updateImage.mutateAsync({
        id: img.id,
        name,
        slug,
        description,
        aptPackages: JSON.stringify(parseList(apt)),
        toolchains: JSON.stringify(parseList(toolchains)),
        env: JSON.stringify(parseEnv(env)),
        dockerfileOverride: advanced ? dockerfileOverride : "",
        tag,
        version: img.version,
      });
      setEditMode(false);
    } catch (e) {
      setBuildError(String(e));
    }
  };

  const handleBuild = async () => {
    if (!img) return;
    setBuildLog("");
    setBuildError("");
    try {
      const res = await buildImage.mutateAsync({
        id: img.id,
        version: img.version,
        onLog: (chunk) => setBuildLog((prev) => prev + chunk),
      });
      if (res.skipped) {
        setBuildLog("Runtime image is already up to date — no rebuild needed.");
      }
    } catch (e) {
      setBuildError(String(e));
    }
  };

  const handleDelete = async () => {
    if (!img) return;
    if (!window.confirm("Delete this runtime image and its local Docker image?")) return;
    await deleteImage.mutateAsync(img.id);
    navigate({ to: "/runtime-images" });
  };

  if (isLoading) return <p className="text-sm text-muted-foreground">Loading…</p>;
  if (error) return <p className="text-sm text-destructive">Failed to load: {String(error)}</p>;
  if (!img) return <p className="text-sm text-muted-foreground">Not found.</p>;

  const d = draft!;
  const deploying = buildImage.isPending;

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div className="flex items-center gap-3">
          <Button variant="outline" size="sm" onClick={() => navigate({ to: "/runtime-images" })}>
            <ArrowLeft className="mr-1 h-4 w-4" />
            Back
          </Button>
          <div>
            <h1 className="text-2xl font-semibold tracking-tight">{img.name}</h1>
            <p className="text-sm text-muted-foreground">
              <code className="rounded bg-muted px-1">{img.tag || "no tag"}</code>{" "}
              · base <code className="rounded bg-muted px-1">{img.baseImageRef}</code>
            </p>
          </div>
        </div>
        <div className="flex items-center gap-2">
          {!editMode && (
            <>
              <Button variant="outline" onClick={startEdit}>
                Edit
              </Button>
              <Button onClick={handleBuild} disabled={deploying || img.status === 2}>
                <Hammer className="mr-1 h-4 w-4" />
                {deploying ? "Building…" : "Deploy"}
              </Button>
              <Button variant="destructive" onClick={handleDelete} disabled={deleteImage.isPending}>
                <Trash2 className="mr-1 h-4 w-4" />
                Delete
              </Button>
            </>
          )}
        </div>
      </div>

      {img.error && (
        <p className="text-sm text-destructive">Last build error: {img.error}</p>
      )}

      {!editMode && (
        <div className="grid gap-6 lg:grid-cols-2">
          <Card>
            <CardHeader>
              <CardTitle>Spec</CardTitle>
            </CardHeader>
            <CardContent className="space-y-2 text-sm">
              <p><span className="text-muted-foreground">Status:</span> {statusLabel(img.status)}</p>
              <p><span className="text-muted-foreground">Slug:</span> {d.slug}</p>
              <p><span className="text-muted-foreground">Description:</span> {d.description || "—"}</p>
              <p><span className="text-muted-foreground">apt packages:</span> {d.apt || "—"}</p>
              <p><span className="text-muted-foreground">Toolchains:</span> {d.toolchains || "—"}</p>
              <p><span className="text-muted-foreground">Env:</span> {d.env || "—"}</p>
            </CardContent>
          </Card>
          <Card>
            <CardHeader>
              <CardTitle>Dockerfile</CardTitle>
              <CardDescription>
                The exact build input (base FROM rewritten by the daemon on build).
              </CardDescription>
            </CardHeader>
            <CardContent>
              <pre className="max-h-96 overflow-auto rounded-md bg-muted p-3 text-xs">
                {d.dockerfileOverride || generateDockerfile(img.baseImageRef, d.apt, d.toolchains, d.env)}
              </pre>
            </CardContent>
          </Card>
        </div>
      )}

      {editMode && (
        <div className="grid gap-6 lg:grid-cols-2">
          <Card>
            <CardHeader>
              <CardTitle>Edit Spec</CardTitle>
              <CardDescription>
                Saving reverts a ready image to draft — rebuild to use it again.
              </CardDescription>
            </CardHeader>
            <CardContent className="space-y-4">
              <div className="grid grid-cols-2 gap-3">
                <div className="space-y-1">
                  <Label htmlFor="name">Name</Label>
                  <Input id="name" value={name} onChange={(e) => setName(e.target.value)} />
                </div>
                <div className="space-y-1">
                  <Label htmlFor="slug">Slug</Label>
                  <Input id="slug" value={slug} onChange={(e) => setSlug(e.target.value)} />
                </div>
              </div>
              <div className="space-y-1">
                <Label htmlFor="desc">Description</Label>
                <Textarea id="desc" value={description} onChange={(e) => setDescription(e.target.value)} rows={2} />
              </div>
              <div className="space-y-1">
                <Label htmlFor="tag">Tag</Label>
                <Input id="tag" value={tag} onChange={(e) => setTag(e.target.value)} />
              </div>
              <label className="flex items-center gap-2 text-sm font-medium">
                <input
                  type="checkbox"
                  checked={advanced}
                  onChange={(e) => setAdvanced(e.target.checked)}
                  className="h-4 w-4 rounded border-input"
                />
                Advanced: edit Dockerfile directly
              </label>
              {!advanced && (
                <>
                  <div className="space-y-1">
                    <Label htmlFor="apt">apt packages (one per line)</Label>
                    <Textarea id="apt" value={apt} onChange={(e) => setApt(e.target.value)} rows={3} />
                  </div>
                  <div className="space-y-1">
                    <Label htmlFor="tools">Toolchain lines</Label>
                    <Textarea id="tools" value={toolchains} onChange={(e) => setToolchains(e.target.value)} rows={3} />
                  </div>
                  <div className="space-y-1">
                    <Label htmlFor="env">Env (KEY=VALUE)</Label>
                    <Textarea id="env" value={env} onChange={(e) => setEnv(e.target.value)} rows={2} />
                  </div>
                </>
              )}
              {advanced && (
                <div className="space-y-1">
                  <Label htmlFor="df">Dockerfile</Label>
                  <Textarea
                    id="df"
                    value={dockerfileOverride}
                    onChange={(e) => setDockerfileOverride(e.target.value)}
                    rows={14}
                    className="font-mono text-xs"
                  />
                </div>
              )}
              <div className="flex gap-2">
                <Button onClick={handleSave} disabled={updateImage.isPending}>
                  <Save className="mr-1 h-4 w-4" />
                  Save
                </Button>
                <Button variant="outline" onClick={() => setEditMode(false)}>
                  Cancel
                </Button>
              </div>
            </CardContent>
          </Card>
          <Card>
            <CardHeader>
              <CardTitle>Dockerfile preview</CardTitle>
            </CardHeader>
            <CardContent>
              <pre className="max-h-96 overflow-auto rounded-md bg-muted p-3 text-xs">{preview || "# no content"}</pre>
            </CardContent>
          </Card>
        </div>
      )}

      {buildError && <p className="text-sm text-destructive">Error: {buildError}</p>}

      {buildLog && (
        <Card>
          <CardHeader>
            <CardTitle>Build Log</CardTitle>
          </CardHeader>
          <CardContent>
            <pre className="max-h-96 overflow-auto rounded-md bg-muted p-3 text-xs">{buildLog}</pre>
          </CardContent>
        </Card>
      )}

      <div className="flex items-center gap-4 text-sm text-muted-foreground">
        <Link to="/runtime-images" className="hover:underline">
          ← All runtime images
        </Link>
      </div>
    </div>
  );
}

function statusLabel(status: RuntimeImageProto["status"]): string {
  switch (status) {
    case 1: return "draft";
    case 2: return "building";
    case 3: return "ready";
    case 4: return "failed";
    default: return "unknown";
  }
}

function generateDockerfile(base: string, apt: string, toolchains: string, env: string): string {
  const aptList = parseList(apt);
  const toolList = parseList(toolchains);
  const envPairs = parseList(env);
  const lines: string[] = [`FROM ${base}`, "USER root"];
  if (aptList.length > 0) {
    lines.push("RUN apt-get update && apt-get install -y --no-install-recommends \\");
    aptList.forEach((p, i) => lines.push(`      ${p} ${i < aptList.length - 1 ? "\\" : ""}`));
    lines.push("      && rm -rf /var/lib/apt/lists/*");
  }
  for (const t of toolList) lines.push(`RUN ${t}`);
  for (const e of envPairs) {
    const i = e.indexOf("=");
    if (i > 0) lines.push(`ENV ${e.slice(0, i)}=${e.slice(i + 1)}`);
  }
  lines.push("RUN chown -R 1000:1000 /usr /opt /var 2>/dev/null || true");
  lines.push("USER orchicon");
  lines.push("WORKDIR /home/orchicon");
  return lines.join("\n");
}
