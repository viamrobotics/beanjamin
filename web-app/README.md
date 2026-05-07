# Beanjamin Web App

Customer-facing kiosk UI for the Beanjamin espresso robot. Built with Next.js and communicates with the robot via the Viam TypeScript SDK.

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

Open [http://localhost:3000](http://localhost:3000). The app automatically runs in **dev/mock mode** on localhost — no real robot connection needed. Step through the full order flow with simulated queue progress.

To force mock mode on any URL, append `?mock=1`. To force real mode (e.g. to test against a live machine from a local server), append `?mock=0`.

## Connecting to a real robot

When served by `viam module local-app-testing` or deployed as a Viam module, the app is served at `/machine/<hostname>/` and reads Viam credentials from a cookie set by the platform. No additional configuration is needed.

## Other commands

```bash
npm run build        # production build (static export → web-app/out/)
npm run lint         # eslint
```

To build the bundled Viam module (web app + Go launcher), run from the repo root:

```bash
make web-app-module
```
