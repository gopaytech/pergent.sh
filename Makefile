.PHONY: build test vet clean

build:
	go build -o bin/pergent-sh ./cmd/pergent-sh

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
