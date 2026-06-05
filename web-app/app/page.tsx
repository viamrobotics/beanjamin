"use client";

import { useEffect, useRef, useState } from "react";
import Link from "next/link";
import * as VIAM from "@viamrobotics/sdk";
import Cookies from "js-cookie";
import {
  getQueue,
  hasCoffeeService,
  type MachineQueueState,
  type ViamConnection,
} from "./lib/viamClient";
import { createConnectionManager } from "./lib/connectionManager";
import {
  type Machine,
  type DailyOrderCount,
  type OrderRecord,
  type LeaderboardEntry,
  type Panel,
  type RobotTotal,
  panelKey,
  listMachines,
  loadDailyOrderCounts,
  loadLeaderboard,
  loadOrdersForDay,
  loadErrorsLast7Days,
  loadRobotTotalsLastNDays,
} from "./home/data";
import { OrdersChart } from "./home/chart";
import { OrdersPanel } from "./home/orders-panel";
import { Leaderboard } from "./home/leaderboard";
import { MachineRow } from "./home/machine-row";

const PILL_BUTTON_BASE =
  "px-3 py-1.5 text-sm rounded-md border transition-colors";
const PILL_NEUTRAL = `${PILL_BUTTON_BASE} border-neutral-200 bg-white text-neutral-900 hover:bg-neutral-100`;
const PILL_DANGER_ACTIVE = `${PILL_BUTTON_BASE} border-red-500 bg-red-50 text-red-600 hover:bg-red-100`;

// A single transient getQueue RPC error shouldn't drop an otherwise-healthy
// WebRTC channel — only tear down and re-dial after this many in a row.
const MAX_QUEUE_FAILURES = 3;

