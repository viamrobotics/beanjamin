import * as VIAM from "@viamrobotics/sdk";
import { BSON } from "bson";

export const ORG_ID = "e76d1b3b-0468-4efd-bb7f-fb1d2b352fcb";
export const LOCATION_ID = "oeq47g5p1m";
export const RESOURCE_NAME = "order-events";

export interface Machine {
  id: string;
  name: string;
  locationName: string;
  online: boolean;
  lastOnline: Date | null;
  mainPartId: string | null;
}

export interface RobotDayRow {
  robotId: string;
  robotName: string;
  count: number;
}

export interface DailyOrderCount {
  day: Date;
  rows: RobotDayRow[];
}

export interface OrderRecord {
  orderId: string;
  customerName: string;
  drink: string;
  startTime: Date;
  endTime: Date;
  durationMs: number;
  ok: boolean;
  errorMessage: string;
}

export interface LeaderboardEntry {
  name: string;
  count: number;
}

export type Panel =
  | { kind: "day"; day: Date; robotId: string; robotName: string }
  | { kind: "errors" };

export type SortKey = "time" | "customer" | "drink" | "duration" | "status";
export type SortDir = "asc" | "desc";

interface RawOrderRow {
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
}

export function browserTimezone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  } catch {
    return "UTC";
  }
}

export function formatDay(day: Date): string {
  return day.toLocaleDateString(undefined, {
    weekday: "short",
    month: "short",
    day: "numeric",
  });
}

async function runMQL<T>(
  client: VIAM.ViamClient,
  stages: Record<string, unknown>[]
): Promise<T[]> {
  const serialized = stages.map((s) => BSON.serialize(s));
  return (await client.dataClient.tabularDataByMQL(
    ORG_ID,
    serialized as unknown as Parameters<
      typeof client.dataClient.tabularDataByMQL
    >[1]
  )) as T[];
}

