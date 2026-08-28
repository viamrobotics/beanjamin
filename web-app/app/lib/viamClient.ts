// Types only — no runtime side effects
import type { ViamClient, RobotClient } from "@viamrobotics/sdk";
import type { PoseValue } from "./calibration";

export interface ViamConnection {
  viamClient: ViamClient;
  robotClient: RobotClient;
  machineId: string;
  hostname: string;
  isDev: boolean;
}

// --------------- Dev mode (localhost) ---------------

// Dev mode returns mock data so the app runs without a real robot.
//
// Detection rules:
//   - No `userToken` cookie → dev mode (no way to make a real connection).
//   - userToken present:
//       1. ?mock=1 query param → force dev mode
//       2. ?mock=0 query param → force real mode
//       3. otherwise → real mode
function isDevMode(): boolean {
  if (typeof window === "undefined") return false;

  if (!hasUserToken()) return true;

  const params = new URLSearchParams(window.location.search);
  const mockParam = params.get("mock");
  if (mockParam === "1" || mockParam === "true") return true;
  if (mockParam === "0" || mockParam === "false") return false;

  return false;
}

function hasUserToken(): boolean {
  if (typeof document === "undefined") return false;
  const prefix = "userToken=";
  for (const part of document.cookie.split(";")) {
    if (part.trim().startsWith(prefix)) return true;
  }
  return false;
}

// Simulated queue: each order takes DEV_ORDER_DURATION_MS to process
const DEV_ORDER_DURATION_MS = 15_000;
const DEV_STEPS = [
  "Grinding",
  "Tamping",
  "Locking portafilter",
  "Brewing",
  "Serving",
  "Cleaning", // exercises the "Ready to pick up" green card in dev mode
];

interface DevOrder {
  id: string;
  name: string;
}
// Mirrors the backend's recent buffer in dev mode so the frontend can poll
// a single shape regardless of which environment it's running in.
interface DevRecentOrder {
  id: string;
  name: string;
  rawStep: string;
  completedAt: number; // ms epoch
}
const DEV_RECENT_DISPLAY_MS = 15_000;
const devQueue: DevOrder[] = [];
const devRecent: DevRecentOrder[] = [];
let devOrderCounter = 0;
let devProcessing = false;
let devProcessingStartedAt = 0;

function startDevProcessing() {
  if (devProcessing) return;
  devProcessing = true;
  devProcessingStartedAt = Date.now();
  const tick = () => {
    if (devQueue.length === 0) {
      devProcessing = false;
      return;
    }
    setTimeout(() => {
      const finished = devQueue.shift();
      if (finished) {
        devRecent.push({
          id: finished.id,
          name: finished.name,
          rawStep: DEV_STEPS[DEV_STEPS.length - 1],
          completedAt: Date.now(),
        });
      }
      // Start timing the next order
      devProcessingStartedAt = Date.now();
      tick();
    }, DEV_ORDER_DURATION_MS);
  };
  tick();
}

function pruneDevRecent() {
  const cutoff = Date.now() - DEV_RECENT_DISPLAY_MS;
  while (devRecent.length > 0 && devRecent[0].completedAt < cutoff) {
    devRecent.shift();
  }
}

function getDevStep(): string {
  if (!devProcessing || devQueue.length === 0) return "";
  const elapsed = Date.now() - devProcessingStartedAt;
  const stepDuration = DEV_ORDER_DURATION_MS / DEV_STEPS.length;
  const stepIndex = Math.min(
    Math.floor(elapsed / stepDuration),
    DEV_STEPS.length - 1,
  );
  return DEV_STEPS[stepIndex];
}

// --------------- Lazy SDK loader ---------------

let sdkCache: {
  createViamClient: typeof import("@viamrobotics/sdk").createViamClient;
  GenericServiceClient: typeof import("@viamrobotics/sdk").GenericServiceClient;
  Cookies: typeof import("js-cookie").default;
} | null = null;

