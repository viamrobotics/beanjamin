import Cookies from "js-cookie";
import {
  createViamClient,
  GenericServiceClient,
  type ViamClient,
  type RobotClient,
} from "@viamrobotics/sdk";
import { Struct } from "@bufbuild/protobuf";

export interface ViamConnection {
  viamClient: ViamClient;
  robotClient: RobotClient;
  machineId: string;
  hostname: string;
}

/**
 * Read Viam credentials from the cookie injected by the Viam app framework.
 * The cookie key is the machine hostname from the URL path: /machine/<hostname>/
 */
export async function connectToViam(): Promise<ViamConnection> {
  const machineCookieKey = window.location.pathname.split("/")[2];
  if (!machineCookieKey) {
    throw new Error(
      "No machine hostname found in URL path. Expected /machine/<hostname>/"
    );
  }

  const raw = Cookies.get(machineCookieKey);
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

  const viamClient = await createViamClient({
    credentials: {
      type: "api-key",
      authEntity: apiKey.id,
      payload: apiKey.key,
    },
  });

  // Connect to the actual machine for DoCommand calls
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
  const metadata = await conn.viamClient.appClient.getRobotMetadata(
    conn.machineId
  );
  const value = metadata[key];
  return typeof value === "string" ? value : undefined;
}

/** Get the human-readable machine name from the Viam app. */
export async function getMachineName(
  conn: ViamConnection
): Promise<string> {
  const robot = await conn.viamClient.appClient.getRobot(conn.machineId);
  return robot?.name ?? "";
}

const COFFEE_SERVICE_NAME = "coffee-lifecycle";
const CUSTOMER_DETECTOR_SERVICE_NAME = "customer-detector";

/**
 * Send prepare_order DoCommand to the beanjamin coffee service.
 * The Go module handles speech announcements and robot control.
 */
/**
 * Fetch the current order queue from the coffee service.
 */
export async function getQueue(
  conn: ViamConnection
): Promise<{ count: number; orders: string[]; is_paused: boolean }> {
  const coffeeService = new GenericServiceClient(
    conn.robotClient,
    COFFEE_SERVICE_NAME
  );
  const command = Struct.fromJson({ get_queue: {} });
  const result = await coffeeService.doCommand(command);
  return result as unknown as {
    count: number;
    orders: string[];
    is_paused: boolean;
  };
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
  const coffeeService = new GenericServiceClient(
    conn.robotClient,
    COFFEE_SERVICE_NAME
  );

  const greeting = opts.pronunciation
    ? `One ${opts.drinkLabel} coming right up!`
    : undefined;

  const command = Struct.fromJson({
    prepare_order: {
      drink: opts.drink,
      customer_name: opts.customerName,
      ...(greeting && { initial_greeting: greeting }),
    },
  });

  const result = await coffeeService.doCommand(command);
  return result as unknown as { status: string };
}

// --- Customer Detector ---

/** Capture a single face photo and register it for a customer. */
export async function registerCustomerFace(
  conn: ViamConnection,
  name: string,
  email: string
): Promise<{ registered: string; name: string; image_path: string }> {
  const svc = new GenericServiceClient(
    conn.robotClient,
    CUSTOMER_DETECTOR_SERVICE_NAME
  );
  const command = Struct.fromJson({
    register_customer: { name, email },
  });
  const result = await svc.doCommand(command);
  return result as unknown as {
    registered: string;
    name: string;
    image_path: string;
  };
}

/** Signal that face capture is complete and trigger embedding recomputation. */
export async function finishRegistration(
  conn: ViamConnection,
  email: string
): Promise<{ email: string; name: string; face_images: number }> {
  const svc = new GenericServiceClient(
    conn.robotClient,
    CUSTOMER_DETECTOR_SERVICE_NAME
  );
  const command = Struct.fromJson({ finish_registration: email });
  const result = await svc.doCommand(command);
  return result as unknown as {
    email: string;
    name: string;
    face_images: number;
  };
}

/** Try to identify a customer from the camera feed. */
export async function identifyCustomer(
  conn: ViamConnection
): Promise<{
  identified: boolean;
  name?: string;
  email?: string;
  confidence?: number;
  message?: string;
}> {
  const svc = new GenericServiceClient(
    conn.robotClient,
    CUSTOMER_DETECTOR_SERVICE_NAME
  );
  const command = Struct.fromJson({ identify_customer: true });
  const result = await svc.doCommand(command);
  return result as unknown as {
    identified: boolean;
    name?: string;
    email?: string;
    confidence?: number;
    message?: string;
  };
}

/** Get the camera name configured on the customer-detector service. */
export async function getCustomerDetectorInfo(
  conn: ViamConnection
): Promise<{ camera_name: string }> {
  const svc = new GenericServiceClient(
    conn.robotClient,
    CUSTOMER_DETECTOR_SERVICE_NAME
  );
  const command = Struct.fromJson({ get_info: true });
  const result = await svc.doCommand(command);
  return result as unknown as { camera_name: string };
}

