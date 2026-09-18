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
  errorCount: number;
}

export interface RobotTotal {
  robotId: string;
  robotName: string;
  count: number;
  errorCount: number;
}

export interface DailyOrderCount {
  day: Date;
  rows: RobotDayRow[];
}

export interface OrderRecord {
  orderId: string;
  robotId: string;
  customerName: string;
  drink: string;
  startTime: Date;
  endTime: Date;
  durationMs: number;
  ok: boolean;
  // An operator stopping the run is not a fault, and the two read very
  // differently when scanning a day for things that went wrong.
  cancelled: boolean;
  failedStep: string;
  decaf: boolean;
  errorMessage: string;
}

export interface LeaderboardEntry {
  name: string;
  count: number;
}

export type Panel =
  | { kind: "today" }
  | { kind: "day"; day: Date; robotId: string; robotName: string }
  | { kind: "errors" };

export type SortKey = "time" | "customer" | "drink" | "duration" | "status";
export type SortDir = "asc" | "desc";

interface RawOrderRow {
  time_received: Date | string;
  robot_id?: string;
  data?: {
    readings?: {
      order_id?: string;
      customer_name?: string;
      drink?: string;
      start_time?: Date | string;
      end_time?: Date | string;
      duration_ms?: number;
      order_ok?: boolean;
      operator_cancelled?: boolean;
      failed_step?: string;
      decaf?: boolean;
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
      robotId: r.robot_id ?? "",
      customerName: x.customer_name ?? "",
      drink: x.drink ?? "",
      startTime: toDate(x.start_time),
      endTime: toDate(x.end_time),
      durationMs: x.duration_ms ?? 0,
      ok: x.order_ok ?? false,
      cancelled: x.operator_cancelled ?? false,
      failedStep: x.failed_step ?? "",
      decaf: x.decaf ?? false,
      errorMessage: x.error_message ?? "",
    };
  });
}

export function startOfToday(): Date {
  const d = new Date();
  d.setHours(0, 0, 0, 0);
  return d;
}

export function panelKey(p: Panel | null): string {
  if (!p) return "none";
  if (p.kind === "today") return "today";
  if (p.kind === "errors") return "errors";
  return `day-${p.day.getTime()}-${p.robotId}`;
}

export function panelTitle(p: Panel): string {
  switch (p.kind) {
    case "today":
      return "Today · all machines";
    case "errors":
      return "Errors · last 7 days";
    case "day":
      return `${p.robotName} · ${formatDay(p.day)}`;
  }
}

export function panelEmptyMsg(p: Panel): string {
  switch (p.kind) {
    case "today":
      return "No orders yet today.";
    case "errors":
      return "No errors in the last 7 days.";
    case "day":
      return "No orders for this day.";
  }
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
  const since = new Date(Date.now() - 7 * 24 * 60 * 60 * 1000);

  const results = await runMQL<{
    time: Date | string;
    robot_id: string;
    order_ok: boolean | null;
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
          order_ok: "$data.readings.order_ok",
        },
        value: { $sum: 1 },
      },
    },
    {
      $project: {
        _id: 0,
        time: "$_id.time",
        robot_id: "$_id.robot_id",
        order_ok: "$_id.order_ok",
        value: 1,
      },
    },
    { $sort: { time: -1 } },
  ]);

  type Tally = { count: number; errorCount: number };
  const byDay = new Map<number, Map<string, Tally>>();
  for (const row of results) {
    if (!nameById.has(row.robot_id)) continue;
    const t = row.time instanceof Date ? row.time : new Date(row.time);
    const key = t.getTime();
    let perRobot = byDay.get(key);
    if (!perRobot) {
      perRobot = new Map<string, Tally>();
      byDay.set(key, perRobot);
    }
    const tally = perRobot.get(row.robot_id) ?? { count: 0, errorCount: 0 };
    tally.count += row.value;
    if (row.order_ok === false) tally.errorCount += row.value;
    perRobot.set(row.robot_id, tally);
  }
  return [...byDay.entries()]
    .map(([ms, perRobot]) => ({
      day: new Date(ms),
      rows: [...perRobot.entries()]
        .map(([robotId, tally]) => ({
          robotId,
          robotName: nameById.get(robotId) ?? robotId,
          count: tally.count,
          errorCount: tally.errorCount,
        }))
        .sort((a, b) => a.robotName.localeCompare(b.robotName)),
    }))
    .sort((a, b) => b.day.getTime() - a.day.getTime());
}

