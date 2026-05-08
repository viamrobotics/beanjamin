"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import * as VIAM from "@viamrobotics/sdk";
import Cookies from "js-cookie";
import { BSON } from 'bson';

const ORG_ID = "e76d1b3b-0468-4efd-bb7f-fb1d2b352fcb";

function browserTimezone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  } catch {
    return "UTC";
  }
}

interface Machine {
  id: string;
  name: string;
  locationName: string;
  online: boolean;
  lastOnline: Date | null;
  mainPartId: string | null;
}

async function listMachines(client: VIAM.ViamClient): Promise<Machine[]> {
  const summaries = await client.appClient.listMachineSummaries(ORG_ID,["e6103e56-ad3a-42c6-ae5b-7cc9c310331d"], ["oeq47g5p1m"]);
  const machines: Machine[] = [];
  for (const location of summaries) {
    for (const m of location.machineSummaries) {
      const mainPart = m.partSummaries.find((p) => p.isMainPart) ?? m.partSummaries[0];
      machines.push({
        id: m.machineId,
        name: m.machineName,
        locationName: location.locationName,
        online: mainPart?.onlineState === VIAM.appApi.OnlineState.ONLINE,
        lastOnline: mainPart?.lastOnline?.toDate() ?? null,
        mainPartId: mainPart?.partId ?? null,
      });
    }
  }
  machines.sort((a, b) => Number(b.online) - Number(a.online));
  return machines;
}

interface RobotDayRow {
  robotId: string;
  robotName: string;
  count: number;
}

interface DailyOrderCount {
  day: Date;
  rows: RobotDayRow[];
}

interface OrderRecord {
  orderId: string;
  customerName: string;
  drink: string;
  startTime: Date;
  endTime: Date;
  durationMs: number;
  ok: boolean;
  errorMessage: string;
}

async function loadDailyOrderCounts(
  client: VIAM.ViamClient,
  machines: Machine[]
): Promise<DailyOrderCount[]> {
  const nameById = new Map(machines.map((m) => [m.id, m.name]));
  const tz = browserTimezone();
  const rawMQLStages = [
    {
      "$match": {
        "location_id": "oeq47g5p1m",
        "component_name": "order-events"
      }
    },
    {
      "$group": {
        "_id": {
          "time": {
            "$dateTrunc": {
              "date": "$time_received",
              "unit": "day",
              "binSize": 1,
              "timezone": tz
            }
          },
          "robot_id": "$robot_id"
        },
        "value": {
          "$sum": 1
        }
      }
    },
    {
      "$project": {
        "_id": 0,
        "time": "$_id.time",
        "robot_id": "$_id.robot_id",
        "value": 1
      }
    },
    {
      "$sort": {
        "time": -1
      }
    }
  ];

  const serializedMQLStages = rawMQLStages.map(stage => BSON.serialize(stage));

  const results = (await client.dataClient.tabularDataByMQL(
    ORG_ID,
    serializedMQLStages
  )) as Array<{ time: Date | string; robot_id: string; value: number }>;

  const byDay = new Map<number, Map<string, number>>();
  for (const row of results) {
    if (!nameById.has(row.robot_id)) continue;
    const t = row.time instanceof Date ? row.time : new Date(row.time);
    const key = t.getTime();
    let perRobot = byDay.get(key);
    if (!perRobot) {
      perRobot = new Map<string, number>();
      byDay.set(key, perRobot);
    }
    perRobot.set(row.robot_id, (perRobot.get(row.robot_id) ?? 0) + row.value);
  }
  return [...byDay.entries()]
    .map(([ms, perRobot]) => ({
      day: new Date(ms),
      rows: [...perRobot.entries()]
        .map(([robotId, count]) => ({
          robotId,
          robotName: nameById.get(robotId) ?? robotId,
          count,
        }))
        .sort((a, b) => a.robotName.localeCompare(b.robotName)),
    }))
    .sort((a, b) => b.day.getTime() - a.day.getTime());
}

