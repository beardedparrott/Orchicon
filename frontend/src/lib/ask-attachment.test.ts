import { describe, it, expect } from "vitest";
import { isAllowedFile, getExt, inferMime, validateFileForAdd, MAX_ATTACHMENTS, MAX_FILE_BYTES, MAX_TOTAL_BYTES } from "./ask-attachment";

describe("ask-attachment helpers", () => {
  it("getExt lowercases", () => {
    expect(getExt("photo.JPG")).toBe("jpg");
    expect(getExt("archive.tar.gz")).toBe("gz");
    expect(getExt("noext")).toBe("noext");
  });

  it("isAllowedFile: images always allowed", () => {
    expect(isAllowedFile({ name: "pic.png", type: "image/png" })).toBe(true);
    expect(isAllowedFile({ name: "photo.jpg", type: "image/jpeg" })).toBe(true);
  });

  it("isAllowedFile: text/* allowed", () => {
    expect(isAllowedFile({ name: "a.txt", type: "text/plain" })).toBe(true);
    expect(isAllowedFile({ name: "a.md", type: "text/markdown" })).toBe(true);
  });

  it("isAllowedFile: json/csv/pdf/xml/yaml via mime or ext", () => {
    expect(isAllowedFile({ name: "data.json", type: "application/json" })).toBe(true);
    expect(isAllowedFile({ name: "data.json", type: "" })).toBe(true); // ext fallback
    expect(isAllowedFile({ name: "data.csv", type: "text/csv" })).toBe(true);
    expect(isAllowedFile({ name: "doc.pdf", type: "application/pdf" })).toBe(true);
    expect(isAllowedFile({ name: "doc.pdf", type: "" })).toBe(true);
    expect(isAllowedFile({ name: "file.yaml", type: "" })).toBe(true);
    expect(isAllowedFile({ name: "file.go", type: "" })).toBe(true);
    expect(isAllowedFile({ name: "file.py", type: "" })).toBe(true);
  });

  it("isAllowedFile: rejects unknown binary", () => {
    expect(isAllowedFile({ name: "evil.exe", type: "application/octet-stream" })).toBe(false);
    expect(isAllowedFile({ name: "archive.zip", type: "application/zip" })).toBe(false);
    // unless ext is allowlisted, zip is not
    expect(isAllowedFile({ name: "archive.zip", type: "" })).toBe(false);
  });

  it("isAllowedFile: empty mime but known ext succeeds", () => {
    expect(isAllowedFile({ name: "notes.md", type: "" })).toBe(true);
    expect(isAllowedFile({ name: "script.js", type: "" })).toBe(true);
  });

  it("inferMime fills empty type via ext", () => {
    expect(inferMime({ name: "a.json", type: "" })).toBe("application/json");
    expect(inferMime({ name: "a.pdf", type: "" })).toBe("application/pdf");
    expect(inferMime({ name: "a.md", type: "" })).toBe("text/markdown");
    expect(inferMime({ name: "a.txt", type: "" })).toBe("text/plain");
    expect(inferMime({ name: "a.png", type: "image/png" })).toBe("image/png");
    expect(inferMime({ name: "unknown.bin", type: "" })).toBe("application/octet-stream");
  });

  it("validateFileForAdd: count cap", () => {
    const file = { name: "a.txt", type: "text/plain", size: 100 };
    expect(validateFileForAdd({ file, existingCount: 5, existingTotalBytes: 0, queuedCount: 0, queuedBytes: 0 }).ok).toBe(false);
    expect(validateFileForAdd({ file, existingCount: 4, existingTotalBytes: 0, queuedCount: 1, queuedBytes: 0 }).ok).toBe(false);
    expect(validateFileForAdd({ file, existingCount: 4, existingTotalBytes: 0, queuedCount: 0, queuedBytes: 0 }).ok).toBe(true);
  });

  it("validateFileForAdd: per-file size", () => {
    const big = { name: "big.png", type: "image/png", size: MAX_FILE_BYTES + 1 };
    const r = validateFileForAdd({ file: big, existingCount: 0, existingTotalBytes: 0, queuedCount: 0, queuedBytes: 0 });
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.reason).toMatch(/10MB/);
  });

  it("validateFileForAdd: total size 20MB", () => {
    const file = { name: "a.txt", type: "text/plain", size: 5 * 1024 * 1024 };
    // existing 16MB + 5MB = 21MB >20
    const r = validateFileForAdd({ file, existingCount: 0, existingTotalBytes: 16 * 1024 * 1024, queuedCount: 0, queuedBytes: 0 });
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.reason).toMatch(/20MB/);
    // queuedBytes also counts
    const r2 = validateFileForAdd({ file, existingCount: 0, existingTotalBytes: 10 * 1024 * 1024, queuedCount: 0, queuedBytes: 10 * 1024 * 1024 });
    expect(r2.ok).toBe(false);
  });

  it("validateFileForAdd: unsupported type", () => {
    const f = { name: "bad.exe", type: "application/octet-stream", size: 100 };
    const r = validateFileForAdd({ file: f, existingCount: 0, existingTotalBytes: 0, queuedCount: 0, queuedBytes: 0 });
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.reason).toMatch(/Unsupported/);
  });

  it("attach → send flow sizes match server caps", () => {
    expect(MAX_ATTACHMENTS).toBe(5);
    expect(MAX_FILE_BYTES).toBe(10 * 1024 * 1024);
    expect(MAX_TOTAL_BYTES).toBe(20 * 1024 * 1024);
  });

  it("drag-drop and picker allow same set", () => {
    // picker accept list includes images, text, json, md, csv, pdf etc; drag should match
    const pickerFiles = [
      { name: "img.png", type: "image/png" },
      { name: "doc.pdf", type: "application/pdf" },
      { name: "data.json", type: "application/json" },
      { name: "notes.md", type: "" },
      { name: "code.go", type: "" },
    ];
    for (const f of pickerFiles) expect(isAllowedFile(f)).toBe(true);
  });
});
