import { useEffect, useRef, useState } from "react";
import { type ViamConnection } from "./viamClient";
import { createConnectionManager, withTimeout } from "./connectionManager";

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
    // One manager per hook instance, backing this hook's single partId. The
    // dial-timeout and teardown behavior lives in the manager so the kiosk and
    // the dashboard stay in sync.
    const manager = createConnectionManager();

    // setError fires only on the first-ever attempt (no previous client) so
    // transient reconnect failures don't stomp the UI.
    async function dial(reason: string) {
      if (cancelled || dialInFlight) return;
      dialInFlight = true;
      const hadPrevious = connRef.current !== null;
      try {
        console.log(`[app] dialing Viam (${reason})…`);
        const next = await manager.get(partId);
        if (cancelled) return;
        connRef.current = next;
        setConn(next);
        setConnected(true);
        setError(null);
        console.log("[app] connected to Viam");
      } catch (err) {
        if (cancelled) return;
        console.log(`[app] dial failed (${reason}):`, err);
        if (!hadPrevious) {
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
        if (navigator.onLine) {
          // Drop the zombie channel so the re-dial gets a fresh client rather
          // than the cached, heartbeat-failing one.
          manager.invalidate(partId);
          void dial("heartbeat failed");
        }
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
      manager.closeAll();
    };
  }, [partId]);

  return { conn, connected, error };
}
