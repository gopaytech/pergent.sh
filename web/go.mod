// This file makes web/ a separate Go module, so `go ./...` in the repository
// root never descends into web/node_modules (some npm packages ship .go files).
// There is no Go code here.
module github.com/gopaytech/pergent.sh/web

go 1.24
