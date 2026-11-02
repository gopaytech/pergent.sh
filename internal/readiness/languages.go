package readiness

import (
	"path"
	"strings"
)

// languages maps a lower-case file extension to its language.
var languages = map[string]string{}

func init() {
	for name, exts := range map[string]string{
		"Go":               ".go",
		"Python":           ".py",
		"JavaScript":       ".js .jsx .mjs .cjs",
		"TypeScript":       ".ts .tsx",
		"Java":             ".java",
		"Kotlin":           ".kt .kts",
		"Ruby":             ".rb",
		"PHP":              ".php",
		"C#":               ".cs",
		"C":                ".c .h",
		"C++":              ".cc .cpp .cxx .hpp .hh",
		"Rust":             ".rs",
		"Swift":            ".swift",
		"Scala":            ".scala",
		"Shell":            ".sh .bash",
		"SQL":              ".sql",
		"Terraform":        ".tf",
		"Dart":             ".dart",
		"Elixir":           ".ex .exs",
		"Lua":              ".lua",
		"Vue":              ".vue",
		"Svelte":           ".svelte",
		"Objective-C":      ".m .mm",
		"Protocol Buffers": ".proto",
	} {
		for _, ext := range strings.Fields(exts) {
			languages[ext] = name
		}
	}
}

// LanguageOf is the language of a file by its extension, in any case, or ""
// when the extension is not a listed one.
func LanguageOf(p string) string {
	return languages[strings.ToLower(path.Ext(p))]
}
