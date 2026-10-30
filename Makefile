.PHONY: build test vet clean

build:
	go build -o bin/pergent-sh ./cmd/pergent-sh

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -rf bin/
