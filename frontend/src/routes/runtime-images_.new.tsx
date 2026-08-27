import { createRoute, useNavigate } from "@tanstack/react-router";
import { useMemo, useState } from "react";
import { ArrowLeft, Hammer } from "lucide-react";

import { useAvailableRuntimeImages, useCreateRuntimeImage, useBuildRuntimeImage } from "@/api/runtimeImages";
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
import { cn } from "@/lib/utils";
import { Route as rootRoute } from "@/routes/__root";

// New runtime image form (docs/10 §5).
//
// Two modes, always showing the resulting Dockerfile:
//   - Easy mode: apt packages + toolchain install lines + env vars. The
//     Dockerfile preview is generated live and is ALWAYS visible.
//   - Advanced mode: edit the Dockerfile text directly. The base FROM
//     line and the runtime-base label are injected by the daemon on
//     build, so the base is always part of the image.
// The Deploy button builds the image on the host runtime daemon and
// streams the build log.
export const Route = createRoute({
  getParentRoute: () => rootRoute,
  path: "/runtime-images/new",
  component: NewRuntimeImagePage,
});

const slugRegex = /^[a-z0-9]+(?:-[a-z0-9]+)*$/;

// generateDockerfile mirrors internal/runtimeimage/generatedDockerfile so
// the preview matches exactly what the daemon receives (minus the
// daemon-rewritten FROM/label lines).
function generateDockerfile(base: string, apt: string, toolchains: string, env: string): string {
  const aptList = (apt || "")
    .split("\n")
    .map((s) => s.trim())
    .filter(Boolean);
  const toolList = (toolchains || "")
    .split("\n")
    .map((s) => s.trim())
    .filter(Boolean);
  const envPairs = (env || "")
    .split("\n")
    .map((s) => s.trim())
    .filter(Boolean);

  const lines: string[] = [`FROM ${base}`, "USER root"];
  if (aptList.length > 0) {
    lines.push("RUN apt-get update && apt-get install -y --no-install-recommends \\");
    aptList.forEach((p, i) => {
      lines.push(`      ${p} ${i < aptList.length - 1 ? "\\" : ""}`);
    });
    lines.push("      && rm -rf /var/lib/apt/lists/*");
  }
  for (const t of toolList) lines.push(`RUN ${t}`);
  for (const e of envPairs) {
    const idx = e.indexOf("=");
    if (idx > 0) lines.push(`ENV ${e.slice(0, idx)}=${e.slice(idx + 1)}`);
  }
  lines.push("RUN chown -R 1000:1000 /usr /opt /var 2>/dev/null || true");
  lines.push("USER orchicon");
  lines.push("WORKDIR /home/orchicon");
  return lines.join("\n");
}

