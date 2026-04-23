"use client";

import { useEffect, useRef, useState } from "react";
import { StreamClient } from "@viamrobotics/sdk";
import type { ViamConnection } from "../lib/viamClient";

const DEFAULT_CAM_NAME = "cam";

export function CamFeed({
  viamConn,
  cameraName,
}: {
  viamConn: ViamConnection | null;
  cameraName?: string;
}) {
  const name = cameraName || DEFAULT_CAM_NAME;
  const videoRef = useRef<HTMLVideoElement>(null);
  const streamClientRef = useRef<StreamClient | null>(null);
  const [ready, setReady] = useState(false);
  const [failed, setFailed] = useState(false);

  const unavailable = !viamConn || viamConn.machineId === "dev-machine";

  useEffect(() => {
    if (unavailable) return;

    let stopped = false;
    const video = videoRef.current;

    async function startStream() {
      try {
        const sc = new StreamClient(viamConn!.robotClient);
        streamClientRef.current = sc;
        const mediaStream = await sc.getStream(name);
        if (stopped) return;
        if (video) {
          video.srcObject = mediaStream;
          await video.play();
          setReady(true);
        }
      } catch (err) {
        console.error("[cam-feed] stream error:", err);
        setFailed(true);
      }
    }

    startStream();

    return () => {
      stopped = true;
      if (streamClientRef.current) {
        streamClientRef.current.remove(name).catch(() => {});
        streamClientRef.current = null;
      }
      if (video) {
        video.srcObject = null;
      }
      setReady(false);
    };
  }, [viamConn, unavailable, name]);

  if (unavailable || failed) return null;

  return (
    <div className="relative bg-neutral-900 aspect-video w-full overflow-hidden">
      <video
        ref={videoRef}
        autoPlay
        muted
        playsInline
        className={`w-full h-full object-cover transition-opacity duration-500 ${
          ready ? "opacity-100" : "opacity-0"
        }`}
      />
      {!ready && (
        <div className="absolute inset-0 flex items-center justify-center">
          <span className="text-xs font-mono text-neutral-500 uppercase tracking-widest">
            Connecting…
          </span>
        </div>
      )}
      <div className="absolute top-2 left-2 flex items-center gap-1.5 px-2 py-1 rounded-full bg-black/60 backdrop-blur-sm">
        <span className="inline-block w-1.5 h-1.5 rounded-full bg-red-500 animate-pulse" />
        <span className="text-[10px] font-mono text-white uppercase tracking-widest">
          Live
        </span>
      </div>
    </div>
  );
}
