import { useEffect, useRef, useState } from "react";
import { connectToViam, type ViamConnection } from "./viamClient";
import {
  DIAL_TIMEOUT_MS,
  withTimeout,
  disconnectQuietly,
} from "./connectionManager";

const HEARTBEAT_INTERVAL_MS = 3000;
// Shorter than the interval so a stuck ping on a zombie channel can't block
// the next one and is treated as a heartbeat failure.
const HEARTBEAT_TIMEOUT_MS = 2500;

export function useViamConnection(partId: string) {
  const [conn, setConn] = useState<ViamConnection | null>(null);
  const [connected, setConnected] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const connRef = useRef<ViamConnection | null>(null);

  useEffect(() => {
    let cancelled = false;
    let dialInFlight = false;
    let pingInFlight = false;

    // Only path to a fresh client. Idempotent via dialInFlight: concurrent
    // callers don't stack dials. setError fires only on the first-ever attempt
    // (no previous client) so transient reconnect failures don't stomp the UI.
    async function dial(reason: string) {
      if (cancelled || dialInFlight) return;
      dialInFlight = true;
      const previous = connRef.current;
      try {
        console.log(`[app] dialing Viam (${reason})…`);
        const next = await withTimeout(
          connectToViam(partId),
          DIAL_TIMEOUT_MS,
          "dial"
        );
        if (cancelled) return;
        connRef.current = next;
        setConn(next);
        setConnected(true);
        setError(null);
        console.log("[app] connected to Viam");
        // Tear down the old zombie client now that the fresh one is live.
        if (previous) disconnectQuietly(previous);
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
        // Re-dial gets a fresh client; the old zombie is torn down once the
        // new one is live (see dial()).
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
      if (connRef.current) disconnectQuietly(connRef.current);
    };
  }, [partId]);

  return { conn, connected, error };
}
