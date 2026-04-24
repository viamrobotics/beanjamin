"use client";

import { useState, useEffect, useRef, useCallback } from "react";
import Image from "next/image";
import { DRINKS } from "./order/drinks";
import { ChooseDrink } from "./order/choose-drink";
import { EnterName } from "./order/enter-name";
import dynamic from "next/dynamic";
const FaceRegister = dynamic(() => import("./order/face-register").then(m => ({ default: m.FaceRegister })), { ssr: false });
import { OrderConfirmation } from "./order/order-confirmation";
import { OrderTracker } from "./order/order-tracker";
import { CamFeed } from "./order/cam-feed";
import {
  getMachineMetadataKey,
  getMachineName,
  prepareOrder,
  identifyCustomer,
} from "./lib/viamClient";
import { useViamConnection } from "./lib/useViamConnection";
import { misspellName } from "./lib/misspell";

type Step = "welcome" | "drink" | "name" | "face-register" | "confirmation";

// 'hidden' = no tracker. 'auto' = right-rail tracker that appeared when the
// user placed an order; auto-dismisses when the queue drains. 'manual' =
// full-screen "View queue" takeover; stays up until the user closes it.
type TrackerMode = "hidden" | "auto" | "manual";

// Width of the order tracker panel in both auto and manual modes.
const TRACKER_PANEL_WIDTH = "w-[560px]";

const LOST_CONNECTION_MSG =
  "Lost connection to the machine. Please wait for it to reconnect and try again.";