async function loadSDK() {
  if (sdkCache) return sdkCache;
  const [viamSdk, cookies] = await Promise.all([
    import("@viamrobotics/sdk"),
    import("js-cookie"),
  ]);
  sdkCache = {
    createViamClient: viamSdk.createViamClient,
    GenericServiceClient: viamSdk.GenericServiceClient,
    Cookies: cookies.default,
  };
  return sdkCache;
}

// --------------- Public API ---------------

const COFFEE_SERVICE_NAME = "coffee-lifecycle";
const CUSTOMER_DETECTOR_SERVICE_NAME = "customer-detector";

export async function connectToViam(partId: string): Promise<ViamConnection> {
  if (isDevMode()) {
    console.log("[dev] using mock Viam connection");
    return {
      viamClient: {} as ViamClient,
      robotClient: {} as RobotClient,
      machineId: "dev-machine",
      hostname: "localhost",
      isDev: true,
    };
  }

  if (!partId) {
    throw new Error("connectToViam: partId is required");
  }

  const sdk = await loadSDK();

  const raw = sdk.Cookies.get("userToken");
  if (!raw) {
    throw new Error('No "userToken" cookie found');
  }
  const { access_token } = JSON.parse(raw) as { access_token: string };

  const viamClient = await sdk.createViamClient({
    credentials: {
      type: "access-token",
      payload: access_token,
    },
  });

  const partResp = await viamClient.appClient.getRobotPart(partId);
  const part = partResp.part;
  if (!part) {
    throw new Error(`getRobotPart(${partId}) returned no part`);
  }
  const hostname = part.fqdn;
  const machineId = part.robot;

  const robotClient = await viamClient.connectToMachine({
    host: hostname,
  });

  return { viamClient, robotClient, machineId, hostname, isDev: false };
}

/** Read a key from the machine's user-defined metadata. */
export async function getMachineMetadataKey(
  conn: ViamConnection,
  key: string,
): Promise<string | undefined> {
  if (isDevMode()) return "dev-mock-key";

  const metadata = await conn.viamClient.appClient.getRobotMetadata(
    conn.machineId,
  );
  const value = metadata[key];
  return typeof value === "string" ? value : undefined;
}

/** Get the human-readable machine name from the Viam app. */
export async function getMachineName(conn: ViamConnection): Promise<string> {
  const robot = await conn.viamClient.appClient.getRobot(conn.machineId);
  return robot?.name ?? "";
}

export interface StepEntry {
  step: string;
  started_at: string;
}

export type Fulfillment = "pickup" | "delivery";

export interface QueueOrder {
  id: string;
  drink: string;
  customer_name: string;
  /**
   * How the customer receives the drink. Optional so dashboards can still
   * talk to machines running older module versions that don't send it;
   * treat absent as "pickup" (the backend default).
   */
  fulfillment?: Fulfillment;
  enqueued_at: string;
  raw_step: string;
  step_history: StepEntry[];
  /**
   * RFC3339 timestamp set by the backend when the espresso routine for this
   * order finished. Empty string means the order is still pending. The
   * backend keeps completed orders visible for ~15s after this timestamp,
   * then prunes them automatically.
   */
  completed_at: string;
}

export interface QueueStatus {
  count: number;
  orders: QueueOrder[];
  is_paused: boolean;
  is_busy: boolean;
  current_step: string;
}

/**
 * Per-machine dashboard state. Absent from the map (`undefined`) means the
 * first poll hasn't completed yet.
 *   - ok:         connected and the coffee-lifecycle service answered.
 *   - no-service: connected, but this machine isn't running coffee-lifecycle.
 *   - error:      the connection or the queue RPC failed.
 */
export type MachineQueueState =
  | { kind: "ok"; status: QueueStatus }
  | { kind: "no-service" }
  | { kind: "error" };

