.PHONY: build test vet clean web

build:
	go build -o bin/pergent-sh ./cmd/pergent-sh

# Builds the dashboard into internal/web/dist, which `build` embeds. Only
# dist/.keep is tracked, so this leaves the work tree clean.
web:
	cd web && npm ci && npm run build

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -rf bin/

.PHONY: readme
readme:
	helm-docs -c ./charts/pergent-sh

.PHONY: helm.create.releases
helm.create.releases:
	helm package charts/pergent-sh --destination charts/releases
	helm repo index charts/releases
