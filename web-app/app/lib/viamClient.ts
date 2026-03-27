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

const COFFEE_SERVICE_NAME = "coffee-lifecycle";

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
