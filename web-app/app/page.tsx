"use client";

import { useEffect, useRef, useState } from "react";
import Link from "next/link";
import * as VIAM from "@viamrobotics/sdk";
import Cookies from "js-cookie";
import {
  connectToViam,
  getQueue,
  type QueueStatus,
  type ViamConnection,
} from "./lib/viamClient";
import {
  type Machine,
  type DailyOrderCount,
  type OrderRecord,
  type LeaderboardEntry,
  type Panel,
  panelKey,
  listMachines,
  loadDailyOrderCounts,
  loadLeaderboard,
  loadOrdersForDay,
  loadErrorsLast7Days,
} from "./home/data";
import { OrdersChart } from "./home/chart";
import { OrdersPanel } from "./home/orders-panel";
import { Leaderboard } from "./home/leaderboard";
import { MachineRow } from "./home/machine-row";

const PILL_BUTTON_BASE =
  "px-3 py-1.5 text-sm rounded-md border transition-colors";
const PILL_NEUTRAL = `${PILL_BUTTON_BASE} border-neutral-200 bg-white text-neutral-900 hover:bg-neutral-100`;
const PILL_DANGER_ACTIVE = `${PILL_BUTTON_BASE} border-red-500 bg-red-50 text-red-600 hover:bg-red-100`;

export default function Home() {
  const [status, setStatus] = useState<"loading" | "ready" | "error">("loading");
  const [error, setError] = useState<string | null>(null);
  const [machines, setMachines] = useState<Machine[]>([]);
  const [orderCounts, setOrderCounts] = useState<DailyOrderCount[] | null>(null);
  const [customerLeaderboard, setCustomerLeaderboard] = useState<
    LeaderboardEntry[] | null
  >(null);
  const [drinkLeaderboard, setDrinkLeaderboard] = useState<
    LeaderboardEntry[] | null
  >(null);
  const [viamClient, setViamClient] = useState<VIAM.ViamClient | null>(null);
  const [machineQueues, setMachineQueues] = useState<
    Map<string, QueueStatus | null>
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
    const connectionCache = new Map<string, Promise<ViamConnection>>();
    let queueInterval: ReturnType<typeof setInterval> | undefined;
    let aggregatesInterval: ReturnType<typeof setInterval> | undefined;
    let currentClient: VIAM.ViamClient | null = null;
    let currentMachines: Machine[] = [];

    const refreshQueues = async () => {
      for (const m of currentMachines) {
        if (!m.online || !m.mainPartId) continue;
        const partId = m.mainPartId;
        const machineId = m.id;
        let connPromise = connectionCache.get(partId);
        if (!connPromise) {
          connPromise = connectToViam(partId);
          connectionCache.set(partId, connPromise);
        }
        try {
          const conn = await connPromise;
          const q = await getQueue(conn);
          if (cancelled) return;
          setMachineQueues((prev) => {
            const next = new Map(prev);
            next.set(machineId, q);
            return next;
          });
        } catch (e) {
          console.error(`failed to fetch queue for ${machineId}:`, e);
          connectionCache.delete(partId);
          if (!cancelled) {
            setMachineQueues((prev) => {
              const next = new Map(prev);
              next.set(machineId, null);
              return next;
            });
          }
        }
      }
    };

    const refreshAggregates = () => {
      if (!currentClient) return;
      loadDailyOrderCounts(currentClient, currentMachines)
        .then((d) => !cancelled && setOrderCounts(d))
        .catch((e) =>
          console.error("failed to load daily order counts:", e)
        );
      loadLeaderboard(currentClient, "data.readings.customer_name")
        .then((d) => !cancelled && setCustomerLeaderboard(d))
        .catch((e) =>
          console.error("failed to load customer leaderboard:", e)
        );
      loadLeaderboard(currentClient, "data.readings.drink")
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
      <ul className="m-0 p-0 list-none mb-8 space-y-2">
        {machines.map((m) => (
          <MachineRow key={m.id} m={m} queue={machineQueues.get(m.id)} />
        ))}
      </ul>

      <Leaderboard
        customers={customerLeaderboard}
        drinks={drinkLeaderboard}
      />

      <section className="mb-8">
        <div className="flex items-center justify-between mb-3">
          <h2 className="text-xl font-semibold text-neutral-900">Orders</h2>
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
            ⚠ Errors · last 7 days
          </button>
        </div>
        {orderCounts === null ? (
          <p className="text-neutral-500">Loading order counts…</p>
        ) : orderCounts.length === 0 ? (
          <p className="text-neutral-500">No orders yet.</p>
        ) : (
          <>
            <OrdersChart
              data={orderCounts}
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
            {panel && (
              <OrdersPanel
                key={panelKey(panel)}
                panel={panel}
                orders={panelOrders}
                error={panelError}
                onClose={closePanel}
              />
            )}
          </>
        )}
      </section>
    </div>
  );
}
