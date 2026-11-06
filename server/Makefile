TAGS = sqlite_fts5

build:
	go build -tags $(TAGS) -o cookievault-server ./cmd/server

build-plain:
	go build -o cookievault-server ./cmd/server

test:
	go vet ./...
	go test -tags $(TAGS) ./...

run: build
	./cookievault-server
