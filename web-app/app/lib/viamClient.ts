// Types only — no runtime side effects
import type { ViamClient, RobotClient } from "@viamrobotics/sdk";

export interface ViamConnection {
  viamClient: ViamClient;
  robotClient: RobotClient;
  machineId: string;
  hostname: string;
}

// --------------- Dev mode (localhost) ---------------

// Dev mode returns mock data so the app runs without a real robot.
//
// Detection rules, in priority order:
//   1. ?mock=1 query param → force dev mode
//   2. ?mock=0 query param → force real mode
//   3. URL path starts with /machine/<hostname>/ → real mode (this is what
//      `viam module local-app-testing` serves, even on localhost)
//   4. hostname is localhost or 127.0.0.1 → dev mode
//   5. otherwise → real mode
function isDevMode(): boolean {
  if (typeof window === "undefined") return false;

  const params = new URLSearchParams(window.location.search);
  const mockParam = params.get("mock");
  if (mockParam === "1" || mockParam === "true") return true;
  if (mockParam === "0" || mockParam === "false") return false;

  if (window.location.pathname.startsWith("/machine/")) return false;

  return (
    window.location.hostname === "localhost" ||
    window.location.hostname === "127.0.0.1"
  );
}

// Simulated queue: each order takes DEV_ORDER_DURATION_MS to process
const DEV_ORDER_DURATION_MS = 15_000;
const DEV_STEPS = ["Grinding", "Tamping", "Locking portafilter", "Brewing", "Serving"];

interface DevOrder {
  id: string;
  name: string;
}
const devQueue: DevOrder[] = [];
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
      devQueue.shift();
      // Start timing the next order
      devProcessingStartedAt = Date.now();
      tick();
    }, DEV_ORDER_DURATION_MS);
  };
  tick();
}

function getDevStep(): string {
  if (!devProcessing || devQueue.length === 0) return "";
  const elapsed = Date.now() - devProcessingStartedAt;
  const stepDuration = DEV_ORDER_DURATION_MS / DEV_STEPS.length;
  const stepIndex = Math.min(
    Math.floor(elapsed / stepDuration),
    DEV_STEPS.length - 1
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

export async function connectToViam(): Promise<ViamConnection> {
  if (isDevMode()) {
    console.log("[dev] using mock Viam connection");
    return {
      viamClient: {} as ViamClient,
      robotClient: {} as RobotClient,
      machineId: "dev-machine",
      hostname: "localhost",
    };
  }

  const sdk = await loadSDK();

  const machineCookieKey = window.location.pathname.split("/")[2];
  if (!machineCookieKey) {
    throw new Error(
      "No machine hostname found in URL path. Expected /machine/<hostname>/"
    );
  }

  const raw = sdk.Cookies.get(machineCookieKey);
  if (!raw) {
    throw new Error(
      `No Viam credentials cookie found for "${machineCookieKey}"`
    );
  }

  const { apiKey, machineId, hostname } = JSON.parse(raw) as {
    apiKey: { id: string; key: string };
    machineId: string;
    hostname: string;
  };

  const viamClient = await sdk.createViamClient({
    credentials: {
      type: "api-key",
      authEntity: apiKey.id,
      payload: apiKey.key,
    },
  });

  const robotClient = await viamClient.connectToMachine({
    host: hostname,
  });

  return { viamClient, robotClient, machineId, hostname };
}

/** Read a key from the machine's user-defined metadata. */
export async function getMachineMetadataKey(
  conn: ViamConnection,
  key: string
): Promise<string | undefined> {
  if (isDevMode()) return "dev-mock-key";

  const metadata = await conn.viamClient.appClient.getRobotMetadata(
    conn.machineId
  );
  const value = metadata[key];
  return typeof value === "string" ? value : undefined;
}

export interface QueueOrder {
  id: string;
  drink: string;
  customer_name: string;
  enqueued_at: string;
}

export interface QueueStatus {
  count: number;
  orders: QueueOrder[];
  is_paused: boolean;
  is_busy: boolean;
  current_step: string;
}

export async function getQueue(conn: ViamConnection): Promise<QueueStatus> {
  if (isDevMode()) {
    return {
      count: devQueue.length,
      orders: devQueue.map((o) => ({
        id: o.id,
        drink: "espresso",
        customer_name: o.name,
        enqueued_at: new Date().toISOString(),
      })),
      is_paused: false,
      is_busy: devQueue.length > 0,
      current_step: getDevStep(),
    };
  }

  const sdk = await loadSDK();
  const coffeeService = new sdk.GenericServiceClient(
    conn.robotClient,
    COFFEE_SERVICE_NAME
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
    pronunciation?: string;
  }
): Promise<{ status: string; queue_position?: number; order_id?: string }> {
  if (isDevMode()) {
    if (opts.customerName) {
      const id = `dev-${++devOrderCounter}`;
      devQueue.push({ id, name: opts.customerName });
      startDevProcessing();
      console.log("[dev] order queued:", opts.customerName, "id:", id, "queue:", devQueue.map((o) => o.name));
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
    COFFEE_SERVICE_NAME
  );

  const greeting = opts.pronunciation
    ? `One ${opts.drinkLabel} coming right up!`
    : undefined;

  const result = await coffeeService.doCommand({
    prepare_order: {
      drink: opts.drink,
      customer_name: opts.customerName,
      ...(greeting && { initial_greeting: greeting }),
    },
  });
  console.log("[viamClient] prepareOrder result:", result);
  return result as unknown as { status: string };
}

// --- Customer Detector ---

export async function registerCustomerFace(
  conn: ViamConnection,
  name: string,
  email: string
): Promise<{ registered: string; name: string; image_path: string }> {
  const sdk = await loadSDK();
  const svc = new sdk.GenericServiceClient(
    conn.robotClient,
    CUSTOMER_DETECTOR_SERVICE_NAME
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
  email: string
): Promise<{ email: string; name: string; face_images: number }> {
  const sdk = await loadSDK();
  const svc = new sdk.GenericServiceClient(
    conn.robotClient,
    CUSTOMER_DETECTOR_SERVICE_NAME
  );
  const result = await svc.doCommand({ finish_registration: email });
  return result as unknown as {
    email: string;
    name: string;
    face_images: number;
  };
}

export async function identifyCustomer(
  conn: ViamConnection
): Promise<{
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
    CUSTOMER_DETECTOR_SERVICE_NAME
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
  conn: ViamConnection
): Promise<{ camera_name: string }> {
  const sdk = await loadSDK();
  const svc = new sdk.GenericServiceClient(
    conn.robotClient,
    CUSTOMER_DETECTOR_SERVICE_NAME
  );
  const result = await svc.doCommand({ get_info: true });
  return result as unknown as { camera_name: string };
}
