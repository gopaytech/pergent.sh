import js from "@eslint/js";
import reactHooks from "eslint-plugin-react-hooks";
import globals from "globals";
import tseslint from "typescript-eslint";

// Text from the API (findings, logs, titles) is model or user text: it is
// only ever rendered as text. These rules keep HTML out of the page.
const noDanger = "Render API text as text: no dangerouslySetInnerHTML (react/no-danger).";
const noHTML = {
  "no-restricted-syntax": [
    "error",
    { selector: "JSXAttribute[name.name='dangerouslySetInnerHTML']", message: noDanger },
    { selector: "Property[key.name='dangerouslySetInnerHTML']", message: noDanger },
    { selector: "Property[key.value='dangerouslySetInnerHTML']", message: noDanger },
    {
      selector: "MemberExpression[property.name=/^(innerHTML|outerHTML)$/]",
      message: "Render API text as text: no innerHTML or outerHTML.",
    },
    {
      selector: "MemberExpression[property.value=/^(innerHTML|outerHTML)$/]",
      message: "Render API text as text: no innerHTML or outerHTML.",
    },
    {
      selector: "CallExpression[callee.property.name=/^(insertAdjacentHTML|createContextualFragment)$/]",
      message: "Render API text as text: no insertAdjacentHTML or createContextualFragment.",
    },
    {
      selector: "CallExpression[callee.object.name='document'][callee.property.name=/^(write|writeln)$/]",
      message: "Render API text as text: no document.write or document.writeln.",
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
