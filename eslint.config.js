import js from "@eslint/js";
import tsParser from "@typescript-eslint/parser";
import globals from "globals";

// The Node half of the repo: the scripts the deploy runs and the tests that
// hold the stacks, the workflows and the public-repo rules. The listener is
// Go, checked by `go vet` in `npm run go`.
export default [
  { ignores: ["node_modules/**"] },
  {
    files: ["**/*.mjs", "**/*.js", "**/*.ts"],
    languageOptions: {
      ecmaVersion: "latest",
      sourceType: "module",
      globals: { ...globals.node },
    },
    rules: {
      ...js.configs.recommended.rules,
      // A leading underscore marks a binding kept for its position.
      "no-unused-vars": ["error", { argsIgnorePattern: "^_", varsIgnorePattern: "^_", destructuredArrayIgnorePattern: "^_" }],
    },
  },
  {
    // tsc checks names and unused bindings in TypeScript.
    files: ["**/*.ts"],
    languageOptions: { parser: tsParser },
    rules: { "no-unused-vars": "off", "no-undef": "off", "no-redeclare": "off" },
  },
];
