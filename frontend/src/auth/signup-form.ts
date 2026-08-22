// Sign-up form client-side validation. UX only (AGENTS.md: client-side
// validation is UX, not security) — the server re-validates every field at
// the boundary and is the security gate. Kept as a pure module so it is
// testable without a DOM.

// USERNAME_RE mirrors the server-side boundary regex (internal/auth
// service.go localUsernameRE): lowercase alphanumerics plus the separators
// common to email-style handles (. _ @ + -).
export const USERNAME_RE = /^[a-z0-9][a-z0-9._@+-]*$/;

// validateSignupForm returns an error message or null.
export function validateSignupForm(input: {
  username: string;
  password: string;
  confirm: string;
}): string | null {
  const username = input.username.trim();
  if (!username) return "Username is required";
  if (!USERNAME_RE.test(username)) {
    return "Username may only contain lowercase letters, numbers, and . _ @ + -";
  }
  if (input.password.length < 8) {
    return "Password must be at least 8 characters";
  }
  if (input.password !== input.confirm) {
    return "Passwords do not match";
  }
  return null;
}
