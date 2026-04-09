"use client";

import { useEffect, useRef, useState, useCallback } from "react";
import { StreamClient } from "@viamrobotics/sdk";
import {
  registerCustomerFace,
  finishRegistration,
  getCustomerDetectorInfo,
  type ViamConnection,
} from "../lib/viamClient";

const INITIAL_DELAY_MS = 3000;
const DELAY_BETWEEN_CAPTURES_MS = 1500;

const POSES: { label: string; arrow: string; nudge: [number, number] }[] = [
  { label: "Look straight at the camera", arrow: "", nudge: [0, 0] },
  { label: "Slowly turn to the left", arrow: "\u2190", nudge: [-18, 0] },
  { label: "Now turn to the right", arrow: "\u2192", nudge: [18, 0] },
];

function snapshotVideo(video: HTMLVideoElement): string | null {
  try {
    const canvas = document.createElement("canvas");
    canvas.width = video.videoWidth || 256;
    canvas.height = video.videoHeight || 256;
    const ctx = canvas.getContext("2d");
    if (!ctx) return null;
    ctx.drawImage(video, 0, 0, canvas.width, canvas.height);
    return canvas.toDataURL("image/jpeg", 0.8);
  } catch {
    return null;
  }
}

export function FaceRegister({
  name,
  email,
  viamConn,
  onComplete,
  onSkip,
}: {
  name: string;
  email: string;
  viamConn: ViamConnection | null;
  onComplete: () => void;
  onSkip: () => void;
}) {
  const [poseIdx, setPoseIdx] = useState(0);
  const [capturing, setCapturing] = useState(false);
  const [status, setStatus] = useState<
    "running" | "retaking" | "review" | "finishing" | "done"
  >("running");
  const [error, setError] = useState<string | null>(null);
  const [cameraName, setCameraName] = useState<string | null>(null);
  const [streamReady, setStreamReady] = useState(false);
  const [snapshots, setSnapshots] = useState<(string | null)[]>([]);
  const [retakeIdx, setRetakeIdx] = useState<number | null>(null);
  const [countdown, setCountdown] = useState<number | null>(null);
  const videoRef = useRef<HTMLVideoElement>(null);
  const streamClientRef = useRef<StreamClient | null>(null);
  const cancelled = useRef(false);

  // Fetch camera name
  useEffect(() => {
    if (!viamConn) return;
    console.log("[face-register] fetching camera name from customer-detector");
    getCustomerDetectorInfo(viamConn)
      .then((info) => {
        console.log("[face-register] camera name:", info.camera_name);
        setCameraName(info.camera_name);
      })
      .catch((err) =>
        console.error("[face-register] failed to get camera name:", err)
      );
  }, [viamConn]);

  // Live camera stream via WebRTC
  useEffect(() => {
    if (!viamConn || !cameraName || (status !== "running" && status !== "retaking")) {
      console.log("[face-register] stream skipped:", {
        viamConn: !!viamConn,
        cameraName,
        status,
      });
      return;
    }

    let stopped = false;

    async function startStream() {
      try {
        console.log("[face-register] starting stream for:", cameraName);
        const sc = new StreamClient(viamConn!.robotClient);
        streamClientRef.current = sc;

        const mediaStream = await sc.getStream(cameraName!);
        if (stopped) return;

        if (videoRef.current) {
          videoRef.current.srcObject = mediaStream;
          await videoRef.current.play();
          setStreamReady(true);
          console.log("[face-register] stream playing");
        }
      } catch (err) {
        console.error("[face-register] stream error:", err);
      }
    }

    startStream();

    return () => {
      stopped = true;
      if (streamClientRef.current && cameraName) {
        streamClientRef.current.remove(cameraName).catch(() => {});
        streamClientRef.current = null;
      }
      if (videoRef.current) {
        videoRef.current.srcObject = null;
      }
      setStreamReady(false);
    };
  }, [viamConn, cameraName, status]);

  const captureOne = useCallback(
    async (idx: number) => {
      if (!viamConn || !videoRef.current) return;
      setCapturing(true);
      try {
        console.log(
          `[face-register] capturing pose ${idx + 1}/${POSES.length}: ${POSES[idx].label}`
        );
        await registerCustomerFace(viamConn, name, email);
        const snap = snapshotVideo(videoRef.current);
        setSnapshots((prev) => {
          const next = [...prev];
          next[idx] = snap;
          return next;
        });
        console.log(`[face-register] pose ${idx + 1} captured`);
      } catch (err) {
        console.error(
          `[face-register] capture failed for pose ${idx + 1}:`,
          err
        );
        setError(
          err instanceof Error ? err.message : "Failed to capture face"
        );
      }
      setCapturing(false);
    },
    [viamConn, name, email]
  );

  // Countdown helper: ticks down each second, resolves when done
  const countdownDelay = useCallback(
    (ms: number) =>
      new Promise<void>((resolve) => {
        const seconds = Math.ceil(ms / 1000);
        setCountdown(seconds);
        let remaining = seconds;
        const tick = setInterval(() => {
          remaining--;
          if (remaining <= 0) {
            clearInterval(tick);
            setCountdown(null);
            resolve();
          } else {
            setCountdown(remaining);
          }
        }, 1000);
      }),
    []
  );

  // Auto-capture sequence
  useEffect(() => {
    if (!viamConn || status !== "running") return;
    cancelled.current = false;

    console.log("[face-register] starting capture sequence for:", email);
    async function runSequence() {
      for (let i = 0; i < POSES.length; i++) {
        if (cancelled.current) return;
        setPoseIdx(i);

        await countdownDelay(i === 0 ? INITIAL_DELAY_MS : DELAY_BETWEEN_CAPTURES_MS);
        if (cancelled.current) return;

        await captureOne(i);
      }

      if (cancelled.current) return;
      console.log("[face-register] capture complete, entering review");
      setStatus("review");
    }

    runSequence();

    return () => {
      cancelled.current = true;
    };
  }, [viamConn, status, email, captureOne]);

  // Handle retake: show capture screen for one pose, then back to review
  useEffect(() => {
    if (status !== "retaking" || retakeIdx === null || !viamConn) return;
    cancelled.current = false;

    const idx = retakeIdx;
    setPoseIdx(idx);
    console.log(`[face-register] retaking pose ${idx + 1}: ${POSES[idx].label}`);

    async function doRetake() {
      await new Promise((r) => setTimeout(r, DELAY_BETWEEN_CAPTURES_MS));
      if (cancelled.current) return;
      await captureOne(idx);
      if (cancelled.current) return;
      setRetakeIdx(null);
      setStatus("review");
    }

    doRetake();

    return () => {
      cancelled.current = true;
    };
  }, [status, retakeIdx, viamConn, captureOne]);

  async function handleConfirm() {
    setStatus("finishing");
    console.log("[face-register] finishing registration");
    try {
      const result = await finishRegistration(viamConn!, email);
      console.log("[face-register] registration complete:", result);
      setStatus("done");
      onComplete();
    } catch (err) {
      console.error("[face-register] finish registration failed:", err);
      setError(
        err instanceof Error ? err.message : "Failed to finish registration"
      );
      setStatus("review");
    }
  }

  const progress =
    status === "review" || status === "finishing" || status === "retaking"
      ? 1
      : (poseIdx + (capturing ? 0.5 : 0)) / POSES.length;

  const currentPose = POSES[poseIdx];

  // --- Review screen ---
  if (status === "review" || status === "finishing") {
    return (
      <main className="relative h-dvh bg-white flex flex-col items-center justify-center p-8 font-sans">
        <div className="w-full max-w-[512px] flex flex-col items-center gap-6">
          <h1 className="anim-in text-2xl font-semibold text-neutral-900 text-center">
            Looking good?
          </h1>
          <p className="anim-in text-neutral-500 text-center text-sm -mt-2">
            Tap a photo to retake it
          </p>

          {/* Snapshot grid */}
          <div className="flex gap-3 justify-center flex-wrap">
            {POSES.map((pose, i) => (
              <button
                key={i}
                onClick={() => {
                  if (status === "finishing") return;
                  setRetakeIdx(i);
                  setStatus("retaking");
                }}
                className="relative w-20 h-20 rounded-xl overflow-hidden border-2 border-neutral-200 hover:border-neutral-400 transition-colors"
                title={`Retake: ${pose.label}`}
              >
                {snapshots[i] ? (
                  // eslint-disable-next-line @next/next/no-img-element
                  <img
                    src={snapshots[i]!}
                    alt={pose.label}
                    className="w-full h-full object-cover"
                  />
                ) : (
                  <div className="w-full h-full bg-neutral-100 flex items-center justify-center text-neutral-400 text-xs">
                    {i + 1}
                  </div>
                )}
                <div className="absolute inset-0 flex items-center justify-center bg-black/0 hover:bg-black/20 transition-colors">
                  <span className="text-white text-lg opacity-0 hover:opacity-100 transition-opacity">
                    {"\u21BB"}
                  </span>
                </div>
              </button>
            ))}
          </div>

          {error && (
            <p className="text-red-500 text-sm text-center">{error}</p>
          )}

          <div className="flex gap-4 w-full">
            <button
              onClick={onSkip}
              disabled={status === "finishing"}
              className="press flex-1 py-4 text-base font-medium bg-neutral-100 text-neutral-600 rounded-full hover:bg-neutral-200 transition-colors disabled:opacity-30"
            >
              Skip
            </button>
            <button
              onClick={handleConfirm}
              disabled={status === "finishing"}
              className="press flex-1 py-4 text-base font-medium bg-black text-white rounded-full hover:bg-neutral-800 transition-colors disabled:opacity-30"
            >
              {status === "finishing" ? "Saving..." : "Looks good!"}
            </button>
          </div>
        </div>
      </main>
    );
  }

  // --- Capture screen ---
  return (
    <main className="relative h-dvh bg-white flex flex-col items-center justify-center p-8 font-sans">
      <div className="w-full max-w-[512px] flex flex-col items-center gap-8">
        <h1 className="anim-in text-2xl font-semibold text-neutral-900 text-center">
          Let&apos;s remember your face
        </h1>

        <p className="anim-in text-neutral-500 text-center text-sm -mt-4">
          So we can greet you next time!
        </p>

        {/* Camera viewfinder with guide overlay */}
        <div
          className="anim-in relative w-64 h-64 rounded-full bg-neutral-100 border-4 flex items-center justify-center overflow-hidden transition-colors duration-300"
          style={{
            animationDelay: "80ms",
            borderColor: capturing ? "#22c55e" : "#e5e5e5",
          }}
        >
          <video
            ref={videoRef}
            autoPlay
            muted
            playsInline
            className={`w-full h-full object-cover -scale-x-100 ${streamReady ? "" : "hidden"}`}
          />
          {!streamReady && (
            <div className="text-center px-4">
              <p className="text-4xl mb-2">{"\uD83D\uDCF7"}</p>
              <p className="text-sm text-neutral-500 font-medium">
                Connecting to camera...
              </p>
            </div>
          )}

          {/* Face guide oval + directional arrow */}
          {streamReady && (
            <div className="absolute inset-0 flex items-center justify-center pointer-events-none">
              {/* Oval guide */}
              <div
                className="w-36 h-48 border-2 border-white/50 rounded-[50%] transition-transform duration-500"
                style={{
                  transform: `translate(${currentPose.nudge[0]}px, ${currentPose.nudge[1]}px)`,
                }}
              />
              {/* Countdown */}
              {countdown !== null && !capturing && (
                <span className="absolute text-white text-5xl font-bold drop-shadow-lg">
                  {countdown}
                </span>
              )}
              {/* Directional arrow */}
              {currentPose.arrow && (
                <span
                  className="absolute text-white/70 text-4xl font-bold drop-shadow-md transition-all duration-500"
                  style={{
                    left:
                      currentPose.nudge[0] < 0
                        ? "12px"
                        : currentPose.nudge[0] > 0
                          ? "auto"
                          : "50%",
                    right: currentPose.nudge[0] > 0 ? "12px" : "auto",
                    top:
                      currentPose.nudge[1] < 0
                        ? "12px"
                        : currentPose.nudge[1] > 0
                          ? "auto"
                          : "50%",
                    bottom: currentPose.nudge[1] > 0 ? "12px" : "auto",
                    transform:
                      currentPose.nudge[0] === 0 && currentPose.nudge[1] === 0
                        ? "translate(-50%, -50%)"
                        : "none",
                  }}
                >
                  {currentPose.arrow}
                </span>
              )}
            </div>
          )}
        </div>

        {/* Instruction */}
        <div className="text-center min-h-[4em]">
          <p className="text-neutral-700 font-medium text-lg">
            {currentPose.label}
          </p>
          <p className="text-neutral-400 text-sm mt-1">
            Photo {Math.min(poseIdx + 1, POSES.length)} of {POSES.length}
          </p>
        </div>

        {/* Progress bar */}
        <div className="w-full bg-neutral-100 rounded-full h-2 overflow-hidden">
          <div
            className="bg-neutral-900 h-full rounded-full transition-all duration-500"
            style={{ width: `${progress * 100}%` }}
          />
        </div>

        {error && (
          <p className="text-red-500 text-sm text-center">{error}</p>
        )}

        <button
          onClick={onSkip}
          className="anim-in press w-full py-4 text-base font-medium bg-neutral-100 text-neutral-600 rounded-full hover:bg-neutral-200 transition-colors"
          style={{ animationDelay: "160ms" }}
        >
          Skip
        </button>
      </div>
    </main>
  );
}