interface LeaderboardEntry {
  name: string;
  count: number;
}

async function loadLeaderboard(
  client: VIAM.ViamClient,
  groupByField: string
): Promise<LeaderboardEntry[]> {
  const since = new Date(Date.now() - 7 * 24 * 60 * 60 * 1000);
  const rawMQLStages = [
    {
      $match: {
        location_id: "oeq47g5p1m",
        component_name: "order-events",
        time_received: { $gte: since },
      },
    },
    {
      $group: {
        _id: `$${groupByField}`,
        value: { $sum: 1 },
      },
    },
    {
      $project: {
        _id: 0,
        name: "$_id",
        value: 1,
      },
    },
    { $sort: { value: -1 } },
  ];

  const serializedMQLStages = rawMQLStages.map((stage) => BSON.serialize(stage));

  const results = (await client.dataClient.tabularDataByMQL(
    ORG_ID,
    serializedMQLStages
  )) as Array<{ name: string | null; value: number }>;

  return results
    .filter((r) => typeof r.name === "string" && r.name.trim() !== "")
    .map((r) => ({ name: r.name as string, count: r.value }));
}

async function loadOrdersForDay(
  client: VIAM.ViamClient,
  robotId: string,
  day: Date
): Promise<OrderRecord[]> {
  const tz = browserTimezone();
  const rawMQLStages = [
    {
      $match: {
        location_id: "oeq47g5p1m",
        component_name: "order-events",
        robot_id: robotId,
        $expr: {
          $eq: [
            {
              $dateTrunc: {
                date: "$time_received",
                unit: "day",
                binSize: 1,
                timezone: tz,
              },
            },
            day,
          ],
        },
      },
    },
    { $sort: { time_received: 1 } },
  ];

  const serializedMQLStages = rawMQLStages.map((stage) => BSON.serialize(stage));

  const results = (await client.dataClient.tabularDataByMQL(
    ORG_ID,
    serializedMQLStages
  )) as Array<{
    time_received: Date | string;
    data?: {
      readings?: {
        order_id?: string;
        customer_name?: string;
        drink?: string;
        start_time?: Date | string;
        end_time?: Date | string;
        duration_ms?: number;
        order_ok?: boolean;
        error_message?: string;
      };
    };
  }>;

  const toDate = (v: Date | string | undefined): Date =>
    v instanceof Date ? v : v ? new Date(v) : new Date(0);

  return results.map((r) => {
    const x = r.data?.readings ?? {};
    return {
      orderId: x.order_id ?? "",
      customerName: x.customer_name ?? "",
      drink: x.drink ?? "",
      startTime: toDate(x.start_time),
      endTime: toDate(x.end_time),
      durationMs: x.duration_ms ?? 0,
      ok: x.order_ok ?? false,
      errorMessage: x.error_message ?? "",
    };
  });
}

function formatDay(day: Date): string {
  return day.toLocaleDateString(undefined, {
    weekday: "short",
    month: "short",
    day: "numeric",
  });
}

const ROBOT_COLORS = [
  "#93c5fd",
  "#fca5a5",
  "#86efac",
  "#fdba74",
  "#c4b5fd",
  "#a5f3fc",
  "#fde68a",
];

