import js from "@eslint/js";
import reactHooks from "eslint-plugin-react-hooks";
import globals from "globals";
import tseslint from "typescript-eslint";

// Text from the API (findings, logs, titles) is model or user text: it is
// only ever rendered as text. These rules keep HTML out of the page.
const noHTML = {
  "no-restricted-syntax": [
    "error",
    {
      selector: "JSXAttribute[name.name='dangerouslySetInnerHTML']",
      message: "Render API text as text: no dangerouslySetInnerHTML (react/no-danger).",
    },
    {
      selector: "Property[key.name='dangerouslySetInnerHTML']",
      message: "Render API text as text: no dangerouslySetInnerHTML (react/no-danger).",
    },
    {
      selector: "MemberExpression[property.name=/^(innerHTML|outerHTML)$/]",
      message: "Render API text as text: no innerHTML or outerHTML.",
    },
    {
      selector: "CallExpression[callee.property.name='insertAdjacentHTML']",
      message: "Render API text as text: no insertAdjacentHTML.",
    },
  ],
};

export default tseslint.config(
  { ignores: ["node_modules", "../internal/web/dist"] },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  {
    files: ["**/*.{ts,tsx}"],
    languageOptions: { globals: globals.browser },
    plugins: { "react-hooks": reactHooks },
    rules: { ...reactHooks.configs.recommended.rules, ...noHTML },
  },
  {
    files: ["*.config.{js,ts}"],
    languageOptions: { globals: globals.node },
  },
);
