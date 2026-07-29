import js from "@eslint/js";
import globals from "globals";
import tseslint from "typescript-eslint";
import { defineConfig, globalIgnores } from "eslint/config";

export default defineConfig([
  globalIgnores(["dist"]),
  {
    // Type-aware linting for source. The rules that matter most here need type
    // information: an unawaited promise in an auth or file-access path fails
    // open rather than loudly.
    files: ["src/**/*.ts"],
    extends: [
      js.configs.recommended,
      tseslint.configs.recommended,
      tseslint.configs.recommendedTypeChecked,
    ],
    languageOptions: {
      globals: globals.node,
      parserOptions: {
        project: "./tsconfig.json",
        tsconfigRootDir: import.meta.dirname,
      },
    },
    rules: {
      // Dockerode returns `any` across most of its surface, which accounts for
      // nearly every violation of these. Failing the build on them would only
      // push `as` casts into the call sites, which hides the same problem
      // without checking anything. Left as warnings so newly introduced `any`
      // is still visible in review.
      "@typescript-eslint/no-unsafe-assignment": "warn",
      "@typescript-eslint/no-unsafe-member-access": "warn",
      "@typescript-eslint/no-unsafe-call": "warn",
      "@typescript-eslint/no-unsafe-argument": "warn",
    },
  },
  {
    // Tests are outside the build's tsconfig, so they get the same rules
    // without the type-aware layer.
    files: ["test/**/*.ts"],
    extends: [js.configs.recommended, tseslint.configs.recommended],
    languageOptions: {
      globals: globals.node,
    },
  },
]);
