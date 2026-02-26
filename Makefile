
GO_BUILD_ENV :=
GO_BUILD_FLAGS :=
MODULE_BINARY := bin/beanjamin

ifeq ($(VIAM_TARGET_OS), windows)
	GO_BUILD_ENV += GOOS=windows GOARCH=amd64
	GO_BUILD_FLAGS := -tags no_cgo
	MODULE_BINARY = bin/beanjamin.exe
endif

CLI_BINARY := bin/beanjamin-cli

$(MODULE_BINARY): Makefile go.mod *.go cmd/module/*.go
	GOOS=$(VIAM_BUILD_OS) GOARCH=$(VIAM_BUILD_ARCH) $(GO_BUILD_ENV) go build $(GO_BUILD_FLAGS) -o $(MODULE_BINARY) cmd/module/main.go

$(CLI_BINARY): Makefile go.mod *.go cmd/cli/*.go
	go build -o $(CLI_BINARY) ./cmd/cli/

lint:
	gofmt -s -w .

update:
	go get go.viam.com/rdk@latest
	go mod tidy

test:
	go test ./...

module.tar.gz: meta.json $(MODULE_BINARY)
ifneq ($(VIAM_TARGET_OS), windows)
	strip $(MODULE_BINARY)
endif
	tar czf $@ meta.json $(MODULE_BINARY)

module: test module.tar.gz

all: test module.tar.gz

setup:
	go mod tidy
