VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  = -s -w -X main.version=$(VERSION)

.PHONY: build test lint lambda clean

build:
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o bin/spendfuse ./cmd/spendfuse

test:
	go test ./... -race -count=1

lint:
	go vet ./...

# Lambda custom runtime expects a binary named `bootstrap` at the zip root.
lambda:
	@mkdir -p dist
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -tags lambda.norpc -o dist/bootstrap ./cmd/spendfuse-lambda
	cd dist && rm -f spendfuse-lambda.zip && zip -q spendfuse-lambda.zip bootstrap
	@ls -la dist/spendfuse-lambda.zip

clean:
	rm -rf bin dist
