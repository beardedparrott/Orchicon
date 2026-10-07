// Minimal DOM shim for tests that import the theme store.
//
// The store applies its palette to `document.documentElement` at MODULE INIT
// (theme-store.ts `apply`), so importing it under vitest's node environment
// throws before any test can run. The repo ships no jsdom on purpose (see
// SessionGrants.test.tsx), and the tests that need this assert on EMITTED MARKUP
// rather than layout — so all that is required is the handful of things the store
// actually touches. This is the same trade as the localStorage shim in
// work-items-preferences.test.ts.
//
// IT MUST BE IMPORTED BEFORE theme-store, and as a SEPARATE MODULE: ES module
// imports are hoisted and evaluated in declaration order, so a shim written
// inline in the test file would still run after the store's own import was
// evaluated.
const classList = {
  _s: new Set<string>(),
  add(c: string) {
    this._s.add(c);
  },
  remove(c: string) {
    this._s.delete(c);
  },
  contains(c: string) {
    return this._s.has(c);
  },
  toggle(c: string, force?: boolean) {
    const on = force ?? !this._s.has(c);
    if (on) this._s.add(c);
    else this._s.delete(c);
    return on;
  },
};

const documentElement = {
  classList,
  setAttribute: () => {},
  removeAttribute: () => {},
  style: { setProperty: () => {}, removeProperty: () => {} },
};

Object.defineProperty(globalThis, "document", {
  configurable: true,
  value: {
    documentElement,
    body: { classList, setAttribute: () => {}, removeAttribute: () => {} },
    addEventListener: () => {},
    removeEventListener: () => {},
  },
});

Object.defineProperty(globalThis, "window", {
  configurable: true,
  value: {
    matchMedia: () => ({
      matches: false,
      addEventListener: () => {},
      removeEventListener: () => {},
    }),
    addEventListener: () => {},
    removeEventListener: () => {},
  },
});

export {};