export async function loadRobotTotalsLastNDays(
  client: VIAM.ViamClient,
  machines: Machine[],
  days: number
): Promise<RobotTotal[]> {
  const nameById = new Map(machines.map((m) => [m.id, m.name]));
  const since = new Date(Date.now() - days * 24 * 60 * 60 * 1000);

  const results = await runMQL<{
    robot_id: string;
    order_ok: boolean | null;
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
          robot_id: "$robot_id",
          order_ok: "$data.readings.order_ok",
        },
        value: { $sum: 1 },
      },
    },
    {
      $project: {
        _id: 0,
        robot_id: "$_id.robot_id",
        order_ok: "$_id.order_ok",
        value: 1,
      },
    },
  ]);

  type Tally = { count: number; errorCount: number };
  const byRobot = new Map<string, Tally>();
  for (const row of results) {
    if (!nameById.has(row.robot_id)) continue;
    const tally = byRobot.get(row.robot_id) ?? { count: 0, errorCount: 0 };
    tally.count += row.value;
    if (row.order_ok === false) tally.errorCount += row.value;
    byRobot.set(row.robot_id, tally);
  }
  return [...byRobot.entries()]
    .map(([robotId, tally]) => ({
      robotId,
      robotName: nameById.get(robotId) ?? robotId,
      count: tally.count,
      errorCount: tally.errorCount,
    }))
    .sort((a, b) => a.robotName.localeCompare(b.robotName));
}

/**
 * `groupId` is the MQL `$group._id` expression, not a bare field name, so a
 * caller can fold several fields into one bucket (see the customer leaderboard
 * in dashboard.tsx). A plain string must keep its `$` — without it MQL reads a
 * literal and buckets every order together, which looks like a working query
 * returning one enormous row, so the type enforces the prefix.
 */
