.PHONY: build test install
build:
	mkdir -p bin
	go build -trimpath -o bin/artifact ./cmd/artifact
	go build -trimpath -o bin/artifactd ./cmd/artifactd
test:
	go test -race ./...
	go vet ./...
install: build
	install -Dm755 bin/artifact $(HOME)/.local/bin/artifact
	install -Dm755 bin/artifactd $(HOME)/.local/bin/artifactd
