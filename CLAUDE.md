# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository overview

`viam:beanjamin` is a Viam module that automates a Viam OK-1 arm to prepare espresso. The repo ships two deployables:

- A Go **Viam module** (`cmd/module/main.go`) registering seven models — see `meta.json` and `README.md` for the full list and per-model configuration docs. The headline model is `viam:beanjamin:coffee` (generic service) which orchestrates the full brew cycle.
- A Next.js **web app** (`web-app/`) that exposes the customer-facing ordering UI and talks to the machine via `@viamrobotics/sdk`. Packaged as its own Viam module via `web-app-module`.

The top-level Go package is `beanjamin` (`module.go`, `espresso.go`, `motion.go`, `queue.go`, `greetings.go`, `maintenance_sensor.go`, `order_sensor.go`, `zoo_cam.go`). Sibling packages in subdirectories each back one of the other models: `customerdetector/`, `dialcontrolmotion/`, `multiposesexecutionswitch/`, `texttospeech/`.

## Common commands

Go module (run from repo root):

```bash
make                  # build bin/beanjamin (default target)
make test             # go test ./...
make lint             # gofmt -s -w . && golangci-lint run
make module.tar.gz    # package for Viam (runs tests first via `make module`)
make setup            # install nlopt (brew on macOS, apt on Linux) + go mod tidy
```

Run a single Go test:

```bash
go test ./... -run TestName
go test -run TestOrderQueue ./...        # top-level package
go test ./customerdetector -run TestFoo  # subpackage
```

Web app (run from `web-app/`):

```bash
npm ci                # install (matches Makefile's web-app-install)
npm run dev           # next dev
npm run build         # next build (static export into web-app/out)
npm run lint          # eslint
```

Build the bundled web-app Viam module from repo root: `make web-app-module` (runs `npm run build` + builds `cmd/web-app` Go launcher).

## Architecture

### Coffee service lifecycle

`prepareDrink` in `espresso.go` is the core orchestrator. An order flows:

1. `DoCommand{"prepare_order": ...}` enqueues an `Order` into `OrderQueue` (`queue.go`).
2. A background queue consumer (`beanjaminCoffee.processQueue`) pops one order at a time and invokes `prepareDrink`.
3. `prepareDrink` advances through 9 steps; each step sets a label via `setStep(...)` that's visible through `get_queue` and the order sensor. Steps are implemented as small methods (`grindCoffee`, `tampGround`, `brew`, `cleanPortafilter`, etc.) that each execute a list of `Step` structs through `executeStep` in `motion.go`.
4. On completion/failure, the order is moved to `recent` for `RecentDisplayDuration` (15s) so the UI can render "Ready!" without diffing polls.
5. A single reading per attempt is pushed to the optional order-sensor sink, and an async clip save is requested on the optional `zoo_cam_storage_name` video-store camera.

`cancel`, `clear_queue`, and `proceed` manipulate the same state. Only one routine runs at a time, gated by `running atomic.Bool`; a shared `cancelCtx` is captured under `mu` so cancellation can interrupt motion.

### Motion layer

`motion.go` wraps Viam's motion-planning APIs. Poses are resolved through `multi-poses-execution-switch` components (one for the filter, one for the claws, configured via `pose_switcher_name` / `claws_pose_switcher_name`). Each `Step` declares a pose name, optional linear constraint, optional circular motion (used for grinding/cleaning), and optional allowed collisions for contact phases. `save_motion_requests_dir`, if set, dumps motion-request JSON per plan for offline debugging. `viz_url` streams the frame system to a motion-tools viz server before each plan.

### Config pattern

All models follow the standard Viam `Validate`/`newX` pattern — see `DEVELOPER_GUIDE.md` for the scaffolding and `module.go` for the coffee service's `Config`. When adding a new tunable, follow the `BrewTimeSec` / `GrindTimeSec` pattern: add a `float64` field with `omitempty`, add a small helper on `beanjaminCoffee` that returns the configured value or a default constant defined near the feature's code.

### Web app

`web-app/app/page.tsx` is a single-page kiosk flow (`enter-name` → `face-register` → `choose-drink` → `order-confirmation` → `order-tracker`). `web-app/app/lib/viamClient.ts` is the only entry point that talks to the robot — it wraps the Viam TS SDK and issues `DoCommand`s against the coffee service. The tracker polls `get_queue` to render step state and ready-to-pick-up cards.

## Lint and style

`.golangci.yml` disables several staticcheck rules — most notably `ST1005` (capitalized errors are fine, e.g. "Google") and `ST1003` (snake_case identifiers are tolerated). Always run `make lint` before committing Go changes; it runs `gofmt -s -w .` in place.

## Conventions

- **Pose work**: when iterating on arm poses, use `viam robot part motion get-pose` / `set-pose` against a running machine (see the README "Development" section) rather than guessing coordinates; commit new poses into the `multi-poses-execution-switch` config only after verifying them physically.
- **Ordered steps carry labels**: when editing `prepareDrink`, keep `setStep("...")` in sync with the numbered `logger.Infof("step N/9: ...")` — both surface to the UI and the tracker collapses/expands based on the raw label.
- **Config docs live in `README.md`**: when you add or rename a `Config` field, update the corresponding model section in `README.md` so operators have an accurate reference.