function OrdersChart({
  data,
  onBarClick,
  selected,
}: {
  data: DailyOrderCount[];
  onBarClick: (day: Date, row: RobotDayRow) => void;
  selected: { dayMs: number; robotId: string } | null;
}) {
  const days = [...data].slice(0, 7).reverse();
  const robotNames = [
    ...new Set(days.flatMap((d) => d.rows.map((r) => r.robotName))),
  ].sort();
  const colorByRobot = new Map(
    robotNames.map((name, i) => [name, ROBOT_COLORS[i % ROBOT_COLORS.length]])
  );
  const totalByRobot = new Map<string, number>();
  for (const d of days) {
    for (const r of d.rows) {
      totalByRobot.set(r.robotName, (totalByRobot.get(r.robotName) ?? 0) + r.count);
    }
  }

  const width = 800;
  const height = 280;
  const margin = { top: 20, right: 16, bottom: 44, left: 40 };
  const plotW = width - margin.left - margin.right;
  const plotH = height - margin.top - margin.bottom;

  const maxCount = Math.max(
    1,
    ...days.flatMap((d) => d.rows.map((r) => r.count))
  );
  const yMax = Math.ceil(maxCount * 1.1);
  const yTicks = 4;

  const groupW = plotW / Math.max(days.length, 1);
  const groupPad = groupW * 0.15;
  const innerW = groupW - groupPad * 2;
  const barW = innerW / Math.max(robotNames.length, 1);

  return (
    <div>
      <svg
        viewBox={`0 0 ${width} ${height}`}
        className="w-full max-w-200 h-auto"
        role="img"
        aria-label="Orders per robot per day"
      >
        {Array.from({ length: yTicks + 1 }, (_, i) => {
          const v = Math.round((yMax * i) / yTicks);
          const y = margin.top + plotH - (plotH * i) / yTicks;
          return (
            <g key={i}>
              <line
                x1={margin.left}
                x2={margin.left + plotW}
                y1={y}
                y2={y}
                stroke="#e5e7eb"
                strokeWidth={1}
              />
              <text
                x={margin.left - 6}
                y={y}
                textAnchor="end"
                dominantBaseline="middle"
                fontSize={11}
                fill="#6b7280"
              >
                {v}
              </text>
            </g>
          );
        })}

        {days.map((d, di) => {
          const groupX = margin.left + di * groupW + groupPad;
          return (
            <g key={d.day.getTime()}>
              {robotNames.map((name, ri) => {
                const row = d.rows.find((r) => r.robotName === name);
                const count = row?.count ?? 0;
                const h = yMax === 0 ? 0 : (plotH * count) / yMax;
                const x = groupX + ri * barW;
                const y = margin.top + plotH - h;
                const isSelected =
                  selected !== null &&
                  row !== undefined &&
                  selected.dayMs === d.day.getTime() &&
                  selected.robotId === row.robotId;
                return (
                  <g key={name}>
                    <rect
                      x={x + 1}
                      y={y}
                      width={Math.max(0, barW - 2)}
                      height={h}
                      fill={colorByRobot.get(name)}
                      stroke={isSelected ? "#374151" : "none"}
                      strokeWidth={isSelected ? 2 : 0}
                      rx={2}
                      className={row && count > 0 ? "cursor-pointer" : "cursor-default"}
                      onClick={() => {
                        if (row && count > 0) onBarClick(d.day, row);
                      }}
                    />
                    {count > 0 && (
                      <text
                        x={x + barW / 2}
                        y={y - 4}
                        textAnchor="middle"
                        fontSize={10}
                        fill="#374151"
                        className="pointer-events-none"
                      >
                        {count}
                      </text>
                    )}
                  </g>
                );
              })}
              <text
                x={groupX + innerW / 2}
                y={margin.top + plotH + 16}
                textAnchor="middle"
                fontSize={11}
                fill="#374151"
              >
                {formatDay(d.day)}
              </text>
            </g>
          );
        })}

        <line
          x1={margin.left}
          x2={margin.left + plotW}
          y1={margin.top + plotH}
          y2={margin.top + plotH}
          stroke="#9ca3af"
          strokeWidth={1}
        />
      </svg>

      <div className="text-sm mt-2">
        <div className="text-neutral-500 mb-1">Last 7 days</div>
        <div className="flex flex-wrap gap-3">
          {robotNames.map((name) => (
            <span key={name} className="inline-flex items-center gap-1.5">
              <span
                className="inline-block w-3 h-3 rounded-sm"
                style={{ background: colorByRobot.get(name) }}
              />
              {name}: <strong>{totalByRobot.get(name) ?? 0}</strong>
            </span>
          ))}
        </div>
      </div>
    </div>
  );
}

