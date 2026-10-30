"use client";

import { useCallback, useEffect, useState } from "react";
import dynamic from "next/dynamic";
import {
  deleteFace,
  listFaces,
  type FaceProfile,
  type ViamConnection,
} from "../lib/viamClient";

const FaceRegister = dynamic(
  () => import("./face-register").then((m) => ({ default: m.FaceRegister })),
  { ssr: false },
);

type Phase = "email" | "view" | "add";

function BackButton({ onClick }: { onClick: () => void }) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-label="Go back"
      className="anim-in absolute left-6 top-6 h-11 w-11 rounded-full border border-neutral-200 bg-white text-neutral-900 transition-colors hover:bg-neutral-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-neutral-400"
    >
      <svg
        aria-hidden="true"
        viewBox="0 0 24 24"
        className="mx-auto h-5 w-5"
        fill="none"
        stroke="currentColor"
        strokeWidth="2"
        strokeLinecap="round"
        strokeLinejoin="round"
      >
        <path d="M15 18l-6-6 6-6" />
      </svg>
    </button>
  );
}

// "Edit my profile": the photos the machine recognises a customer by. It starts
// from whoever the kiosk has just recognised, or asks for an email when nobody
// was, and lets the customer delete photos or add more through the same capture
// flow as enrolment.
export function Profile({
  viamConn,
  connected,
  recognizedName,
  recognizedEmail,
  onBack,
}: {
  viamConn: ViamConnection | null;
  connected: boolean;
  /** Who the kiosk currently thinks is at the camera; null when nobody. */
  recognizedName: string | null;
  recognizedEmail: string;
  onBack: () => void;
}) {
  const recognized = !!recognizedName && !!recognizedEmail.trim();
  const [phase, setPhase] = useState<Phase>(recognized ? "view" : "email");
  // The email whose photos are shown, and whether the customer typed it
  // (rather than being recognised), which changes what "not me" offers.
  const [email, setEmail] = useState(recognized ? recognizedEmail.trim() : "");
  const [typedEmail, setTypedEmail] = useState(false);
  const [emailInput, setEmailInput] = useState("");

  const [profile, setProfile] = useState<FaceProfile | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [reloadKey, setReloadKey] = useState(0);
  // The photo awaiting a delete confirmation, and the one being deleted.
  const [confirmFile, setConfirmFile] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<string | null>(null);

  const reload = useCallback(() => setReloadKey((k) => k + 1), []);

  useEffect(() => {
    if (phase !== "view" || !email || !viamConn) return;
    let cancelled = false;
    setLoading(true);
    listFaces(viamConn, email)
      .then((p) => {
        if (cancelled) return;
        setProfile(p);
        setError(null);
      })
      .catch((err) => {
        if (cancelled) return;
        console.error("[profile] failed to load photos:", err);
        setProfile(null);
        setError(err instanceof Error ? err.message : "Couldn't load photos");
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [phase, email, viamConn, reloadKey]);

  function notMe() {
    setProfile(null);
    setEmail("");
    setEmailInput("");
    setError(null);
    setConfirmFile(null);
    setPhase("email");
  }

  function submitEmail() {
    const typed = emailInput.trim();
    if (!typed.includes("@")) {
      setError("Enter the email you registered with.");
      return;
    }
    setError(null);
    setProfile(null);
    setEmail(typed);
    setTypedEmail(true);
    setPhase("view");
  }

  async function handleDelete(file: string) {
    if (!viamConn) return;
    setConfirmFile(null);
    setDeleting(file);
    setError(null);
    try {
      await deleteFace(viamConn, email, file);
    } catch (err) {
      console.error("[profile] delete failed:", err);
      setError(err instanceof Error ? err.message : "Couldn't delete the photo");
    } finally {
      setDeleting(null);
      // Reload either way: a failure after the file went (the machine couldn't
      // retrain) must still leave the screen showing what is really there.
      reload();
    }
  }

  // --- Add photos: the enrolment capture flow, back to this page after ---
  if (phase === "add" && profile?.name) {
    return (
      <FaceRegister
        name={profile.name}
        email={email}
        viamConn={viamConn}
        onBack={() => setPhase("view")}
        onSkip={() => setPhase("view")}
        onComplete={() => {
          reload();
          setPhase("view");
        }}
      />
    );
  }

  // --- Ask for an email ---
  if (phase === "email") {
    return (
      <main className="relative h-full bg-white flex flex-col items-center justify-center p-8 font-sans">
        <BackButton onClick={onBack} />
        <div className="w-full max-w-[512px] flex flex-col gap-8">
          <div className="flex flex-col gap-2">
            <h1 className="anim-in text-3xl tracking-tight font-semibold text-neutral-900 text-center">
              What&apos;s your email?
            </h1>
            <p className="anim-in text-neutral-500 text-center text-sm">
              The one you gave when we first took your photos.
            </p>
          </div>
          <input
            type="email"
            value={emailInput}
            onChange={(e) => {
              setEmailInput(e.target.value);
              if (error) setError(null);
            }}
            onKeyDown={(e) => e.key === "Enter" && submitEmail()}
            placeholder="Email"
            autoFocus
            className="anim-in w-full px-5 py-3 bg-neutral-50 border-2 border-neutral-200 rounded-2xl text-neutral-900 text-base outline-none focus:border-neutral-400 transition-colors font-sans"
            style={{ animationDelay: "80ms" }}
          />
          {error && (
            <p className="anim-in text-red-500 text-sm text-center -mt-4">
              {error}
            </p>
          )}
          <button
            onClick={submitEmail}
            disabled={!emailInput.trim() || !connected}
            className="anim-in press w-full py-5 text-base font-medium bg-black text-white rounded-full hover:bg-neutral-800 transition-colors disabled:opacity-30"
            style={{ animationDelay: "160ms" }}
          >
            Show my photos
          </button>
        </div>
      </main>
    );
  }

  // --- Photos ---
  const faces = profile?.faces ?? [];
  const noProfile = !!profile && !profile.registered;

  return (
    <main className="relative h-full min-h-0 overflow-y-auto bg-white flex flex-col items-center p-8 pt-24 pb-12 font-sans">
      <BackButton onClick={onBack} />
      {/* shrink-0: in this fixed-height column the block would otherwise
          compress to fit the screen, and the photos below the fold would never
          scroll into view. */}
      <div className="w-full max-w-[512px] shrink-0 flex flex-col gap-6">
        {noProfile ? (
          <div className="anim-in flex flex-col items-center gap-4 text-center">
            <h1 className="text-2xl font-semibold text-neutral-900">
              We don&apos;t have a profile for that email
            </h1>
            <p className="text-neutral-500 text-sm break-all">{email}</p>
            <p className="text-neutral-500 text-sm">
              Photos are saved when you place an order with an email and say
              yes to &ldquo;remember your face&rdquo;.
            </p>
            <button
              onClick={notMe}
              className="press px-8 py-3 text-base font-medium bg-neutral-100 text-neutral-700 rounded-full hover:bg-neutral-200 transition-colors"
            >
              Try another email
            </button>
          </div>
        ) : (
          <>
            <div className="anim-in flex flex-col items-center gap-1 text-center">
              <h1 className="text-3xl tracking-tight font-semibold text-neutral-900 break-words max-w-full">
                {typedEmail
                  ? (profile?.name ?? "Your photos")
                  : `Is this you, ${profile?.name ?? recognizedName}?`}
              </h1>
              <p className="text-neutral-500 text-sm break-all">{email}</p>
              <button
                onClick={notMe}
                className="mt-2 text-sm text-neutral-500 underline underline-offset-4 hover:text-neutral-900 transition-colors"
              >
                {typedEmail ? "Use a different email" : "Not me"}
              </button>
            </div>

            {error && (
              <p className="anim-in text-red-500 text-sm text-center">{error}</p>
            )}

            {loading && !profile && (
              <p className="text-neutral-500 text-sm text-center">
                Loading your photos…
              </p>
            )}

            {profile && faces.length === 0 && (
              <p className="anim-in text-neutral-500 text-sm text-center">
                No photos yet, so we can&apos;t recognise you. Add some below.
              </p>
            )}

            {faces.length > 0 && (
              <div className="grid grid-cols-3 gap-4">
                {faces.map((f) => {
                  const isConfirming = confirmFile === f.file;
                  const isDeleting = deleting === f.file;
                  return (
                    <div
                      key={f.file}
                      className="anim-in relative aspect-square rounded-2xl overflow-hidden border-2 border-neutral-200 bg-neutral-100"
                    >
                      {/* eslint-disable-next-line @next/next/no-img-element -- a data: URL straight from the machine; next/image can't optimise it */}
                      <img
                        src={`data:${f.mime};base64,${f.image}`}
                        alt="A photo we recognise you by"
                        className="h-full w-full object-cover"
                      />
                      {!isConfirming && !isDeleting && (
                        <button
                          type="button"
                          onClick={() => setConfirmFile(f.file)}
                          disabled={!connected || deleting !== null}
                          aria-label="Delete this photo"
                          className="absolute right-1.5 top-1.5 h-8 w-8 rounded-full bg-black/70 text-white text-lg leading-none flex items-center justify-center transition-colors hover:bg-black focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-white disabled:opacity-40"
                        >
                          &times;
                        </button>
                      )}
                      {isConfirming && (
                        <div className="absolute inset-0 bg-white/95 flex flex-col items-center justify-center gap-2 p-2 text-center">
                          <p className="text-xs font-medium text-neutral-900">
                            {faces.length === 1
                              ? "Last photo: we won't recognise you"
                              : "Delete this photo?"}
                          </p>
                          <div className="flex gap-2">
                            <button
                              type="button"
                              onClick={() => setConfirmFile(null)}
                              className="px-3 py-1.5 text-xs font-medium bg-neutral-100 text-neutral-700 rounded-full hover:bg-neutral-200"
                            >
                              Keep
                            </button>
                            <button
                              type="button"
                              onClick={() => handleDelete(f.file)}
                              className="px-3 py-1.5 text-xs font-medium bg-red-600 text-white rounded-full hover:bg-red-700"
                            >
                              Delete
                            </button>
                          </div>
                        </div>
                      )}
                      {isDeleting && (
                        <div className="absolute inset-0 bg-white/80 flex items-center justify-center text-xs text-neutral-600">
                          Deleting…
                        </div>
                      )}
                    </div>
                  );
                })}
              </div>
            )}

            {profile?.truncated && (
              <p className="text-neutral-400 text-xs text-center">
                Showing the most recent photos.
              </p>
            )}

            {profile?.registered && (
              <button
                onClick={() => setPhase("add")}
                disabled={!connected || deleting !== null}
                className="anim-in press w-full py-5 text-base font-medium bg-black text-white rounded-full hover:bg-neutral-800 transition-colors disabled:opacity-30"
              >
                Add photos
              </button>
            )}
          </>
        )}
      </div>
    </main>
  );
}
