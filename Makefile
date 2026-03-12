
GO_BUILD_ENV :=
GO_BUILD_FLAGS :=
MODULE_BINARY := bin/beanjamin

ifeq ($(VIAM_TARGET_OS), windows)
	GO_BUILD_ENV += GOOS=windows GOARCH=amd64
	GO_BUILD_FLAGS := -tags no_cgo
	MODULE_BINARY = bin/beanjamin.exe
endif

$(MODULE_BINARY): Makefile go.mod *.go cmd/module/*.go
	GOOS=$(VIAM_BUILD_OS) GOARCH=$(VIAM_BUILD_ARCH) $(GO_BUILD_ENV) go build $(GO_BUILD_FLAGS) -o $(MODULE_BINARY) cmd/module/main.go

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
	tar czf $@ meta.json README.md first_run.sh $(MODULE_BINARY)

module: test module.tar.gz

web-app-install:
	cd web-app && npm ci

web-app-build: web-app-install
	cd web-app && npm run build

WEB_APP_BINARY := web-app/beanjamin-app

$(WEB_APP_BINARY): cmd/web-app/main.go
	go build -o $@ ./cmd/web-app/

web-app-module: web-app-build $(WEB_APP_BINARY)
	cd web-app && tar czf module.tar.gz out beanjamin-app meta.json

all: test module.tar.gz web-app-module

setup:
ifeq ($(shell uname), Darwin)
	brew tap viamrobotics/brews
	brew install nlopt-static
else ifeq ($(shell uname), Linux)
	sudo apt-get update && sudo apt-get install -y --no-install-recommends libnlopt-dev
endif
	go mod tidy
