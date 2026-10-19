import js from "@eslint/js";
import globals from "globals";
import reactHooks from "eslint-plugin-react-hooks";
import reactRefresh from "eslint-plugin-react-refresh";
import tseslint from "typescript-eslint";

export default tseslint.config(
  // GENERATED CODE IS NOT OURS TO LINT. `npm run lint` is `eslint .`, so every
  // directory under frontend/ that a TOOL writes into must be named here or the
  // whole gate fails on code nobody on this team wrote.
  //
  //   dist/           vite's bundle output
  //   src/api/gen/    buf's Connect-ES output (committed; gen-check proves it is current)
  //   wailsjs/        the Wails CLI's TypeScript bindings for the desktop shell
  //
  // wailsjs was the missing one, and it cost the operator a rebuild: `wails
  // build` (run by hand, and by the release workflow) emits frontend/wailsjs
  // into the frontend source tree, and the next `make rebuild-dev` died in
  // fe-lint on 16 errors in files generated that minute — `no-namespace`, and
  // `no-explicit-any` throughout Wails' own model marshalling. Its output is
  // not configurable (wails.json has no setting for it; the CLI writes
  // <frontend:dir>/wailsjs), so ignoring it is the only fix, and it is asserted
  // in cmd/orchicon/wailsjs_generated_ignored_test.go so a future edit to this
  // list cannot quietly reintroduce it.
  { ignores: ["dist", "src/api/gen", "wailsjs"] },
  {
    extends: [js.configs.recommended, ...tseslint.configs.recommended],
    files: ["**/*.{ts,tsx}"],
    languageOptions: {
      ecmaVersion: 2022,
      globals: globals.browser,
    },
    plugins: {
      "react-hooks": reactHooks,
      "react-refresh": reactRefresh,
    },
    rules: {
      ...reactHooks.configs.recommended.rules,
      "react-refresh/only-export-components": [
        "warn",
        { allowConstantExport: true },
      ],
    },
  }
);
