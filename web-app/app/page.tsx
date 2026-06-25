"use client";

import { Suspense } from "react";
import { useSearchParams } from "next/navigation";
import { Dashboard } from "./dashboard";
import { Kiosk } from "./kiosk";

// Everything is served from the single static entrypoint (out/index.html), so
// the kiosk can't live at its own route — the Viam app host serves the export
// straight from object storage with no SPA fallback, and a refresh on a nested
// path (e.g. /machine) 404s. Instead we select the view from a query param,
// which survives refresh because the host always resolves "/" to the entrypoint.
function Root() {
  const view = useSearchParams().get("view");
  return view === "machine" ? <Kiosk /> : <Dashboard />;
}

export default function Page() {
  return (
    <Suspense fallback={null}>
      <Root />
    </Suspense>
  );
}
