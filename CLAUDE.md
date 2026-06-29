# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository overview

`viam:beanjamin` is a Viam module that automates a Viam OK-1 arm to prepare espresso. The repo ships two deployables:

- A Go **Viam module** (`cmd/module/main.go`) registering seven models — see `meta.json` and `README.md` for the full list and per-model configuration docs. The headline model is `viam:beanjamin:coffee` (generic service) which orchestrates the full brew cycle.
- A Next.js **web app** (`web-app/`) that exposes the customer-facing ordering UI and talks to the machine via `@viamrobotics/sdk`. Packaged as its own Viam module via `web-app-module`.

Each model lives in its own package. The coffee service is `coffee/` (`package coffee`), whose files split by concern — lifecycle and command API (`module.go`, `config.go`, `api.go`, `control.go`), the brew cycle and serving (`espresso.go`, `brew_steps.go`, `serving.go`, `iced.go`, `queue.go`, `troubleshooting.go`), motion planning (`motion.go`, `held_geometry.go`, `collisions.go`, `joints.go`), vision-driven pickup and serving-area placement (`cup_pickup.go`, `served_shelf.go`, `gripper_state.go`), and peripheral integrations (`greetings.go`, `cam_storage.go`, `slack_notify.go`, `sensor_usage.go`, `order_sensor.go` — the order-sensor model is bundled here because it shares the coffee `Order` type). The remaining models are sibling packages: `maintenancesensor/`, `customerdetector/`, `dialcontrolmotion/`, `multiposesexecutionswitch/`, `texttospeech/`. There is no top-level Go package; `cmd/module/main.go` registers every model.

## Common commands

Go module (run from repo root):

```bash
make                  # build bin/beanjamin (default target)
make test             # go test ./...
make lint             # gofmt -s -w . && golangci-lint run
make module.tar.gz    # package for Viam (use `make module` to run tests first)
make setup            # install nlopt (brew on macOS, apt on Linux) + go mod tidy
```

Run a single Go test:

```bash
go test ./... -run TestName
go test ./coffee -run TestEnqueueOrder   # coffee package
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

`prepareDrink` in `coffee/espresso.go` is the core orchestrator. An order flows:

1. `DoCommand{"prepare_order": ...}` enqueues an `Order` into `OrderQueue` (`coffee/queue.go`).
2. A background queue consumer (`beanjaminCoffee.processQueue`) pops one order at a time and invokes `prepareDrink`.
3. `prepareDrink` advances through 9 steps; each step sets a label via `setStep(...)` that's visible through `get_queue` and the order sensor. Steps are implemented as small methods (`grindCoffee`, `tampGround`, `brew`, `cleanPortafilter`, etc. in `coffee/brew_steps.go`) that each execute a list of `Step` structs through `executeStep` (`coffee/espresso.go`), which drives the motion layer in `coffee/motion.go`.
4. On completion/failure, the order is moved to `recent` for `RecentDisplayDuration` (15s) so the UI can render "Ready!" without diffing polls.
5. A single reading per attempt is pushed to the optional order-sensor sink, and an async clip save is requested on the optional `cam_storage_mux_name` video-store multiplexer.

`cancel`, `clear_queue`, and `proceed` manipulate the same state. Only one routine runs at a time, gated by `running atomic.Bool`; a shared `cancelCtx` is captured under `mu` so cancellation can interrupt motion.

### Motion layer

`coffee/motion.go` wraps Viam's motion-planning APIs. Poses are resolved through `multi-poses-execution-switch` components (one for the filter, one for the claws, configured via `pose_switcher_name` / `claws_pose_switcher_name`). Each `Step` declares a pose name, optional linear constraint, optional circular motion (used for grinding/cleaning), and optional allowed collisions for contact phases. `save_motion_requests_dir`, if set, dumps motion-request JSON per plan for offline debugging. `viz_url` streams the frame system to a motion-tools viz server before each plan.

### Config pattern

All models follow the standard Viam `Validate`/`newX` pattern — see `coffee/config.go` for the coffee service's `Config`, and the sibling model packages (e.g. `dialcontrolmotion/`) for simpler examples. When adding a new tunable, follow the `BrewTimeSec` / `GrindTimeSec` pattern: add a `float64` field with `omitempty`, add a small helper on `beanjaminCoffee` that returns the configured value or a default constant defined near the feature's code.

### Web app

`web-app/app/page.tsx` is a thin view selector: it renders the **fleet dashboard** (`web-app/app/dashboard.tsx`, with `web-app/app/home/` components — a machine list with a per-machine status dot and queue summary, order charts, and leaderboards) or the single-machine **kiosk** (`web-app/app/kiosk.tsx`, with `web-app/app/order/` components — `welcome` → `drink` → `name` → `face-register` → `confirmation`, with a right-rail `order-tracker`) based on the `?view=` query param (`?view=machine` selects the kiosk). Everything is served from one static entrypoint, so the kiosk can't have its own route — a refresh on a nested path would 404. `web-app/app/lib/viamClient.ts` wraps the Viam TS SDK and is where `DoCommand`s are issued against the coffee and customer-detector services. Connection lifecycle helpers live in `web-app/app/lib/connectionManager.ts`: `withTimeout`/`disconnectQuietly` (the dial-timeout and teardown primitives) plus `createConnectionManager`, a per-machine connection pool with in-flight dedup that the dashboard uses (one pooled connection per machine). `useViamConnection.ts` is the kiosk's single connection — it dials directly with those shared helpers and runs a heartbeat, without the pool. The tracker polls `get_queue` to render step state and ready-to-pick-up cards; the dashboard polls each machine and colors its status dot green (coffee service answering), yellow (reachable but coffee service absent), or gray (offline).

## Lint and style

`.golangci.yml` disables several staticcheck rules — most notably `ST1005` (capitalized errors are fine, e.g. "Google") and `ST1003` (snake_case identifiers are tolerated). Always run `make lint` before committing Go changes; it runs `gofmt -s -w .` in place.

## Conventions

- **Pose work**: when iterating on arm poses, use `viam robot part motion get-pose` / `set-pose` against a running machine (see the README "Development" section) rather than guessing coordinates; commit new poses into the `multi-poses-execution-switch` config only after verifying them physically.
- **Ordered steps carry labels**: when editing `prepareDrink`, keep `setStep("...")` in sync with the numbered `logger.Infof("step N/9: ...")` — both surface to the UI and the tracker collapses/expands based on the raw label.
- **Config docs live in `README.md`**: when you add or rename a `Config` field, update the corresponding model section in `README.md` so operators have an accurate reference.