function NewRuntimeImagePage() {
  const navigate = useNavigate();
  const { data: avail } = useAvailableRuntimeImages();
  const base = avail?.defaultImage || "orchicon-runtime:latest";
  const createImage = useCreateRuntimeImage();
  const buildImage = useBuildRuntimeImage();

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
  const [error, setError] = useState("");
  const [deployedId, setDeployedId] = useState<string | null>(null);

  // Live preview — always shown (easy mode generated; advanced mode raw).
  const preview = useMemo(() => {
    if (advanced) return dockerfileOverride;
    return generateDockerfile(base, apt, toolchains, env);
  }, [advanced, dockerfileOverride, base, apt, toolchains, env]);

  const slugError = slug && !slugRegex.test(slug) ? "Slug must be lowercase words separated by hyphens." : "";

  const handleSave = async () => {
    setError("");
    setBuildLog("");
    const finalSlug = slug || name.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/(^-|-$)/g, "");
    if (!finalSlug || !slugRegex.test(finalSlug)) {
      setError("A valid slug is required.");
      return;
    }
    const finalTag = tag || `${finalSlug}:latest`;
    try {
      const created = await createImage.mutateAsync({
        name: name || finalSlug,
        slug: finalSlug,
        description,
        aptPackages: JSON.stringify(apt.split("\n").map((s) => s.trim()).filter(Boolean)),
        toolchains: JSON.stringify(toolchains.split("\n").map((s) => s.trim()).filter(Boolean)),
        env: JSON.stringify(Object.fromEntries(env.split("\n").map((s) => s.trim()).filter(Boolean).map((e) => {
          const i = e.indexOf("=");
          return i > 0 ? [e.slice(0, i), e.slice(i + 1)] : [e, ""];
        }))),
        dockerfileOverride: advanced ? dockerfileOverride : "",
        tag: finalTag,
      });
      setDeployedId(created.id);
      // Immediately offer the build.
      await handleBuild(created.id);
    } catch (e) {
      setError(String(e));
    }
  };

  const handleBuild = async (id: string) => {
    setError("");
    setBuildLog("");
    try {
      await buildImage.mutateAsync({
        id,
        version: 1,
        onLog: (chunk) => setBuildLog((prev) => prev + chunk),
      });
    } catch (e) {
      setError(String(e));
    }
  };

  const deploying = buildImage.isPending;

  return (
    <div className="space-y-6">
      <div className="flex items-center gap-3">
        <Button variant="outline" size="sm" onClick={() => navigate({ to: "/runtime-images" })}>
          <ArrowLeft aria-hidden="true" className="mr-1 h-4 w-4" />
          Back
        </Button>
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">New Runtime Image</h1>
          <p className="text-sm text-muted-foreground">
            Define a container image for workers. Base image: <code className="rounded bg-muted px-1">{base}</code>
          </p>
        </div>
      </div>

      <div className="grid gap-6 lg:grid-cols-2">
        <div className="space-y-4">
          <Card>
            <CardHeader>
              <CardTitle>Image</CardTitle>
              <CardDescription>
                The spec. Edit the Dockerfile live, then deploy to build it
                on the host runtime daemon.
              </CardDescription>
            </CardHeader>
            <CardContent className="space-y-4">
              <div className="grid grid-cols-2 gap-3">
                <div className="space-y-1">
                  <Label htmlFor="name">Name</Label>
                  <Input id="name" value={name} onChange={(e) => setName(e.target.value)} placeholder="Headless GUI toolkit" />
                </div>
                <div className="space-y-1">
                  <Label htmlFor="slug">Slug (image name)</Label>
                  <Input id="slug" value={slug} onChange={(e) => setSlug(e.target.value)} placeholder="headless-gui" />
                  {slugError && <p className="text-xs text-destructive">{slugError}</p>}
                </div>
              </div>
              <div className="space-y-1">
                <Label htmlFor="desc">Description</Label>
                <Textarea id="desc" value={description} onChange={(e) => setDescription(e.target.value)} rows={2} />
              </div>
              <div className="space-y-1">
                <Label htmlFor="tag">Tag</Label>
                <Input id="tag" value={tag} onChange={(e) => setTag(e.target.value)} placeholder="defaults to <slug>:latest" />
              </div>

              <div className="flex items-center gap-2 border-t pt-3">
                <label className="flex items-center gap-2 text-sm font-medium">
                  <input
                    type="checkbox"
                    checked={advanced}
                    onChange={(e) => setAdvanced(e.target.checked)}
                    className="h-4 w-4 rounded border-input"
                  />
                  Advanced: edit Dockerfile directly
                </label>
              </div>

              {!advanced && (
                <>
                  <div className="space-y-1">
                    <Label htmlFor="apt">apt packages (one per line)</Label>
                    <Textarea
                      id="apt"
                      value={apt}
                      onChange={(e) => setApt(e.target.value)}
                      rows={3}
                      placeholder={"libgl1\nlibegl1\nlibfontconfig1\nlibxkbcommon0\nxvfb\ntk"}
                    />
                  </div>
                  <div className="space-y-1">
                    <Label htmlFor="tools">Toolchain install lines (pip/npm/mise/curl)</Label>
                    <Textarea
                      id="tools"
                      value={toolchains}
                      onChange={(e) => setToolchains(e.target.value)}
                      rows={3}
                      placeholder={"pip install --break-system-packages pyside6\nnpm install -g playwright"}
                    />
                  </div>
                  <div className="space-y-1">
                    <Label htmlFor="env">Environment variables (KEY=VALUE per line)</Label>
                    <Textarea
                      id="env"
                      value={env}
                      onChange={(e) => setEnv(e.target.value)}
                      rows={2}
                      placeholder="QT_QPA_PLATFORM=offscreen"
                    />
                  </div>
                </>
              )}
            </CardContent>
          </Card>

          {error && (
            <p className="text-sm text-destructive">Error: {error}</p>
          )}

          <Button onClick={handleSave} disabled={deploying || createImage.isPending} className="w-full">
            <Hammer aria-hidden="true" className="mr-2 h-4 w-4" />
            {deploying ? "Deploying…" : "Save & Deploy"}
          </Button>

          {(buildLog || deployedId) && (
            <Card>
              <CardHeader>
                <CardTitle>Build Log</CardTitle>
                <CardDescription>
                  {deployedId ? "Image built on the host runtime daemon." : "Building…"}
                </CardDescription>
              </CardHeader>
              <CardContent>
                <pre className="max-h-96 overflow-auto rounded-md bg-muted p-3 text-xs">
                  {buildLog || "Waiting for build output…"}
                </pre>
              </CardContent>
            </Card>
          )}
        </div>

        <div className="space-y-1">
          <Label>Dockerfile preview (always visible — what gets built)</Label>
          <pre className="min-h-96 overflow-auto rounded-2xl glass-panel p-3 text-xs">
            <code className={cn(advanced ? "" : "text-muted-foreground")}>{preview || "# no content yet"}</code>
          </pre>
          {advanced && (
            <Textarea
              value={dockerfileOverride}
              onChange={(e) => setDockerfileOverride(e.target.value)}
              rows={18}
              className="mt-2 font-mono text-xs"
              placeholder={"FROM " + base + "\nUSER root\nRUN apt-get update && apt-get install -y --no-install-recommends \\\n      libgl1 libegl1 \\\n      && rm -rf /var/lib/apt/lists/*\nRUN chown -R 1000:1000 /usr /opt /var 2>/dev/null || true\nUSER orchicon\nWORKDIR /home/orchicon"}
            />
          )}
          <p className="text-xs text-muted-foreground">
            The base <code className="rounded bg-muted px-1">{base}</code> is always the first
            layer — the daemon rewrites the FROM line and injects the runtime-base label on build.
          </p>
        </div>
      </div>
    </div>
  );
}