export default function Home() {
  const [status, setStatus] = useState<"loading" | "ready" | "error">("loading");
  const [error, setError] = useState<string | null>(null);
  const [machines, setMachines] = useState<Machine[]>([]);
  const [orderCounts, setOrderCounts] = useState<DailyOrderCount[] | null>(null);
  const [totals14d, setTotals14d] = useState<RobotTotal[]>([]);
  const [customerLeaderboard, setCustomerLeaderboard] = useState<
    LeaderboardEntry[] | null
  >(null);
  const [drinkLeaderboard, setDrinkLeaderboard] = useState<
    LeaderboardEntry[] | null
  >(null);
  const [viamClient, setViamClient] = useState<VIAM.ViamClient | null>(null);
  const [machineQueues, setMachineQueues] = useState<
    Map<string, MachineQueueState>
  >(new Map());
  const [panel, setPanel] = useState<Panel | null>(null);
  const [panelOrders, setPanelOrders] = useState<OrderRecord[] | null>(null);
  const [panelError, setPanelError] = useState<string | null>(null);
  const [isLocalhost, setIsLocalhost] = useState(false);
  const refreshAllRef = useRef<() => void>(() => {});

  useEffect(() => {
    setIsLocalhost(
      window.location.hostname === "localhost" ||
        window.location.hostname === "127.0.0.1"
    );

    let cancelled = false;
    // One connection per machine (keyed by partId), with shared dial-timeout
    // and teardown behavior — same manager the kiosk uses.
    const connections = createConnectionManager();
    let queueInterval: ReturnType<typeof setInterval> | undefined;
    let aggregatesInterval: ReturnType<typeof setInterval> | undefined;
    let currentClient: VIAM.ViamClient | null = null;
    let currentMachines: Machine[] = [];

    // Consecutive getQueue failures per partId, so we only drop a connection
    // after MAX_QUEUE_FAILURES rather than on every transient RPC error.
    const queueFailures = new Map<string, number>();

    const setQueue = (machineId: string, state: MachineQueueState) => {
      if (cancelled) return;
      setMachineQueues((prev) => {
        const next = new Map(prev);
        next.set(machineId, state);
        return next;
      });
    };

    const refreshOneQueue = async (m: Machine) => {
      const partId = m.mainPartId;
      if (!partId) return;
      const machineId = m.id;

      let conn: ViamConnection;
      try {
        // The manager dials with a timeout and evicts on failure, so a stuck
        // dial can't wedge the pool and the next cycle re-dials cleanly.
        conn = await connections.get(partId);
      } catch (e) {
        console.error(`failed to connect to ${machineId}:`, e);
        queueFailures.delete(partId);
        setQueue(machineId, { kind: "error" });
        return;
      }

      try {
        // A machine can be online and reachable without running the coffee
        // service. That's not an error — keep the connection and mark it so
        // the dashboard can show a distinct (yellow) state.
        if (!(await hasCoffeeService(conn))) {
          queueFailures.delete(partId);
          setQueue(machineId, { kind: "no-service" });
          return;
        }
        const q = await getQueue(conn);
        queueFailures.delete(partId);
        setQueue(machineId, { kind: "ok", status: q });
      } catch (e) {
        const failures = (queueFailures.get(partId) ?? 0) + 1;
        queueFailures.set(partId, failures);
        console.error(
          `failed to fetch queue for ${machineId} (${failures}/${MAX_QUEUE_FAILURES}):`,
          e
        );
        if (failures >= MAX_QUEUE_FAILURES) {
          // Channel looks dead — tear it down so the next cycle re-dials.
          connections.invalidate(partId);
          queueFailures.delete(partId);
        }
        setQueue(machineId, { kind: "error" });
      }
    };

    let queueRefreshInFlight = false;
    const refreshQueues = async () => {
      // Skip if a prior cycle is still running so ticks can't pile up on a
      // slow machine; the interval will catch the next one.
      if (queueRefreshInFlight) return;
      queueRefreshInFlight = true;
      try {
        const targets = currentMachines.filter((m) => m.online && m.mainPartId);
        // Fan out across machines so one slow machine can't stretch the cycle.
        await Promise.allSettled(targets.map(refreshOneQueue));
      } finally {
        queueRefreshInFlight = false;
      }
    };

    const refreshAggregates = () => {
      if (!currentClient) return;
      loadDailyOrderCounts(currentClient, currentMachines)
        .then((d) => !cancelled && setOrderCounts(d))
        .catch((e) =>
          console.error("failed to load daily order counts:", e)
        );
      loadRobotTotalsLastNDays(currentClient, currentMachines, 14)
        .then((d) => !cancelled && setTotals14d(d))
        .catch((e) =>
          console.error("failed to load 14-day robot totals:", e)
        );
      loadLeaderboard(currentClient, "data.readings.customer_name")
        .then((d) => !cancelled && setCustomerLeaderboard(d))
        .catch((e) =>
          console.error("failed to load customer leaderboard:", e)
        );
      loadLeaderboard(currentClient, "data.readings.drink", {
        "data.readings.order_ok": true,
      })
        .then((d) => !cancelled && setDrinkLeaderboard(d))
        .catch((e) =>
          console.error("failed to load drink leaderboard:", e)
        );
    };

    refreshAllRef.current = () => {
      refreshQueues();
      refreshAggregates();
    };

    (async () => {
      try {
        const userTokenRaw = Cookies.get("userToken");
        if (!userTokenRaw) {
          throw new Error("No userToken cookie found");
        }
        const { access_token } = JSON.parse(userTokenRaw);

        const client = await VIAM.createViamClient({
          credentials: {
            type: "access-token",
            payload: access_token,
          },
        });
        if (cancelled) return;
        currentClient = client;
        setViamClient(client);

        const found = await listMachines(client);
        if (cancelled) return;
        currentMachines = found;
        setMachines(found);
        setStatus("ready");

        refreshAggregates();
        refreshQueues();

        queueInterval = setInterval(refreshQueues, 5000);
        aggregatesInterval = setInterval(refreshAggregates, 30000);
      } catch (e) {
        if (!cancelled) {
          setError(e instanceof Error ? e.message : String(e));
          setStatus("error");
        }
      }
    })();

    return () => {
      cancelled = true;
      if (queueInterval) clearInterval(queueInterval);
      if (aggregatesInterval) clearInterval(aggregatesInterval);
      connections.closeAll();
    };
  }, []);

  const closePanel = () => {
    setPanel(null);
    setPanelOrders(null);
    setPanelError(null);
  };

  const togglePanel = (
    next: Panel,
    loader: () => Promise<OrderRecord[]>
  ) => {
    if (panel && panelKey(panel) === panelKey(next)) {
      closePanel();
      return;
    }
    setPanel(next);
    setPanelOrders(null);
    setPanelError(null);
    loader()
      .then((orders) => setPanelOrders(orders))
      .catch((e) => {
        console.error("failed to load panel orders:", e);
        setPanelError(e instanceof Error ? e.message : String(e));
      });
  };

  const devButton = isLocalhost && (
    <Link
      href="/machine?mock=1"
      className="inline-block mb-4 px-4 py-2 bg-neutral-900 text-white rounded-md hover:bg-neutral-800 transition-colors no-underline"
    >
      Open dev kiosk →
    </Link>
  );

  if (status === "loading")
    return (
      <div className="p-6 font-sans text-neutral-900">
        {devButton}
        <p className="text-neutral-500">Loading machines…</p>
      </div>
    );
  if (status === "error")
    return (
      <div className="p-6 font-sans text-neutral-900">
        {devButton}
        <p className="text-red-500">Error: {error}</p>
      </div>
    );
  if (machines.length === 0)
    return (
      <div className="p-6 font-sans text-neutral-900">
        {devButton}
        <p className="text-neutral-500">No machines found.</p>
      </div>
    );

  return (
    <div className="p-6 font-sans text-neutral-900">
      {devButton}
      <div className="flex items-center justify-between mb-4">
        <h1 className="text-2xl font-semibold">
          Machines ({machines.length})
        </h1>
        <button
          onClick={() => refreshAllRef.current()}
          className={PILL_NEUTRAL}
        >
          ↻ Refresh
        </button>
      </div>
      <ul className="m-0 p-0 list-none mb-6 space-y-2">
        {[...machines]
          .sort((a, b) => {
            const score = (m: Machine) => {
              const q = machineQueues.get(m.id);
              if (
                q?.kind === "ok" &&
                q.status.is_busy &&
                q.status.orders.some((o) => o.completed_at === "")
              )
                return 2;
              if (m.online) return 1;
              return 0;
            };
            return score(b) - score(a);
          })
          .map((m) => (
            <MachineRow key={m.id} m={m} queue={machineQueues.get(m.id)} />
          ))}
      </ul>

      <Leaderboard
        customers={customerLeaderboard}
        drinks={drinkLeaderboard}
      />

      <section className="mb-6">
        <h2 className="text-xl font-semibold text-neutral-900 mb-3">Orders</h2>
        {orderCounts === null ? (
          <p className="text-neutral-500">Loading order counts…</p>
        ) : orderCounts.length === 0 ? (
          <p className="text-neutral-500">No orders yet.</p>
        ) : (
          <>
            <OrdersChart
              data={orderCounts}
              totals14d={totals14d}
              selected={
                panel?.kind === "day"
                  ? { dayMs: panel.day.getTime(), robotId: panel.robotId }
                  : null
              }
              onBarClick={(day, row) => {
                if (!viamClient) return;
                togglePanel(
                  {
                    kind: "day",
                    day,
                    robotId: row.robotId,
                    robotName: row.robotName,
                  },
                  () => loadOrdersForDay(viamClient, row.robotId, day)
                );
              }}
            />
            <div className="mt-3">
              <button
                onClick={() => {
                  if (!viamClient) return;
                  togglePanel({ kind: "errors" }, () =>
                    loadErrorsLast7Days(viamClient)
                  );
                }}
                className={
                  panel?.kind === "errors" ? PILL_DANGER_ACTIVE : PILL_NEUTRAL
                }
              >
                ⚠ Expand errors - last 7 days
              </button>
            </div>
            {panel && (
              <OrdersPanel
                key={panelKey(panel)}
                panel={panel}
                orders={panelOrders}
                error={panelError}
                onClose={closePanel}
                viamClient={viamClient}
              />
            )}
          </>
        )}
      </section>
    </div>
  );
}
