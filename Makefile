GO_FILES := $(shell \
	find . '(' -path '*/.*' -o -path './vendor' ')' -prune \
	-o -name '*.go' -print | cut -b3-)

.PHONY: build
build:
	go build ./...

.PHONY: cover
cover:
	go test -coverprofile=cover.out -coverpkg=./... -v ./...
	go tool cover -html=cover.out -o cover.html

.PHONY: gofmt
gofmt:
	$(eval FMT_LOG := $(shell mktemp -t gofmt.XXXXX))
	@gofmt -e -s -l $(GO_FILES) > $(FMT_LOG) || true
	@[ ! -s "$(FMT_LOG)" ] || (echo "gofmt failed:" | cat - $(FMT_LOG) && false)

.PHONY: test
test:
	go test -race ./...

.PHONY: example
example:
	GOCACHE=$(CURDIR)/.gocache GOPATH=$(CURDIR)/.gopath go run ./cmd/example $(ARGS)

.PHONY: build-example
build-example:
	mkdir -p bin
	GOCACHE=$(CURDIR)/.gocache GOPATH=$(CURDIR)/.gopath go build -o ./bin/rpmlimiter-example ./cmd/example

.PHONY: run-example
run-example: build-example
	./bin/rpmlimiter-example $(ARGS)