/**
 * Whether the coffee-lifecycle service is present on the machine. A machine can
 * be online and reachable without running it, in which case getQueue would
 * fail spuriously — the dashboard checks this first so a missing service shows
 * as a distinct state rather than a connection error.
 */
export async function hasCoffeeService(conn: ViamConnection): Promise<boolean> {
  if (isDevMode()) return true;
  const names = await conn.robotClient.resourceNames();
  // The coffee service is a generic service (type "service", subtype
  // "generic"). Match all three so a component or sensor that happens to share
  // the name can't be mistaken for the service.
  return names.some(
    (n) =>
      n.name === COFFEE_SERVICE_NAME &&
      n.type === "service" &&
      n.subtype === "generic",
  );
}

export async function getQueue(conn: ViamConnection): Promise<QueueStatus> {
  if (isDevMode()) {
    pruneDevRecent();
    const devStep = getDevStep();
    // Recent first (most-recent-first), then pending — same shape as the
    // real backend's List() / Status() output.
    const recentOrders: QueueOrder[] = [...devRecent].reverse().map((r) => ({
      id: r.id,
      drink: "espresso",
      customer_name: r.name,
      fulfillment: "pickup" as const,
      enqueued_at: new Date(r.completedAt).toISOString(),
      raw_step: r.rawStep,
      step_history: [],
      completed_at: new Date(r.completedAt).toISOString(),
    }));
    const pendingOrders: QueueOrder[] = devQueue.map((o, i) => ({
      id: o.id,
      drink: "espresso",
      customer_name: o.name,
      fulfillment: "pickup" as const,
      enqueued_at: new Date().toISOString(),
      raw_step: i === 0 ? devStep : "",
      step_history:
        i === 0 && devStep
          ? [{ step: devStep, started_at: new Date().toISOString() }]
          : [],
      completed_at: "",
    }));
    return {
      count: devQueue.length,
      orders: [...recentOrders, ...pendingOrders],
      is_paused: false,
      is_busy: devQueue.length > 0,
      current_step: devStep,
    };
  }

  const sdk = await loadSDK();
  const coffeeService = new sdk.GenericServiceClient(
    conn.robotClient,
    COFFEE_SERVICE_NAME,
  );
  const result = await coffeeService.getStatus();
  return result as unknown as QueueStatus;
}

export async function prepareOrder(
  conn: ViamConnection,
  opts: {
    drink: string;
    drinkLabel: string;
    customerName: string;
    customerEmail?: string;
    pronunciation?: string;
    /** Defaults to "pickup" on the backend when omitted. */
    fulfillment?: Fulfillment;
  },
): Promise<{ status: string; queue_position?: number; order_id?: string }> {
  if (isDevMode()) {
    if (opts.customerName) {
      const id = `dev-${++devOrderCounter}`;
      devQueue.push({ id, name: opts.customerName });
      startDevProcessing();
      console.log(
        "[dev] order queued:",
        opts.customerName,
        "id:",
        id,
        "queue:",
        devQueue.map((o) => o.name),
      );
    }
    return {
      status: "queued",
      order_id: `dev-${devOrderCounter}`,
      queue_position: devQueue.length,
    };
  }

  const sdk = await loadSDK();
  const coffeeService = new sdk.GenericServiceClient(
    conn.robotClient,
    COFFEE_SERVICE_NAME,
  );

  const greeting = opts.pronunciation
    ? `One ${opts.drinkLabel} coming right up!`
    : undefined;

  const result = await coffeeService.doCommand({
    prepare_order: {
      drink: opts.drink,
      customer_name: opts.customerName,
      ...(opts.customerEmail && { customer_email: opts.customerEmail }),
      ...(greeting && { initial_greeting: greeting }),
      ...(opts.fulfillment && { fulfillment: opts.fulfillment }),
    },
  });
  console.log("[viamClient] prepareOrder result:", result);
  return result as unknown as { status: string };
}

// --- Customer Detector ---

