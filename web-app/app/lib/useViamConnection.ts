import { useEffect, useRef, useState } from "react";
import { connectToViam, type ViamConnection } from "./viamClient";

const HEARTBEAT_INTERVAL_MS = 3000;

export function useViamConnection() {
  const [conn, setConn] = useState<ViamConnection | null>(null);
  const [connected, setConnected] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const connRef = useRef<ViamConnection | null>(null);

  useEffect(() => {
    let cancelled = false;
    let heartbeatInterval: ReturnType<typeof setInterval> | null = null;
    let initInFlight = false;
    let lastHealthy: boolean | null = null;
    let reconnecting = false;

    async function init() {
      if (initInFlight || connRef.current) return;
      initInFlight = true;
      try {
        console.log("[app] connecting to Viam...");
        const next = await connectToViam();
        if (cancelled) return;
        connRef.current = next;
        setConn(next);
        setConnected(true);
        setError(null);
        console.log("[app] connected to Viam");
      } catch (err) {
        if (cancelled) return;
        console.error("Viam connection failed:", err);
        setError(
          `Viam connection failed: ${err instanceof Error ? err.message : String(err)}`
        );
      } finally {
        initInFlight = false;
      }
    }

    async function pingHealth() {
      if (reconnecting) return;
      const current = connRef.current;
      if (!current) return;
      // Dev-mode mock returns `{} as RobotClient` which has no real methods.
      if (current.isDev) return;
      try {
        await current.robotClient.resourceNames();
        if (cancelled) return;
        if (lastHealthy !== true) {
          console.log("[app] heartbeat ok");
          lastHealthy = true;
          setConnected(true);
        }
      } catch (err) {
        if (cancelled) return;
        if (lastHealthy !== false) {
          console.log("[app] heartbeat failed:", err);
          lastHealthy = false;
          setConnected(false);
        }
        if (!navigator.onLine) return;
        reconnecting = true;
        console.log("[app] attempting fresh reconnect…");
        try {
          const next = await connectToViam();
          if (cancelled) return;
          // Tear down the old zombie client before swapping in the new one —
          // the SDK usually handles a dead channel gracefully, but the
          // explicit disconnect avoids lingering peer connections.
          try {
            await current.robotClient.disconnect();
          } catch {
            /* old client is already broken; ignore */
          }
          connRef.current = next;
          setConn(next);
          lastHealthy = true;
          setConnected(true);
          console.log("[app] reconnected with fresh client");
        } catch (reconnectErr) {
          if (cancelled) return;
          console.log("[app] fresh reconnect failed:", reconnectErr);
        } finally {
          reconnecting = false;
        }
      }
    }

    // Recover from load-while-offline by retrying init() when the browser
    // comes back online; also kick an immediate heartbeat so recovery doesn't
    // wait up to 3s.
    const handleOnline = () => {
      console.log("[app] browser online");
      if (!connRef.current) {
        console.log("[app] retrying initial connect after online event");
        init();
      } else {
        void pingHealth();
      }
    };
    window.addEventListener("online", handleOnline);

    init();
    heartbeatInterval = setInterval(pingHealth, HEARTBEAT_INTERVAL_MS);

    return () => {
      cancelled = true;
      if (heartbeatInterval) clearInterval(heartbeatInterval);
      window.removeEventListener("online", handleOnline);
    };
  }, []);

  return { conn, connected, error };
}