export async function loadLeaderboard(
  client: VIAM.ViamClient,
  groupId: `$${string}` | Record<string, unknown>,
  extraMatch: Record<string, unknown> = {}
): Promise<LeaderboardEntry[]> {
  const since = new Date(Date.now() - 7 * 24 * 60 * 60 * 1000);
  const results = await runMQL<{ name: string | null; value: number }>(client, [
    {
      $match: {
        location_id: LOCATION_ID,
        component_name: RESOURCE_NAME,
        time_received: { $gte: since },
        ...extraMatch,
      },
    },
    {
      $group: {
        _id: groupId,
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

/** A null robotId spans every machine in the location. */
export async function loadOrdersForDay(
  client: VIAM.ViamClient,
  robotId: string | null,
  day: Date
): Promise<OrderRecord[]> {
  const dayEnd = new Date(day.getTime() + 24 * 60 * 60 * 1000);
  const results = await runMQL<RawOrderRow>(client, [
    {
      $match: {
        location_id: LOCATION_ID,
        component_name: RESOURCE_NAME,
        ...(robotId === null ? {} : { robot_id: robotId }),
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

export const VIDEO_MIME = "video/mp4";
export const PLAN_MIME = "application/json";

// An order's ID tags two very different payloads: its camera clips, and every
// motion-plan JSON coffee/motion.go's planRequestTagDir saves (one file per
// planned motion, dozens per order). Only the mime type separates them —
// without it the clip list fills with plan files that render as broken video
// players. The tag match is exact, so no capture-time window is needed.
export function buildOrderFileFilter(
  orderIds: string[],
  mimeTypes: string[]
): VIAM.dataApi.Filter {
  return new VIAM.dataApi.Filter({
    locationIds: [LOCATION_ID],
    mimeType: mimeTypes,
    tagsFilter: new VIAM.dataApi.TagsFilter({
      type: VIAM.dataApi.TagsFilterType.MATCH_BY_OR,
      tags: orderIds,
    }),
  });
}

export async function loadVideosForOrder(
  client: VIAM.ViamClient,
  orderId: string
): Promise<OrderVideo[]> {
  const result = await client.dataClient.binaryDataByFilter(
    buildOrderFileFilter([orderId], [VIDEO_MIME]),
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

export async function getBinarySignedUrl(
  client: VIAM.ViamClient,
  binaryDataId: string
): Promise<string> {
  return client.dataClient.createBinaryDataSignedURL(binaryDataId);
}

// One clip per camera per order, so a day's worth of orders fits well inside
// this page; the tag match is exact, so nothing extra is counted.
const VIDEO_COUNT_PAGE_SIZE = 500;

export async function countVideosForOrders(
  client: VIAM.ViamClient,
  orderIds: string[]
): Promise<Map<string, number>> {
  const counts = new Map<string, number>();
  for (const id of orderIds) counts.set(id, 0);
  if (orderIds.length === 0) return counts;

  const result = await client.dataClient.binaryDataByFilter(
    buildOrderFileFilter(orderIds, [VIDEO_MIME]),
    VIDEO_COUNT_PAGE_SIZE,
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

// --- Motion plan requests ---------------------------------------------------

/**
 * One saved motion-plan request/response pair. Everything here is read off the
 * tags and filename that coffee/motion.go writes — see planRequestTagDir.
 */
export interface PlanRequestFile {
  binaryDataId: string;
  /** Basename, e.g. "20260917_094831.007_move.json". */
  fileName: string;
  /** Full synced path, including the tag= directories the tags came from. */
  path: string;
  /** Brew step the plan was issued under, or "" for one issued outside a step. */
  step: string;
  /** Motion kind: move, pivot, circular, carry, or a door action. */
  motion: string;
  ok: boolean;
  /** Clock time from the filename stamp, e.g. "09:48:31.007". */
  at: string;
  /** 1-based position in the order's plan sequence. */
  seq: number;
}

// Tag prefixes planRequestTagDir writes. Renaming one in Go empties this panel
// with no compile error here, so they are named in that function's doc comment.
const STEP_TAG_PREFIX = "step_";
const MOTION_TAG_PREFIX = "motion_";
const PLANNING_SUCCESS_TAG = "planning_success";

// ponytail: single page, no cursor. An order runs tens of plans against this
// ceiling; if one ever exceeds it the tail is silently dropped — page with
// `result.last` if that becomes real.
const PLAN_PAGE_SIZE = 1000;

// savePlanRequestAndResponse names every file "20060102_150405.000_<label>.json".
const PLAN_STAMP = /^\d{8}_(\d{2})(\d{2})(\d{2})\.(\d{3})_/;

export function planTimeLabel(fileName: string): string {
  const m = PLAN_STAMP.exec(fileName);
  return m ? `${m[1]}:${m[2]}:${m[3]}.${m[4]}` : "";
}

/** "step_locking_portafilter" -> "Locking portafilter". */
export function unslugStep(tag: string): string {
  const words = tag.slice(STEP_TAG_PREFIX.length).replace(/_/g, " ");
  return words.charAt(0).toUpperCase() + words.slice(1);
}

export async function loadPlanRequestsForOrder(
  client: VIAM.ViamClient,
  orderId: string
): Promise<PlanRequestFile[]> {
  const result = await client.dataClient.binaryDataByFilter(
    buildOrderFileFilter([orderId], [PLAN_MIME]),
    PLAN_PAGE_SIZE,
    undefined,
    undefined,
    // Metadata only: the payloads are megabytes each and are only ever fetched
    // one at a time, on a download click.
    false,
    false,
    false
  );

  const files = result.data
    .filter((d) => d.metadata?.binaryDataId)
    .map((d) => {
      const tags = d.metadata?.captureMetadata?.tags ?? [];
      const stepTag = tags.find((t) => t.startsWith(STEP_TAG_PREFIX));
      const motionTag = tags.find((t) => t.startsWith(MOTION_TAG_PREFIX));
      const path = d.metadata!.fileName;
      const fileName = path.split("/").pop() ?? "";
      return {
        binaryDataId: d.metadata!.binaryDataId,
        fileName,
        path,
        step: stepTag ? unslugStep(stepTag) : "",
        motion: motionTag ? motionTag.slice(MOTION_TAG_PREFIX.length) : "",
        ok: tags.includes(PLANNING_SUCCESS_TAG),
        at: planTimeLabel(fileName),
        seq: 0,
      };
    });

  // The filename's zero-padded stamp sorts lexically into plan order, which
  // beats cloud capture timestamps: the module wrote it at save time, and it
  // survives however the data manager batches the sync.
  files.sort((a, b) => a.fileName.localeCompare(b.fileName));
  return files.map((f, i) => ({ ...f, seq: i + 1 }));
}