export async function registerCustomerFace(
  conn: ViamConnection,
  name: string,
  email: string,
): Promise<{ registered: string; name: string; image_path: string }> {
  const sdk = await loadSDK();
  const svc = new sdk.GenericServiceClient(
    conn.robotClient,
    CUSTOMER_DETECTOR_SERVICE_NAME,
  );
  const result = await svc.doCommand({
    register_customer: { name, email },
  });
  return result as unknown as {
    registered: string;
    name: string;
    image_path: string;
  };
}

export async function finishRegistration(
  conn: ViamConnection,
  email: string,
): Promise<{ email: string; name: string; face_images: number }> {
  const sdk = await loadSDK();
  const svc = new sdk.GenericServiceClient(
    conn.robotClient,
    CUSTOMER_DETECTOR_SERVICE_NAME,
  );
  const result = await svc.doCommand({ finish_registration: email });
  return result as unknown as {
    email: string;
    name: string;
    face_images: number;
  };
}

export async function identifyCustomer(conn: ViamConnection): Promise<{
  identified: boolean;
  name?: string;
  email?: string;
  confidence?: number;
  message?: string;
}> {
  if (isDevMode()) {
    return { identified: false, message: "dev mode" };
  }

  const sdk = await loadSDK();
  const svc = new sdk.GenericServiceClient(
    conn.robotClient,
    CUSTOMER_DETECTOR_SERVICE_NAME,
  );
  const result = await svc.doCommand({ identify_customer: true });
  return result as unknown as {
    identified: boolean;
    name?: string;
    email?: string;
    confidence?: number;
    message?: string;
  };
}

export async function getCustomerDetectorInfo(
  conn: ViamConnection,
): Promise<{ camera_name: string }> {
  if (isDevMode()) {
    // No customer-detector in dev; let the caller's catch path skip face
    // registration rather than stranding it on an empty camera stream.
    throw new Error("dev mode: customer detector unavailable");
  }

  const sdk = await loadSDK();
  const svc = new sdk.GenericServiceClient(
    conn.robotClient,
    CUSTOMER_DETECTOR_SERVICE_NAME,
  );
  const result = await svc.doCommand({ get_info: true });
  return result as unknown as { camera_name: string };
}

// --- Live frame poses ---

/**
 * Where the machine currently believes the named frame is, in world
 * coordinates — the same reading as `viam robot part motion get-pose`. The
 * calibration view polls this so an operator can jog the arm and copy the
 * result straight into a pose, instead of transcribing it from the CLI.
 */
export async function getFramePose(
  conn: ViamConnection,
  frame: string,
): Promise<PoseValue> {
  if (isDevMode()) return devFramePose(frame);

  const pif = await conn.robotClient.getPose(frame, "world", []);
  const p = pif.pose;
  if (!p) throw new Error(`no pose returned for ${frame}`);
  // Rounded at the source so the page can serialize once and use that single
  // string for both the on-screen preview and the clipboard — a reading shown
  // at one precision and copied at another is the bug this view exists to stop.
  return round({
    x: p.x,
    y: p.y,
    z: p.z,
    o_x: p.oX,
    o_y: p.oY,
    o_z: p.oZ,
    theta: p.theta,
  });
}

const POSE_DECIMALS = 4;

function round(pose: PoseValue): PoseValue {
  return Object.fromEntries(
    Object.entries(pose).map(([k, v]) => [k, Number(v.toFixed(POSE_DECIMALS))]),
  ) as unknown as PoseValue;
}

// A slowly drifting pose, so dev mode shows the reading is live rather than a
// frozen constant.
function devFramePose(frame: string): PoseValue {
  const seed = [...frame].reduce((n, c) => n + c.charCodeAt(0), 0);
  const drift = Math.sin(Date.now() / 2000 + seed) * 5;
  return round({
    x: 400 + seed / 10 + drift,
    y: -120 + drift * 2,
    z: 210 + drift,
    o_x: 0.67,
    o_y: -0.74,
    o_z: 0.03,
    theta: -180,
  });
}
