<!-- pergent -->
### ❌ 1 blocking issue

<sub>Commit `0123456` · 3 files · 1 of 2 checks passed · 1 blocking · 1 suggestion</sub>

#### Findings (2)

<details open><summary>❌ <b>Unchecked input</b> · <a href="https://github.com/o/r/blob/0123456789abcdef0123456789abcdef01234567/db/query.go?plain=1#L3"><code>db/query.go:3</code></a></summary>

plain

Not a real problem? Reply `@pergent-bot dismiss abc123 <reason>`

</details>

<details><summary>💡 <b>Unchecked input</b> · <a href="https://github.com/o/r/blob/0123456789abcdef0123456789abcdef01234567/db/query.go?plain=1#L3"><code>db/query.go:3</code></a></summary>

a smaller one

Not a real problem? Reply `@pergent-bot dismiss fedcba <reason>`

</details>

#### Input

| Check | Status | Detail |
|---|---|---|
| Queries are parameterised | ❌ | string concatenation |
| Output is escaped | ✅ |  |
| Uploads are checked | ➖ |  |

<details><summary>Files (3)</summary>

- `db/query.go`
- `go.sum` — skipped: lockfile
- `web/page.go`

</details>

> 1 failed check has no matching finding; the verdict is based on findings.

<sub>Reviewed by [pergent](https://github.com/gopaytech/pergent.sh) · Run again: `@pergent-bot review` · pergent v0.5.5</sub>

<!-- pergent:findings eyJ2ZXJzaW9uIjoxLCJza2lsbHMiOnsic2VjdXJpdHkiOlt7ImZpbGUiOiJkYi9xdWVyeS5nbyIsImxpbmUiOjMsInNldmVyaXR5IjoiYmxvY2tpbmciLCJ0aXRsZSI6IlVuY2hlY2tlZCBpbnB1dCIsImV4cGxhbmF0aW9uIjoicGxhaW4iLCJmaW5nZXJwcmludCI6ImFiYzEyM2RlZjQ1NiJ9LHsiZmlsZSI6ImRiL3F1ZXJ5LmdvIiwibGluZSI6Mywic2V2ZXJpdHkiOiJzdWdnZXN0aW9uIiwidGl0bGUiOiJVbmNoZWNrZWQgaW5wdXQiLCJleHBsYW5hdGlvbiI6ImEgc21hbGxlciBvbmUiLCJmaW5nZXJwcmludCI6ImZlZGNiYTY1NDMyMSJ9XX19 -->
