import { useEffect, useRef, useState } from "react";
import { connectToViam, type ViamConnection } from "./viamClient";

const HEARTBEAT_INTERVAL_MS = 3000;
// Shorter than the interval so a stuck ping on a zombie channel can't block
// the next one and is treated as a heartbeat failure.
const HEARTBEAT_TIMEOUT_MS = 2500;
// Upper bound on a single dial. Without this, a stuck SDK signaling dial
// would wedge recovery permanently.
const DIAL_TIMEOUT_MS = 10_000;

function withTimeout<T>(p: Promise<T>, ms: number, label: string): Promise<T> {
  return Promise.race([
    p,
    new Promise<T>((_, reject) =>
      setTimeout(() => reject(new Error(`${label} timed out after ${ms}ms`)), ms)
    ),
  ]);
}

export function useViamConnection() {
  const [conn, setConn] = useState<ViamConnection | null>(null);
  const [connected, setConnected] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const connRef = useRef<ViamConnection | null>(null);

  useEffect(() => {
    let cancelled = false;
    let dialInFlight = false;
    let pingInFlight = false;

    // Only path to a fresh client. Idempotent: concurrent callers share the
    // in-flight dial. setError fires only on the first-ever attempt (no
    // previous client) so transient reconnect failures don't stomp the UI.
    async function dial(reason: string) {
      if (cancelled || dialInFlight) return;
      dialInFlight = true;
      const previous = connRef.current;
      try {
        console.log(`[app] dialing Viam (${reason})…`);
        const next = await withTimeout(connectToViam(), DIAL_TIMEOUT_MS, "dial");
        if (cancelled) return;
        connRef.current = next;
        setConn(next);
        setConnected(true);
        setError(null);
        console.log("[app] connected to Viam");
        // Fire-and-forget teardown of the old zombie client. Awaiting would
        // let a stuck disconnect block future dials; the new client is
        // already live and publishing, so we don't need to wait.
        if (previous && !previous.isDev) {
          void previous.robotClient.disconnect().catch(() => {});
        }
      } catch (err) {
        if (cancelled) return;
        console.log(`[app] dial failed (${reason}):`, err);
        if (!previous) {
          setError(
            `Viam connection failed: ${err instanceof Error ? err.message : String(err)}`
          );
        }
      } finally {
        dialInFlight = false;
      }
    }

    async function ping() {
      if (cancelled || dialInFlight || pingInFlight) return;
      const current = connRef.current;
      if (!current) return;
      // Dev-mode mock returns `{} as RobotClient` which has no real methods.
      if (current.isDev) return;
      pingInFlight = true;
      try {
        await withTimeout(
          current.robotClient.resourceNames(),
          HEARTBEAT_TIMEOUT_MS,
          "heartbeat"
        );
        if (cancelled) return;
        setConnected(true);
      } catch (err) {
        if (cancelled) return;
        console.log("[app] heartbeat failed:", err);
        setConnected(false);
        if (navigator.onLine) void dial("heartbeat failed");
      } finally {
        pingInFlight = false;
      }
    }

    const handleOnline = () => {
      console.log("[app] browser online");
      if (connRef.current) void ping();
      else void dial("online event");
    };

    window.addEventListener("online", handleOnline);
    void dial("initial");
    const interval = setInterval(ping, HEARTBEAT_INTERVAL_MS);

    return () => {
      cancelled = true;
      clearInterval(interval);
      window.removeEventListener("online", handleOnline);
    };
  }, []);

  return { conn, connected, error };
}