function parseOrderResults(rows: RawOrderRow[]): OrderRecord[] {
  const toDate = (v: Date | string | undefined): Date =>
    v instanceof Date ? v : v ? new Date(v) : new Date(0);
  return rows.map((r) => {
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

export function panelKey(p: Panel | null): string {
  if (!p) return "none";
  if (p.kind === "errors") return "errors";
  return `day-${p.day.getTime()}-${p.robotId}`;
}

export function panelTitle(p: Panel): string {
  return p.kind === "day"
    ? `${p.robotName} · ${formatDay(p.day)}`
    : "Errors · last 7 days";
}

export function panelEmptyMsg(p: Panel): string {
  return p.kind === "errors"
    ? "No errors in the last 7 days."
    : "No orders for this day.";
}

export async function listMachines(client: VIAM.ViamClient): Promise<Machine[]> {
  const summaries = await client.appClient.listMachineSummaries(
    ORG_ID,
    ["e6103e56-ad3a-42c6-ae5b-7cc9c310331d"],
    [LOCATION_ID]
  );
  const machines: Machine[] = [];
  for (const location of summaries) {
    for (const m of location.machineSummaries) {
      const mainPart =
        m.partSummaries.find((p) => p.isMainPart) ?? m.partSummaries[0];
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

export async function loadDailyOrderCounts(
  client: VIAM.ViamClient,
  machines: Machine[]
): Promise<DailyOrderCount[]> {
  const nameById = new Map(machines.map((m) => [m.id, m.name]));
  const tz = browserTimezone();
  const since = new Date(Date.now() - 14 * 24 * 60 * 60 * 1000);

  const results = await runMQL<{
    time: Date | string;
    robot_id: string;
    value: number;
  }>(client, [
    {
      $match: {
        location_id: LOCATION_ID,
        component_name: RESOURCE_NAME,
        time_received: { $gte: since },
      },
    },
    {
      $group: {
        _id: {
          time: {
            $dateTrunc: {
              date: "$time_received",
              unit: "day",
              binSize: 1,
              timezone: tz,
            },
          },
          robot_id: "$robot_id",
        },
        value: { $sum: 1 },
      },
    },
    {
      $project: {
        _id: 0,
        time: "$_id.time",
        robot_id: "$_id.robot_id",
        value: 1,
      },
    },
    { $sort: { time: -1 } },
  ]);

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

export async function loadLeaderboard(
  client: VIAM.ViamClient,
  groupByField: string
): Promise<LeaderboardEntry[]> {
  const since = new Date(Date.now() - 7 * 24 * 60 * 60 * 1000);
  const results = await runMQL<{ name: string | null; value: number }>(client, [
    {
      $match: {
        location_id: LOCATION_ID,
        component_name: RESOURCE_NAME,
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
  ]);

  return results
    .filter((r) => typeof r.name === "string" && r.name.trim() !== "")
    .map((r) => ({ name: r.name as string, count: r.value }));
}

export async function loadOrdersForDay(
  client: VIAM.ViamClient,
  robotId: string,
  day: Date
): Promise<OrderRecord[]> {
  const dayEnd = new Date(day.getTime() + 24 * 60 * 60 * 1000);
  const results = await runMQL<RawOrderRow>(client, [
    {
      $match: {
        location_id: LOCATION_ID,
        component_name: RESOURCE_NAME,
        robot_id: robotId,
        time_received: { $gte: day, $lt: dayEnd },
      },
    },
    { $sort: { time_received: 1 } },
  ]);
  return parseOrderResults(results);
}

export interface OrderVideo {
  binaryDataId: string;
  fileName: string;
  capturedAt: Date | null;
}

const VIDEO_TIME_BUFFER_MS = 5 * 60 * 1000;

interface OrderTimeBounds {
  orderId: string;
  startTime: Date;
  endTime: Date;
}

function buildVideoFilter(order: OrderTimeBounds): VIAM.dataApi.Filter {
  const start = new Date(order.startTime.getTime() - VIDEO_TIME_BUFFER_MS);
  const end = new Date(order.endTime.getTime() + VIDEO_TIME_BUFFER_MS);
  return {
    locationIds: [LOCATION_ID],
    tagsFilter: { tags: [order.orderId] },
    startTime: start,
    endTime: end,
  } as unknown as VIAM.dataApi.Filter;
}

export async function loadVideosForOrder(
  client: VIAM.ViamClient,
  order: OrderTimeBounds
): Promise<OrderVideo[]> {
  const result = await client.dataClient.binaryDataByFilter(
    buildVideoFilter(order),
    100,
    undefined,
    undefined,
    false,
    false,
    false
  );
  return result.data
    .filter((d) => d.metadata?.binaryDataId)
    .map((d) => ({
      binaryDataId: d.metadata!.binaryDataId,
      fileName: d.metadata!.fileName,
      capturedAt: d.metadata!.timeReceived?.toDate() ?? null,
    }));
}

export async function getVideoSignedUrl(
  client: VIAM.ViamClient,
  binaryDataId: string
): Promise<string> {
  return client.dataClient.createBinaryDataSignedURL(binaryDataId);
}

export async function countVideosForOrder(
  client: VIAM.ViamClient,
  order: OrderTimeBounds
): Promise<number> {
  const result = await client.dataClient.binaryDataByFilter(
    buildVideoFilter(order),
    undefined,
    undefined,
    undefined,
    false,
    true,
    false
  );
  return Number(result.count);
}

export async function countVideosForOrders(
  client: VIAM.ViamClient,
  orders: OrderTimeBounds[]
): Promise<Map<string, number>> {
  const counts = new Map<string, number>();
  for (const o of orders) counts.set(o.orderId, 0);
  if (orders.length === 0) return counts;

  let minStart = orders[0].startTime.getTime();
  let maxEnd = orders[0].endTime.getTime();
  for (const o of orders) {
    minStart = Math.min(minStart, o.startTime.getTime());
    maxEnd = Math.max(maxEnd, o.endTime.getTime());
  }
  const filter = {
    locationIds: [LOCATION_ID],
    tagsFilter: { tags: orders.map((o) => o.orderId) },
    startTime: new Date(minStart - VIDEO_TIME_BUFFER_MS),
    endTime: new Date(maxEnd + VIDEO_TIME_BUFFER_MS),
  } as unknown as VIAM.dataApi.Filter;

  const result = await client.dataClient.binaryDataByFilter(
    filter,
    100,
    undefined,
    undefined,
    false,
    false,
    false
  );
  for (const item of result.data) {
    const itemTags = item.metadata?.captureMetadata?.tags ?? [];
    for (const t of itemTags) {
      if (counts.has(t)) counts.set(t, (counts.get(t) ?? 0) + 1);
    }
  }
  return counts;
}

export async function loadErrorsLast7Days(
  client: VIAM.ViamClient
): Promise<OrderRecord[]> {
  const since = new Date(Date.now() - 7 * 24 * 60 * 60 * 1000);
  const results = await runMQL<RawOrderRow>(client, [
    {
      $match: {
        location_id: LOCATION_ID,
        component_name: RESOURCE_NAME,
        time_received: { $gte: since },
        "data.readings.order_ok": false,
      },
    },
    { $sort: { time_received: -1 } },
  ]);
  return parseOrderResults(results);
}
