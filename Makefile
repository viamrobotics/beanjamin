
GO_BUILD_ENV :=
GO_BUILD_FLAGS :=
MODULE_BINARY := bin/beanjamin

ifeq ($(VIAM_TARGET_OS), windows)
	GO_BUILD_ENV += GOOS=windows GOARCH=amd64
	GO_BUILD_FLAGS := -tags no_cgo
	MODULE_BINARY = bin/beanjamin.exe
endif

# All Go sources the module binary depends on, now spread across per-model
# packages (coffee/, maintenancesensor/, …) rather than the repo root.
GO_SOURCES := $(shell find . -name '*.go' -not -path './web-app/*')

$(MODULE_BINARY): Makefile go.mod $(GO_SOURCES)
	GOOS=$(VIAM_BUILD_OS) GOARCH=$(VIAM_BUILD_ARCH) $(GO_BUILD_ENV) go build $(GO_BUILD_FLAGS) -o $(MODULE_BINARY) cmd/module/main.go

lint:
	gofmt -s -w .
	golangci-lint run

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

CLI_BINARY := bin/beanjamin-cli

$(CLI_BINARY): Makefile go.mod $(GO_SOURCES)
	go build -o $(CLI_BINARY) ./cmd/cli/

# Downloads one order's saved motion plan requests into ./<order-id>/, flattened
# out of the export's tag= directories and renamed so alphabetical order is
# execution order. Needs a logged-in `viam` CLI.
# Usage: make fetch-order ORDER=<order-id> [WITH_VIDEO=1] [FETCH_FLAGS="--out /tmp"]
FETCH_FLAGS ?=
ifdef WITH_VIDEO
FETCH_FLAGS += --with-video
endif

fetch-order: $(CLI_BINARY)
	@test -n "$(ORDER)" || { echo 'usage: make fetch-order ORDER=<order-id>'; exit 1; }
	$(CLI_BINARY) fetch-order $(FETCH_FLAGS) $(ORDER)

# Prints the most recent orders and how each ended, read from the order-events
# sensor's tabular data. Needs a logged-in `viam` CLI.
# Usage: make orders [LIMIT=50] [ORDERS_FLAGS="--errors --newest-first"]
LIMIT ?= 20

orders: $(CLI_BINARY)
	$(CLI_BINARY) orders --limit $(LIMIT) $(ORDERS_FLAGS)

web-app-install:
	cd web-app && npm ci

web-app-build: web-app-install
	cd web-app && npm run build

# Dev targets deliberately skip web-app-install: `npm ci` wipes and reinstalls
# node_modules, which is right before a build but wrong in a dev loop. Run
# `make web-app-install` yourself after a dependency change.
web-app-dev:
	cd web-app && npm run dev

# Serves the dev server through a Viam origin so the userToken cookie is set and
# the app can reach real machines. Needs `make web-app-dev` running alongside it;
# then open http://localhost:8012.
web-app-local-test:
	cd web-app && viam module local-app-testing --app-url http://localhost:3000

web-app-test:
	cd web-app && npm test

# Part IDs for the pose calibration manifest. Defaults to whatever the
# checked-in manifest already covers, so a bare `make web-app-manifest` just
# refreshes it; pass PART_IDS="<id> ..." to add or change machines.
PART_IDS ?= $(shell sed -n 's/.*"partId": "\([^"]*\)".*/\1/p' web-app/app/lib/calibrationManifest.ts 2>/dev/null)

web-app-manifest:
	@test -n "$(PART_IDS)" || { echo 'no part IDs found; pass PART_IDS="<partId> ..."'; exit 1; }
	cd web-app && npm run gen:manifest -- $(PART_IDS)

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