export default function Home() {
  const [step, setStep] = useState<Step>("welcome");
  const [name, setName] = useState("");
  const [email, setEmail] = useState("");
  const [selectedDrink, setSelectedDrink] = useState<string | null>(null);
  const [misspelled, setMisspelled] = useState("");
  const [loading, setLoading] = useState(false);
  const [drinkRejection, setDrinkRejection] = useState<string | null>(null);
  const [welcomeBack, setWelcomeBack] = useState<string | null>(null);
  const [machineName, setMachineName] = useState<string | null>(null);
  const [camName, setCamName] = useState<string | undefined>(undefined);
  const [trackerMode, setTrackerMode] = useState<TrackerMode>("hidden");

  // Viam connection state
  const {
    conn: viamConn,
    connected,
    error: connectionError,
  } = useViamConnection();
  const anthropicKey = useRef<string>("");
  const [appError, setAppError] = useState<string | null>(null);
  const viamError = connectionError ?? appError;

  // Make the machine-name header visible (falling back to "Beanjamin") if the
  // initial connect failed — otherwise the heading stays invisible forever.
  useEffect(() => {
    if (viamError) setMachineName((m) => m ?? "Beanjamin");
  }, [viamError]);

  // Re-run on reconnect; values are stable per machine-session so the
  // redundant calls are cheap.
  useEffect(() => {
    if (!connected || !viamConn) return;
    let cancelled = false;
    (async () => {
      try {
        const name = await getMachineName(viamConn);
        if (!cancelled) setMachineName(name || "Beanjamin");
      } catch (err) {
        console.log("[app] failed to fetch machine name:", err);
        if (!cancelled) setMachineName("Beanjamin");
      }

      const key = await getMachineMetadataKey(viamConn, "anthropic_api_key");
      if (cancelled) return;
      if (!key) {
        setAppError(
          'Machine metadata missing "anthropic_api_key". Set it in the Viam app.'
        );
        return;
      }
      anthropicKey.current = key;

      const configuredCam = await getMachineMetadataKey(viamConn, "cam_name");
      if (!cancelled && configuredCam) setCamName(configuredCam);
    })();
    return () => {
      cancelled = true;
    };
  }, [connected, viamConn]);

  // Customer identification is per-visit, not per-app. Run whenever we're on
  // the welcome screen with a live connection so each new customer who walks
  // up to the kiosk gets the camera-based "welcome back" treatment.
  useEffect(() => {
    if (step !== "welcome" || !connected || !viamConn) return;
    let cancelled = false;
    (async () => {
      try {
        console.log("[app] attempting to identify customer...");
        const id = await identifyCustomer(viamConn);
        console.log("[app] identify result:", id);
        if (cancelled || !id.identified || !id.name) return;
        setName(id.name);
        setEmail(id.email ?? "");
        setWelcomeBack(id.name);
        console.log("[app] welcome back:", id.name);
      } catch (err) {
        console.log("[app] identify skipped:", err);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [step, connected, viamConn]);

  async function handleDrinkNext() {
    if (!selectedDrink) return;
    setDrinkRejection(null);

    const supportedDrinks = new Set(["espresso", "lungo", "decaf", "decaf_lungo"]);
    if (supportedDrinks.has(selectedDrink)) {
      setStep("name");
      return;
    }

    // ~25% chance: ignore what they picked and just make espresso anyway
    if (Math.random() < 0.25) {
      setSelectedDrink("espresso");
      setStep("name");
      return;
    }

    // Non-espresso: call prepare_order so the robot speaks the rejection
    if (connected && viamConn) {
      try {
        const drink = DRINKS.find((d) => d.id === selectedDrink);
        await prepareOrder(viamConn, {
          drink: selectedDrink,
          drinkLabel: drink?.label ?? selectedDrink,
          customerName: "",
        });
      } catch (err) {
        const msg = err instanceof Error ? err.message : String(err);
        const colonIdx = msg.indexOf(": ");
        setDrinkRejection(colonIdx >= 0 ? msg.slice(colonIdx + 2) : msg);
      }
    } else {
      setDrinkRejection(LOST_CONNECTION_MSG);
    }
  }

  async function placeOrder(misspelledName: string): Promise<void> {
    console.log("[app] placing order for:", misspelledName);
    setMisspelled(misspelledName);
    setAppError(null);

    // Route failures back to the name screen so the red error is visible and
    // the user can retry. If we were on face-register, mark them as "welcomed
    // back" so retry skips face-register (the server already stored their
    // face from the earlier successful finishRegistration).
    const rewindToNameAfterFailure = () => {
      if (step === "face-register") setWelcomeBack(name);
      setStep("name");
    };

    if (!connected || !viamConn) {
      setAppError(LOST_CONNECTION_MSG);
      rewindToNameAfterFailure();
      return;
    }

    const drink = DRINKS.find((d) => d.id === selectedDrink);
    try {
      await prepareOrder(viamConn, {
        drink: selectedDrink!,
        drinkLabel: drink?.label ?? selectedDrink!,
        customerName: misspelledName,
        pronunciation: undefined,
      });
      setStep("confirmation");
      setTrackerMode("auto");
    } catch (err) {
      console.error("prepare_order failed:", err);
      const msg = err instanceof Error ? err.message : String(err);
      setAppError(`Could not place order: ${msg}`);
      rewindToNameAfterFailure();
    }
  }

  async function handleSubmit() {
    if (!name.trim() || !selectedDrink) return;
    setLoading(true);

    try {
      if (!anthropicKey.current) {
        throw new Error("Anthropic API key not loaded");
      }
      const result = await misspellName(name.trim(), anthropicKey.current);
      setMisspelled(result.misspelled || name);

      // If the customer provided an email and wasn't already identified,
      // offer face registration before proceeding to the order.
      console.log("[app] handleSubmit:", { email: email.trim(), welcomeBack, name });
      if (email.trim() && !welcomeBack) {
        console.log("[app] routing to face-register");
        setStep("face-register");
      } else {
        console.log("[app] skipping face-register:", !email.trim() ? "no email" : "returning customer");
        await placeOrder(result.misspelled || name);
      }
    } catch (err) {
      console.error("Misspell error:", err);
      await placeOrder(name);
    } finally {
      setLoading(false);
    }
  }

  const handleConfirmationDismiss = useCallback(() => {
    setStep("welcome");
    setName("");
    setEmail("");
    setSelectedDrink(null);
    setWelcomeBack(null);
    setAppError(null);
  }, []);

  // Only the 'auto' tracker fires onEmpty — 'manual' is persistent — so this
  // always collapses to 'hidden'.
  const handleTrackerEmpty = useCallback(() => {
    setTrackerMode("hidden");
  }, []);

  const handleTrackerClose = useCallback(() => {
    setTrackerMode("hidden");
  }, []);

  // --- Render the left panel content based on step ---
  function renderStep() {
    if (step === "face-register") {
      return (
        <FaceRegister
          name={name}
          email={email}
          viamConn={viamConn}
          onBack={() => setStep("name")}
          onComplete={() => placeOrder(misspelled)}
          onSkip={() => placeOrder(misspelled)}
        />
      );
    }

    if (step === "confirmation") {
      const drink = DRINKS.find((d) => d.id === selectedDrink);
      return (
        <OrderConfirmation
          misspelled={misspelled}
          actualName={name}
          drinkLabel={drink?.label ?? ""}
          onDismiss={handleConfirmationDismiss}
        />
      );
    }

    if (step === "name") {
      return (
        <EnterName
          name={name}
          email={email}
          loading={loading}
          connected={connected}
          error={appError}
          onNameChange={(n) => {
            setName(n);
            if (appError) setAppError(null);
          }}
          onEmailChange={setEmail}
          onBack={() => setStep("drink")}
          onSubmit={handleSubmit}
        />
      );
    }

    if (step === "drink") {
      return (
        <ChooseDrink
          selectedDrink={selectedDrink}
          rejection={drinkRejection}
          connected={connected}
          onSelect={(id) => {
            setDrinkRejection(null);
            setSelectedDrink(id);
          }}
          onBack={() => setStep("welcome")}
          onNext={handleDrinkNext}
        />
      );
    }

    // --- Welcome screen (default) ---
    return (
      <main className="h-full bg-white flex flex-col items-center justify-center p-8 font-sans">
        <Image
          src="./beans.png"
          alt="coffee beans"
          width={280}
          height={280}
          priority
          className="anim-pop w-[min(280px,40vw)] h-auto mb-6"
        />

        <h1
          className={`anim-in-hero text-4xl font-mono font-bold text-neutral-900 mb-4 ${machineName ? "" : "invisible"}`}
          style={{ animationDelay: "500ms" }}
        >
          Hi, I&apos;m {machineName ?? "Beanjamin"}
        </h1>
        <p
          className="anim-in-hero text-neutral-500 text-center mb-10 text-lg"
          style={{ animationDelay: "800ms" }}
        >
          Freshly brewed coffee, questionable spelling
        </p>

        {welcomeBack && (
          <div
            className="anim-in-hero w-full max-w-sm bg-neutral-50 border-2 border-neutral-200 rounded-2xl px-6 py-5 text-center mb-8"
            style={{ animationDelay: "1000ms" }}
          >
            <p className="text-2xl font-semibold text-neutral-900">
              Welcome back, {welcomeBack}! 👋
            </p>
            <p className="text-neutral-500 text-sm mt-1">
              {[
                "Nice to see you again ☕",
                "Couldn't stay away, huh? ☕",
                "Back so soon? We're flattered ☕",
                "Oh look who it is! Your usual? 👀",
                "At this point, we should charge rent 💅",
              ][Math.floor(Math.random() * 5)]}
            </p>
          </div>
        )}

        {viamError && (
          <p className="text-red-500 text-sm text-center mb-4 max-w-md">
            {viamError}
          </p>
        )}

        {!connected && !viamError && (
          <p className="text-neutral-500 text-sm text-center mb-4">
            Connecting to the machine…
          </p>
        )}

        <button
          onClick={() => setStep("drink")}
          disabled={!connected}
          className="anim-in-hero press px-20 py-4 text-lg font-medium bg-neutral-900 text-white rounded-full transition-colors hover:bg-neutral-800 disabled:bg-neutral-300 disabled:cursor-not-allowed disabled:hover:bg-neutral-300"
          style={{ animationDelay: welcomeBack ? "1400ms" : "1200ms" }}
        >
          Place an order
        </button>

        {trackerMode === "hidden" && (
          <button
            onClick={() => setTrackerMode("manual")}
            disabled={!connected}
            className="anim-in-hero mt-6 text-sm font-mono text-neutral-400 uppercase tracking-widest hover:text-neutral-600 transition-colors disabled:text-neutral-300 disabled:cursor-not-allowed disabled:hover:text-neutral-300"
            style={{ animationDelay: welcomeBack ? "1600ms" : "1400ms" }}
          >
            View queue
          </button>
        )}
      </main>
    );
  }

  // Manual "View queue" mode takes over the full screen: larger cam feed on
  // the left, order list on the right. The ordering flow is hidden.
  if (trackerMode === "manual") {
    return (
      <div className="h-dvh flex">
        <div className="flex-1 min-w-0 h-full bg-neutral-900">
          <CamFeed viamConn={viamConn} cameraName={camName} fill />
        </div>
        <div className={`${TRACKER_PANEL_WIDTH} shrink-0 border-l border-neutral-200`}>
          <OrderTracker
            viamConn={viamConn}
            onEmpty={handleTrackerEmpty}
            persistent
            onClose={handleTrackerClose}
          />
        </div>
      </div>
    );
  }

  return (
    <div className="h-dvh flex">
      {/* Left panel: ordering flow */}
      <div className="flex-1 min-w-0 relative transition-all duration-500">
        {renderStep()}
      </div>

      {/* Right panel: live cam feed + order tracker */}
      {trackerMode === "auto" && (
        <div className={`${TRACKER_PANEL_WIDTH} shrink-0 border-l border-neutral-200 flex flex-col`}>
          <CamFeed viamConn={viamConn} cameraName={camName} />
          <div className="flex-1 min-h-0">
            <OrderTracker
              viamConn={viamConn}
              onEmpty={handleTrackerEmpty}
            />
          </div>
        </div>
      )}
    </div>
  );
}