export default function Home() {
  const [status, setStatus] = useState<"loading" | "ready" | "error">("loading");
  const [error, setError] = useState<string | null>(null);
  const [machines, setMachines] = useState<Machine[]>([]);
  const [orderCounts, setOrderCounts] = useState<DailyOrderCount[] | null>(null);
  const [leaderboard, setLeaderboard] = useState<LeaderboardEntry[] | null>(null);
  const [drinkLeaderboard, setDrinkLeaderboard] = useState<
    LeaderboardEntry[] | null
  >(null);
  const [viamClient, setViamClient] = useState<VIAM.ViamClient | null>(null);
  const [selection, setSelection] = useState<{
    day: Date;
    robotId: string;
    robotName: string;
  } | null>(null);
  const [selectionOrders, setSelectionOrders] = useState<
    OrderRecord[] | null
  >(null);
  const [selectionError, setSelectionError] = useState<string | null>(null);
  const [selectionPage, setSelectionPage] = useState(0);
  const [selectionSort, setSelectionSort] = useState<{
    key: "time" | "customer" | "drink" | "duration" | "status";
    dir: "asc" | "desc";
  }>({ key: "time", dir: "desc" });
  const ORDERS_PER_PAGE = 5;
  const [isLocalhost, setIsLocalhost] = useState(false);

  useEffect(() => {
    setIsLocalhost(
      window.location.hostname === "localhost" ||
        window.location.hostname === "127.0.0.1"
    );

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
        setViamClient(client);

        const found = await listMachines(client);
        setMachines(found);
        setStatus("ready");

        loadDailyOrderCounts(client, found)
          .then(setOrderCounts)
          .catch((e) => {
            console.error("failed to load daily order counts:", e);
          });

        loadLeaderboard(client, "data.readings.customer_name")
          .then(setLeaderboard)
          .catch((e) => {
            console.error("failed to load customer leaderboard:", e);
          });

        loadLeaderboard(client, "data.readings.drink")
          .then(setDrinkLeaderboard)
          .catch((e) => {
            console.error("failed to load drink leaderboard:", e);
          });
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
        setStatus("error");
      }
    })();
  }, []);

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

  const ordersSection = (
    <section className="mb-8">
      <h2 className="text-xl font-semibold text-neutral-900 mb-3">Orders</h2>
      {orderCounts === null ? (
        <p className="text-neutral-500">Loading order counts…</p>
      ) : orderCounts.length === 0 ? (
        <p className="text-neutral-500">No orders yet.</p>
      ) : (
        <>
          <OrdersChart
            data={orderCounts}
            selected={
              selection
                ? { dayMs: selection.day.getTime(), robotId: selection.robotId }
                : null
            }
            onBarClick={(day, row) => {
              if (
                selection &&
                selection.day.getTime() === day.getTime() &&
                selection.robotId === row.robotId
              ) {
                setSelection(null);
                setSelectionOrders(null);
                setSelectionError(null);
                setSelectionPage(0);
                return;
              }
              setSelection({
                day,
                robotId: row.robotId,
                robotName: row.robotName,
              });
              setSelectionOrders(null);
              setSelectionError(null);
              setSelectionPage(0);
              setSelectionSort({ key: "time", dir: "desc" });
              if (!viamClient) return;
              loadOrdersForDay(viamClient, row.robotId, day)
                .then(setSelectionOrders)
                .catch((e) => {
                  console.error("failed to load orders for day:", e);
                  setSelectionError(
                    e instanceof Error ? e.message : String(e)
                  );
                });
            }}
          />
          {selection && (
            <div className="mt-4 p-4 border border-neutral-200 rounded-lg bg-neutral-50">
              <div className="flex justify-between items-center mb-3">
                <strong className="text-neutral-900">
                  {selection.robotName} · {formatDay(selection.day)}
                </strong>
                <button
                  onClick={() => {
                    setSelection(null);
                    setSelectionOrders(null);
                    setSelectionError(null);
                    setSelectionPage(0);
                  }}
                  className="border-none bg-transparent cursor-pointer text-neutral-500 hover:text-neutral-900 transition-colors text-lg leading-none"
                  aria-label="Close"
                >
                  ×
                </button>
              </div>
              {selectionError ? (
                <p className="text-red-500">Error: {selectionError}</p>
              ) : selectionOrders === null ? (
                <p className="text-neutral-500">Loading orders…</p>
              ) : selectionOrders.length === 0 ? (
                <p className="text-neutral-500">No orders for this day.</p>
              ) : (
                <>
                <table className="w-full border-collapse text-sm">
                  <thead>
                    <tr className="text-left text-neutral-500">
                      {(
                        [
                          { key: "time", label: "Time", defaultDir: "desc" },
                          { key: "customer", label: "Customer", defaultDir: "asc" },
                          { key: "drink", label: "Drink", defaultDir: "asc" },
                          { key: "duration", label: "Duration", defaultDir: "desc" },
                          { key: "status", label: "Status", defaultDir: "asc" },
                        ] as const
                      ).map((col) => {
                        const active = selectionSort.key === col.key;
                        const arrow = active
                          ? selectionSort.dir === "desc"
                            ? "↓"
                            : "↑"
                          : "";
                        return (
                          <th
                            key={col.key}
                            className="px-2 py-1 cursor-pointer select-none font-medium hover:text-neutral-900 transition-colors"
                            onClick={() => {
                              setSelectionSort((s) =>
                                s.key === col.key
                                  ? { key: col.key, dir: s.dir === "desc" ? "asc" : "desc" }
                                  : { key: col.key, dir: col.defaultDir }
                              );
                              setSelectionPage(0);
                            }}
                          >
                            {col.label} {arrow}
                          </th>
                        );
                      })}
                    </tr>
                  </thead>
                  <tbody>
                    {[...selectionOrders]
                      .sort((a, b) => {
                        const sign = selectionSort.dir === "desc" ? -1 : 1;
                        switch (selectionSort.key) {
                          case "time":
                            return (
                              sign *
                              (a.startTime.getTime() - b.startTime.getTime())
                            );
                          case "customer":
                            return sign * a.customerName.localeCompare(b.customerName);
                          case "drink":
                            return sign * a.drink.localeCompare(b.drink);
                          case "duration":
                            return sign * (a.durationMs - b.durationMs);
                          case "status":
                            return sign * (Number(a.ok) - Number(b.ok));
                        }
                      })
                      .slice(
                        selectionPage * ORDERS_PER_PAGE,
                        selectionPage * ORDERS_PER_PAGE + ORDERS_PER_PAGE
                      )
                      .map((o) => (
                      <tr
                        key={o.orderId || o.startTime.toISOString()}
                        className="border-t border-neutral-200"
                      >
                        <td className="px-2 py-1">
                          {o.startTime.toLocaleTimeString(undefined, {
                            hour: "numeric",
                            minute: "2-digit",
                          })}
                        </td>
                        <td className="px-2 py-1">
                          {o.customerName || "—"}
                        </td>
                        <td className="px-2 py-1">
                          {o.drink || "—"}
                        </td>
                        <td className="px-2 py-1">
                          {o.durationMs
                            ? `${(o.durationMs / 1000).toFixed(1)}s`
                            : "—"}
                        </td>
                        <td className="px-2 py-1">
                          {o.ok ? (
                            <span className="text-green-600">OK</span>
                          ) : (
                            <span
                              className="text-red-500"
                              title={o.errorMessage}
                            >
                              {o.errorMessage || "Failed"}
                            </span>
                          )}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
                {selectionOrders.length > ORDERS_PER_PAGE && (
                  <div className="flex items-center justify-between mt-3 text-sm">
                    <button
                      onClick={() => setSelectionPage((p) => Math.max(0, p - 1))}
                      disabled={selectionPage === 0}
                      className="px-3 py-1 rounded-md border border-neutral-200 bg-white text-neutral-900 hover:bg-neutral-100 transition-colors disabled:bg-neutral-50 disabled:text-neutral-400 disabled:cursor-not-allowed"
                    >
                      ← Prev
                    </button>
                    <span className="text-neutral-500">
                      Page {selectionPage + 1} of{" "}
                      {Math.ceil(selectionOrders.length / ORDERS_PER_PAGE)} ·{" "}
                      {selectionOrders.length} orders
                    </span>
                    <button
                      onClick={() =>
                        setSelectionPage((p) =>
                          Math.min(
                            Math.ceil(
                              selectionOrders.length / ORDERS_PER_PAGE
                            ) - 1,
                            p + 1
                          )
                        )
                      }
                      disabled={
                        selectionPage >=
                        Math.ceil(selectionOrders.length / ORDERS_PER_PAGE) - 1
                      }
                      className="px-3 py-1 rounded-md border border-neutral-200 bg-white text-neutral-900 hover:bg-neutral-100 transition-colors disabled:bg-neutral-50 disabled:text-neutral-400 disabled:cursor-not-allowed"
                    >
                      Next →
                    </button>
                  </div>
                )}
                </>
              )}
            </div>
          )}
        </>
      )}
    </section>
  );

  const rankEmoji = ["🥇", "🥈", "🥉", "☕", "☕"];
  const renderLeaderboardList = (
    entries: LeaderboardEntry[] | null,
    emptyMsg: string
  ) => {
    if (entries === null) {
      return <p className="text-neutral-500">Loading…</p>;
    }
    if (entries.length === 0) {
      return <p className="text-neutral-500">{emptyMsg}</p>;
    }
    return (
      <ul className="m-0 p-0 list-none">
        {entries.slice(0, 5).map((e, i) => (
          <li key={e.name} className="py-0.5">
            <span className="mr-2">{rankEmoji[i]}</span>
            {e.name} — <strong>{e.count}</strong>
          </li>
        ))}
      </ul>
    );
  };
  const leaderboardSection = (
    <section className="mb-8">
      <h2 className="text-xl font-semibold text-neutral-900 mb-3">
        🏆 Leaderboard · last 7 days
      </h2>
      <div className="grid grid-cols-2 gap-6 max-w-xl">
        <div>
          <div className="text-sm text-neutral-500 mb-2">Top customers</div>
          {renderLeaderboardList(leaderboard, "No orders yet.")}
        </div>
        <div>
          <div className="text-sm text-neutral-500 mb-2">Top drinks</div>
          {renderLeaderboardList(drinkLeaderboard, "No drinks yet.")}
        </div>
      </div>
    </section>
  );

  return (
    <div className="p-6 font-sans text-neutral-900">
      {devButton}
      <h1 className="text-2xl font-semibold mb-4">
        Machines ({machines.length})
      </h1>
      <ul className="m-0 p-0 list-none mb-8 space-y-2">
        {machines.map((m) => {
          const row = (
            <>
              <span
                title={m.online ? "Online" : "Offline"}
                className={`inline-block w-2 h-2 rounded-full mr-2 align-middle ${
                  m.online ? "bg-green-500" : "bg-neutral-400"
                }`}
              />
              <strong>{m.name}</strong> — {m.locationName}{" "}
              <span className="text-neutral-500">({m.id})</span>
              {m.lastOnline && (
                <span className="text-neutral-500 ml-2">
                  · last online {m.lastOnline.toLocaleString()}
                </span>
              )}
            </>
          );
          return (
            <li key={m.id}>
              {m.mainPartId ? (
                <>
                  <Link
                    href={`/machine?partId=${m.mainPartId}`}
                    className="text-neutral-900 no-underline hover:underline"
                  >
                    {row}
                  </Link>{" "}
                  <Link
                    href={`/machine?partId=${m.mainPartId}&kiosk=1`}
                    className="text-blue-600 ml-2 hover:underline"
                  >
                    [kiosk mode →]
                  </Link>
                </>
              ) : (
                row
              )}
            </li>
          );
        })}
      </ul>
      {ordersSection}
      {leaderboardSection}
    </div>
  );
}
