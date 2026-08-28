# Beanjamin Web App

Customer-facing kiosk UI for the Beanjamin espresso robot. Built with Next.js and communicates with the robot via the Viam TypeScript SDK.

## Routes

- `/` — fleet dashboard. Lists machines accessible to the logged-in user with a live per-machine queue summary, plus order charts and leaderboards. Each row offers two links: the row itself opens the kiosk in **standard mode**, and a `[kiosk mode →]` link opens it in **kiosk mode**. A status dot precedes each machine name:
  - 🟢 **green** — online and the coffee-lifecycle service is answering.
  - 🟡 **yellow** — online and reachable, but the machine isn't running the coffee-lifecycle service.
  - ⚪ **gray** — offline.
- `/machine?partId=<partId>` — kiosk for a specific robot part. The `partId` is resolved to an FQDN via `appClient.getRobotPart` and the kiosk connects directly to the machine.

### Query parameters

- `partId` — the robot part to connect to (required for real connections).
- `kiosk=1` — kiosk mode: hides the "← Back to Fleet Dashboard" link on the welcome and order-confirmation screens. Use when the page is the only thing on screen and you don't want customers navigating away.
- `mock=1` / `mock=0` — see dev-mode rules below.

## Running locally

Requires Node.js 20.20.2 or later.

Install dependencies (from `web-app/`):

```bash
npm ci
```

Start the dev server:

```bash
npm run dev
```

Open [http://localhost:3000](http://localhost:3000). On localhost without a `userToken` cookie, the app runs in **dev/mock mode** — no real robot connection needed. The home page shows an "Open dev kiosk" button that opens the kiosk against a simulated queue.

### Dev/mock mode rules

Defined in [`app/lib/viamClient.ts`](app/lib/viamClient.ts):

- No `userToken` cookie → dev mode (forced).
- `userToken` cookie present:
  - `?mock=1` → dev mode
  - `?mock=0` → real mode
  - default → real mode

The dev-kiosk button always appends `?mock=1` so it works whether or not a `userToken` is set on the origin.

## Connecting to a real robot

The kiosk authenticates via an `access-token` parsed from the `userToken` cookie set by Viam's web app on its own origin. To connect to a real machine, serve the app from an origin where that cookie is set — typically by:

- **Deploying as a Viam module** (the production path).
- **Tunneling localhost through a domain that has the cookie** (e.g. for end-to-end testing against a real robot from your dev machine). The tunnel domain is what `window.location.hostname` reads, so the app behaves as a deployed instance on that origin.

Run the following and go to http://localhost:8012 to get the cookie. 
```bash
viam module local-app-testing --app-url http://localhost:3000
```

## Pose calibration view

`?view=calibrate&partId=<partId>` shows which poses belong to which frame on a
given machine. Reached from the fleet dashboard: each row links to
its own machine as `[poses →]`, shown only for machines the manifest covers. A
`partId` that isn't in the manifest says so rather than quietly showing a
different machine's poses.

It shows which poses belong to which frame on a given machine,
and which of them are set by hand versus derived from another pose. At the top
it polls where the machine believes `filter`, `grip-point`, and `cam` currently
are — the same reading as `viam robot part motion get-pose` — each with a copy
button. That is the loop: jog the arm, copy the live frame, paste it into one of
the poses listed for that frame.

The live readout needs the machine online and a `userToken` cookie; without one
it falls back to dev mode. The pose list itself comes from a generated manifest,
so regenerate it whenever a pose is added, removed, renamed, or re-baselined:

```bash
make web-app-manifest                          # refresh the machines already in it
make web-app-manifest PART_IDS="<id> <id>"     # add or change machines
```

`viam login` is the only setup. The page always warns that the list may be out
of date — it shows the capture time and how long ago that was, and escalates
past 30 days, but never claims the list is current, because nothing in the page
can detect a pose change.

## Other commands

```bash
npm run build        # static export → web-app/out/
npm run lint         # eslint
npm test             # unit tests (node:test)
```

From the repo root:

```bash
make web-app-dev          # dev server (npm run dev)
make web-app-local-test   # serve it through a Viam origin so the userToken cookie is set
make web-app-test         # unit tests
make web-app-install      # npm ci, after a dependency change
make web-app-module       # bundled Viam module (web app + Go launcher)
```

`make web-app-local-test` wraps the `viam module local-app-testing` command
above; run it alongside `make web-app-dev` and open
[http://localhost:8012](http://localhost:8012). The dev targets deliberately
skip `npm ci`, which would wipe `node_modules` on every invocation.
