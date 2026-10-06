VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test integration vet

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/pbctl ./cmd/pbctl

vet:
	go vet ./...
	cd pbguard && go vet ./...

test:
	go test ./...

bin/pbserver: testdata/pbserver/main.go testdata/pbserver/go.mod pbguard/pbguard.go
	cd testdata/pbserver && go build -o ../../bin/pbserver .

integration: bin/pbserver
	PBCTL_TEST_SERVER=$(CURDIR)/bin/pbserver go test ./... -count=1
