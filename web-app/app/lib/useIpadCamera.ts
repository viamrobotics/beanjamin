import { useCallback, useEffect, useRef, useState } from "react";
import { pushCameraFrame, type ViamConnection } from "./viamClient";

export type IpadCameraStatus =
  | "off"
  | "starting"
  | "running"
  // Permission wasn't granted without a user gesture; call start() from a tap.
  | "needs-tap"
  // The camera track ended or opening failed; the hook keeps retrying.
  | "lost";

const JPEG_QUALITY = 0.7;
const RETRY_MS = 5000;

interface Options {
  enabled: boolean;
  conn: ViamConnection | null;
  cameraName: string;
  /** Read each frame, so changing it never restarts the camera. */
  fps: number;
  width: number;
  facing: "user" | "environment";
}

function blobToBase64(blob: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onloadend = () => resolve((reader.result as string).split(",")[1]);
    reader.onerror = () => reject(reader.error);
    reader.readAsDataURL(blob);
  });
}

/**
 * Keeps this device's camera open for the life of the page and pushes JPEG
 * frames to a `viam:beanjamin:ipad-camera` component, which serves the latest
 * one as an ordinary camera.
 *
 * Render the returned `videoRef` in a <video playsInline muted> (1px and
 * transparent, not display:none; Safari stops decoding hidden video).
 *
 * The connection and frame rate live in refs, so a re-dial or a rate change
 * doesn't tear down the camera. Each frame is awaited before the next is
 * captured, so a slow link lowers the rate instead of queueing frames. State
 * only changes on transitions, never per frame, so this doesn't re-render the
 * page at the frame rate.
 */
export function useIpadCamera({
  enabled,
  conn,
  cameraName,
  fps,
  width,
  facing,
}: Options) {
  const videoRef = useRef<HTMLVideoElement>(null);
  const streamRef = useRef<MediaStream | null>(null);
  const wakeLockRef = useRef<WakeLockSentinel | null>(null);
  const connRef = useRef(conn);
  const fpsRef = useRef(fps);
  const [status, setStatus] = useState<IpadCameraStatus>("off");
  const [error, setError] = useState<string | null>(null);
  const statusRef = useRef(status);

  useEffect(() => {
    connRef.current = conn;
    fpsRef.current = fps;
    statusRef.current = status;
  }, [conn, fps, status]);

  const requestWakeLock = useCallback(async () => {
    if (wakeLockRef.current && !wakeLockRef.current.released) return;
    try {
      wakeLockRef.current = (await navigator.wakeLock?.request("screen")) ?? null;
    } catch {
      // Unsupported or refused; capture works until the screen sleeps.
    }
  }, []);

  // Opens the camera. start() calls this straight from a click handler, which
  // is the user gesture Safari wants when permission isn't already granted.
  const start = useCallback(async () => {
    if (streamRef.current) return;
    const video = videoRef.current;
    if (!video) return;
    setStatus("starting");
    try {
      const stream = await navigator.mediaDevices.getUserMedia({
        video: { facingMode: facing, width: { ideal: width } },
        audio: false,
      });
      video.srcObject = stream;
      await video.play();
      streamRef.current = stream;
      stream.getVideoTracks()[0]?.addEventListener("ended", () => {
        if (streamRef.current !== stream) return;
        streamRef.current = null;
        setStatus("lost");
      });
      void requestWakeLock();
      setError(null);
      setStatus("running");
    } catch (e) {
      const denied = e instanceof DOMException && e.name === "NotAllowedError";
      setError(e instanceof Error ? e.message : String(e));
      setStatus(denied ? "needs-tap" : "lost");
    }
  }, [facing, width, requestWakeLock]);

  useEffect(() => {
    if (!enabled) return;
    let cancelled = false;
    const video = videoRef.current;

    // Try without a gesture first; this works once the site's camera
    // permission is set to Allow. Deferred so start()'s state updates don't
    // run synchronously inside the effect.
    const autoStart = setTimeout(() => void start(), 0);

    // Safari drops the wake lock when the page is hidden, and may end the
    // camera track too. Reacquire both when the page comes back.
    const onVisible = () => {
      if (document.hidden) return;
      void requestWakeLock();
      if (!streamRef.current) void start();
    };
    document.addEventListener("visibilitychange", onVisible);

    // Retry a lost camera. A needs-tap state waits for the tap instead.
    const retry = setInterval(() => {
      if (!streamRef.current && statusRef.current === "lost") void start();
    }, RETRY_MS);

    (async () => {
      const canvas = document.createElement("canvas");
      const ctx = canvas.getContext("2d")!;
      let lastError: string | null = null;

      while (!cancelled) {
        const t0 = performance.now();
        const v = videoRef.current;
        const c = connRef.current;
        if (streamRef.current && v && c && v.videoWidth > 0) {
          const scale = Math.min(1, width / v.videoWidth);
          canvas.width = Math.round(v.videoWidth * scale);
          canvas.height = Math.round(v.videoHeight * scale);
          ctx.drawImage(v, 0, 0, canvas.width, canvas.height);
          try {
            const blob = await new Promise<Blob>((resolve, reject) =>
              canvas.toBlob(
                (b) => (b ? resolve(b) : reject(new Error("toBlob returned null"))),
                "image/jpeg",
                JPEG_QUALITY,
              ),
            );
            await pushCameraFrame(c, cameraName, await blobToBase64(blob));
            if (lastError !== null) {
              lastError = null;
              if (!cancelled) setError(null);
            }
          } catch (e) {
            const msg = e instanceof Error ? e.message : String(e);
            if (msg !== lastError) {
              lastError = msg;
              if (!cancelled) setError(msg);
            }
          }
        }
        const wait = 1000 / fpsRef.current - (performance.now() - t0);
        await new Promise((r) => setTimeout(r, Math.max(wait, 50)));
      }
    })();

    return () => {
      cancelled = true;
      clearTimeout(autoStart);
      clearInterval(retry);
      document.removeEventListener("visibilitychange", onVisible);
      streamRef.current?.getTracks().forEach((t) => t.stop());
      streamRef.current = null;
      wakeLockRef.current?.release().catch(() => {});
      wakeLockRef.current = null;
      if (video) video.srcObject = null;
      setStatus("off");
    };
  }, [enabled, cameraName, width, start, requestWakeLock]);

  return { videoRef, status, error, start };
}
