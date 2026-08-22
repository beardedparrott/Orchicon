import { describe, expect, it } from "vitest";

import { validateSignupForm } from "@/auth/signup-form";

describe("validateSignupForm", () => {
  it("accepts a valid signup (trimmed email-style handle, >=8 char password, matching confirm)", () => {
    expect(
      validateSignupForm({ username: " jane@example.com ", password: "password-123", confirm: "password-123" }),
    ).toBeNull();
  });

  it("rejects an empty username", () => {
    expect(validateSignupForm({ username: " ", password: "password-123", confirm: "password-123" })).toBe(
      "Username is required",
    );
  });

  it("rejects an out-of-charset username (uppercase / spaces)", () => {
    expect(validateSignupForm({ username: "BAD USER", password: "password-123", confirm: "password-123" })).toContain(
      "lowercase",
    );
  });

  it("accepts the allowed separator characters (. _ @ + -)", () => {
    expect(validateSignupForm({ username: "ok+user", password: "password-123", confirm: "password-123" })).toBeNull();
    expect(
      validateSignupForm({ username: "jane.doe_2@example.com", password: "password-123", confirm: "password-123" }),
    ).toBeNull();
  });

  it("rejects a short password (UX min-length hint)", () => {
    expect(validateSignupForm({ username: "jane", password: "short", confirm: "short" })).toBe(
      "Password must be at least 8 characters",
    );
  });

  it("rejects a confirm-password mismatch", () => {
    expect(validateSignupForm({ username: "jane", password: "password-123", confirm: "password-124" })).toBe(
      "Passwords do not match",
    );
  });
});
