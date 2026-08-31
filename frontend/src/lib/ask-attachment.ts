export const MAX_ATTACHMENTS = 5;
export const MAX_FILE_BYTES = 10 * 1024 * 1024;
export const MAX_TOTAL_BYTES = 20 * 1024 * 1024;

export const ACCEPTED_EXTS = new Set([
  "png","jpg","jpeg","gif","webp","svg","bmp","pdf","txt","md","mdx","json","csv","yaml","yml","html","css","js","ts","tsx","go","py","rs","java","sh","xml","log",
]);

export function getExt(name: string): string {
  return name.split(".").pop()?.toLowerCase() ?? "";
}

export function isAllowedFile(file: { name: string; type: string }): boolean {
  const mime = (file.type || "").toLowerCase();
  const ext = getExt(file.name);
  if (mime.startsWith("image/")) return true;
  if (mime.startsWith("text/")) return true;
  if (mime.includes("json") || mime.includes("csv") || mime.includes("pdf") || mime.includes("xml") || mime.includes("yaml")) return true;
  if (ext && ACCEPTED_EXTS.has(ext)) return true;
  if (!mime && ext && ACCEPTED_EXTS.has(ext)) return true;
  return false;
}

export function inferMime(file: { name: string; type: string }): string {
  if (file.type) return file.type;
  const ext = getExt(file.name);
  if (ext === "json") return "application/json";
  if (ext === "csv") return "text/csv";
  if (ext === "pdf") return "application/pdf";
  if (ext === "md" || ext === "mdx") return "text/markdown";
  if (ext === "txt") return "text/plain";
  return "application/octet-stream";
}

export type ValidationResult = { ok: true } | { ok: false; reason: string };

export function validateFileForAdd(opts: {
  file: { name: string; type: string; size: number };
  existingCount: number;
  existingTotalBytes: number;
  queuedCount: number;
  queuedBytes: number;
}): ValidationResult {
  const { file, existingCount, existingTotalBytes, queuedCount, queuedBytes } = opts;
  if (existingCount + queuedCount >= MAX_ATTACHMENTS) {
    return { ok: false, reason: `Too many attachments (max ${MAX_ATTACHMENTS})` };
  }
  if (!isAllowedFile(file)) {
    return { ok: false, reason: `Unsupported file type: ${file.name}` };
  }
  if (file.size > MAX_FILE_BYTES) {
    return { ok: false, reason: `File too large (max 10MB): ${file.name}` };
  }
  if (existingTotalBytes + queuedBytes + file.size > MAX_TOTAL_BYTES) {
    return { ok: false, reason: `Attachments too large (max 20MB total)` };
  }
  return { ok: true };
}
