"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import * as VIAM from "@viamrobotics/sdk";
import Cookies from "js-cookie";

interface Machine {
  id: string;
  name: string;
  locationName: string;
  online: boolean;
  lastOnline: Date | null;
  mainPartId: string | null;
}

async function listMachines(client: VIAM.ViamClient): Promise<Machine[]> {
  const summaries = await client.appClient.listMachineSummaries("e76d1b3b-0468-4efd-bb7f-fb1d2b352fcb",["e6103e56-ad3a-42c6-ae5b-7cc9c310331d"], ["oeq47g5p1m"]);
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

export default function Home() {
  const [status, setStatus] = useState<"loading" | "ready" | "error">("loading");
  const [error, setError] = useState<string | null>(null);
  const [machines, setMachines] = useState<Machine[]>([]);
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

        const found = await listMachines(client);
        setMachines(found);
        setStatus("ready");
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
        setStatus("error");
      }
    })();
  }, []);

  const devButton = isLocalhost && (
    <Link
      href="/machine?mock=1"
      style={{
        display: "inline-block",
        marginBottom: "1rem",
        padding: "0.5rem 1rem",
        background: "#111",
        color: "#fff",
        borderRadius: "0.375rem",
        textDecoration: "none",
      }}
    >
      Open dev kiosk →
    </Link>
  );

  if (status === "loading")
    return (
      <div style={{ padding: "1rem", fontFamily: "monospace" }}>
        {devButton}
        <p>Loading machines…</p>
      </div>
    );
  if (status === "error")
    return (
      <div style={{ padding: "1rem", fontFamily: "monospace" }}>
        {devButton}
        <p>Error: {error}</p>
      </div>
    );
  if (machines.length === 0)
    return (
      <div style={{ padding: "1rem", fontFamily: "monospace" }}>
        {devButton}
        <p>No machines found.</p>
      </div>
    );

  return (
    <div style={{ padding: "1rem", fontFamily: "monospace" }}>
      {devButton}
      <h1>Machines ({machines.length})</h1>
      <ul>
        {machines.map((m) => {
          const row = (
            <>
              <span
                title={m.online ? "Online" : "Offline"}
                style={{
                  display: "inline-block",
                  width: "0.6em",
                  height: "0.6em",
                  borderRadius: "50%",
                  backgroundColor: m.online ? "#22c55e" : "#9ca3af",
                  marginRight: "0.5em",
                  verticalAlign: "middle",
                }}
              />
              <strong>{m.name}</strong> — {m.locationName}{" "}
              <span style={{ color: "#888" }}>({m.id})</span>
              {m.lastOnline && (
                <span style={{ color: "#888", marginLeft: "0.5em" }}>
                  · last online {m.lastOnline.toLocaleString()}
                </span>
              )}
            </>
          );
          return (
            <li key={m.id}>
              {m.mainPartId ? (
                <>
                  <Link href={`/machine?partId=${m.mainPartId}`} style={{ color: "inherit", textDecoration: "none" }}>
                    {row}
                  </Link>
                  {" "}
                  <Link
                    href={`/machine?partId=${m.mainPartId}&kiosk=1`}
                    style={{ color: "#2563eb", marginLeft: "0.5em" }}
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
    </div>
  );
}
