"use client";

import { useState, useEffect, useCallback } from "react";
import { useSearchParams } from "next/navigation";
import Image from "next/image";
import Link from "next/link";
import {
  DRINKS,
  REGULAR_BEANS,
  beanOptionsOf,
  hasMilkChoice,
  spokenOrder,
  splitDrink,
  type DrinkChoice,
} from "./order/drinks";
import { ChooseDrink } from "./order/choose-drink";
import { DrinkName } from "./order/drink-name";
import { EnterName } from "./order/enter-name";
import dynamic from "next/dynamic";
const FaceRegister = dynamic(
  () =>
    import("./order/face-register").then((m) => ({ default: m.FaceRegister })),
  { ssr: false },
);
import { OrderConfirmation } from "./order/order-confirmation";
import { Profile } from "./order/profile";
import { OrderTracker } from "./order/order-tracker";
import { CamFeed } from "./order/cam-feed";
import {
  getMachineMetadataKey,
  getMachineName,
  prepareOrder,
  getUsualOrders,
  identifyCustomer,
  getQueue,
  type Fulfillment,
} from "./lib/viamClient";
import { useViamConnection } from "./lib/useViamConnection";
import { misspellName } from "./lib/misspell";

// A usual as the kiosk holds it: the drink and beans split apart (see
// splitDrink), with the fulfillment it is usually ordered for.
type Usual = DrinkChoice & { fulfillment: Fulfillment };

type Step =
  | "welcome"
  | "drink"
  | "name"
  | "face-register"
  | "confirmation"
  | "profile";

// 'hidden' = no tracker. 'auto' = right-rail tracker that appeared either
// because this kiosk placed an order or because the page-level poll noticed
// orders on the backend (e.g. another tab); auto-dismisses when the queue
// drains. 'manual' = full-screen "View queue" takeover; stays up until the
// user closes it.
type TrackerMode = "hidden" | "auto" | "manual";

// Width of the order tracker panel in both auto and manual modes.
const TRACKER_PANEL_WIDTH = "w-[min(560px,45vw)]";

// How often the welcome screen re-checks who is at the camera, and how long
// nobody may be recognised before the greeting clears (`?identifyIntervalMs=`
// and `?clearAfterSec=` override the defaults). Requiring a run of misses
// keeps a turned head from flickering "Welcome back" away. Each check grabs a
// camera frame and runs the face model, so the interval has a floor.
const DEFAULT_IDENTIFY_INTERVAL_MS = 1000;
const MIN_IDENTIFY_INTERVAL_MS = 250;
const DEFAULT_CLEAR_AFTER_SEC = 2;

const LOST_CONNECTION_MSG =
  "Lost connection to the machine. Please wait for it to reconnect and try again.";

export function Kiosk() {
  const searchParams = useSearchParams();
  const partId = searchParams.get("partId") ?? "";
  const kioskMode = searchParams.get("kiosk") === "1";
  // Prefill for personal links; restored after each order, so the next one
  // starts from the same values.
  const prefillName = searchParams.get("name") ?? "";
  const prefillEmail = searchParams.get("email") ?? "";
  const clearAfterParam = Number(searchParams.get("clearAfterSec"));
  const clearAfterSec =
    clearAfterParam > 0 ? clearAfterParam : DEFAULT_CLEAR_AFTER_SEC;
  const intervalParam = Number(searchParams.get("identifyIntervalMs"));
  const identifyIntervalMs =
    intervalParam > 0
      ? Math.max(intervalParam, MIN_IDENTIFY_INTERVAL_MS)
      : DEFAULT_IDENTIFY_INTERVAL_MS;
  const missesToClear = Math.max(
    1,
    Math.ceil((clearAfterSec * 1000) / identifyIntervalMs),
  );

  // iOS Safari doesn't shrink the layout viewport (dvh/vh) when the on-screen
  // keyboard opens — only the visual viewport. Mirror visualViewport.height into
  // --app-height so the full-height wrappers track the keyboard and centered
  // content lifts above it. Falls back to 100dvh when unavailable (SSR/no JS).
  useEffect(() => {
    const vv = window.visualViewport;
    if (!vv) return;
    const apply = () =>
      document.documentElement.style.setProperty(
        "--app-height",
        `${vv.height}px`,
      );
    apply();
    vv.addEventListener("resize", apply);
    vv.addEventListener("scroll", apply);
    return () => {
      vv.removeEventListener("resize", apply);
      vv.removeEventListener("scroll", apply);
    };
  }, []);

  const [step, setStep] = useState<Step>("welcome");
  const [name, setName] = useState(prefillName);
  const [email, setEmail] = useState(prefillEmail);
  const [selectedDrink, setSelectedDrink] = useState<string | null>(null);
  const [selectedBeans, setSelectedBeans] = useState(REGULAR_BEANS);
  // The beans this machine grinds, from the queue poll; regular only until
  // the first poll answers.
  const [beanOptions, setBeanOptions] = useState<string[]>([REGULAR_BEANS]);
  // A pick the machine no longer offers falls back to regular.
  const chosenBeans = beanOptions.includes(selectedBeans)
    ? selectedBeans
    : REGULAR_BEANS;
  // Defaults to pickup everywhere; delivery requires an email (enforced here
  // and again by the backend).
  const [fulfillment, setFulfillment] = useState<Fulfillment>("pickup");
  // The machine's milk_options (default first), read off its queue status.
  const [milkOptions, setMilkOptions] = useState<string[]>([]);
  // The customer's pick; "" until they choose, which means the default. A pick
  // the machine no longer offers falls back to the default too.
  const [milk, setMilk] = useState("");
  const chosenMilk = milkOptions.includes(milk) ? milk : (milkOptions[0] ?? "");
  const [misspelled, setMisspelled] = useState("");
  const [loading, setLoading] = useState(false);
  const [welcomeBack, setWelcomeBack] = useState<string | null>(null);
  // The greeted customer's frequent orders, most-frequent first, offered as
  // one-tap shortcuts on the welcome card; empty when they have none.
  const [usuals, setUsuals] = useState<Usual[]>([]);
  // Only offer usuals the machine can still make: a drink on the menu, from
  // beans it still grinds.
  const orderableUsuals = usuals.filter(
    (u) =>
      (DRINKS.find((d) => d.id === u.drink)?.available ?? true) &&
      beanOptions.includes(u.beans),
  );
  const [machineName, setMachineName] = useState<string | null>(null);
  const [camName, setCamName] = useState<string | undefined>(undefined);
  const [trackerMode, setTrackerMode] = useState<TrackerMode>("hidden");

  // Viam connection state
  const {
    conn: viamConn,
    connected,
    error: connectionError,
  } = useViamConnection(partId);
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

      const configuredCam = await getMachineMetadataKey(viamConn, "cam_name");
      if (!cancelled && configuredCam) setCamName(configuredCam);
    })();
    return () => {
      cancelled = true;
    };
  }, [connected, viamConn]);

  // Cold-start visibility: a fresh app instance starts with trackerMode='hidden',
  // so a second tab/instance opened while the machine is already brewing for
  // someone else would never see the queue. Poll getQueue at the page level
  // and flip into 'auto' whenever the backend reports any orders. The
  // OrderTracker still owns its own per-second polling for rendering and
  // calls onEmpty -> handleTrackerEmpty to hide itself once the queue drains.
  // We only promote 'hidden' -> 'auto' here; 'manual' stays put.
  useEffect(() => {
    if (!connected || !viamConn) return;
    let cancelled = false;

    const tick = async () => {
      try {
        const q = await getQueue(viamConn);
        if (cancelled) return;
        // Keep the same arrays while the options are unchanged, so the 2s poll
        // doesn't re-render the drink screen.
        const options = beanOptionsOf(q);
        setBeanOptions((prev) =>
          prev.join() === options.join() ? prev : options,
        );
        const milks = q.milk_options ?? [];
        setMilkOptions((prev) => (prev.join() === milks.join() ? prev : milks));
        if (q.orders.length > 0) {
          setTrackerMode((m) => (m === "hidden" ? "auto" : m));
        }
      } catch (err) {
        console.error("[app] queue visibility poll failed:", err);
      }
    };
    tick();
    const interval = setInterval(tick, 2000);

    return () => {
      cancelled = true;
      clearInterval(interval);
    };
  }, [connected, viamConn]);

  // Customer identification is per-visit, not per-app. While we're on the
  // welcome screen with a live connection, keep checking the camera: a
  // recognised customer gets the "welcome back" treatment, and once they have
  // walked away (a run of misses) it is cleared along with the prefilled
  // name and email, so the next person isn't greeted as, or ordering as, them.
  useEffect(() => {
    if (step !== "welcome" || !connected || !viamConn) return;
    let cancelled = false;
    let greeted: string | null = null;
    let misses = 0;
    (async () => {
      while (!cancelled) {
        try {
          const id = await identifyCustomer(viamConn);
          if (cancelled) return;
          if (id.identified && id.name) {
            misses = 0;
            if (greeted !== id.name) {
              greeted = id.name;
              setName(id.name);
              setEmail(id.email ?? "");
              setWelcomeBack(id.name);
              setUsuals([]);
              console.log("[app] welcome back:", id.name);
              const who = id.name;
              getUsualOrders(viamConn, id.email ?? "")
                .then((found) => {
                  // Drop the answer if they've left or someone else has stepped up.
                  if (!cancelled && greeted === who)
                    setUsuals(
                      found.map((u) => ({
                        ...splitDrink(u.drink, u.beans),
                        fulfillment: u.fulfillment,
                      })),
                    );
                })
                .catch((err) => console.log("[app] usuals lookup skipped:", err));
            }
          } else if (greeted && ++misses >= missesToClear) {
            console.log("[app] customer left, clearing greeting");
            greeted = null;
            misses = 0;
            setName(prefillName);
            setEmail(prefillEmail);
            setWelcomeBack(null);
            setUsuals([]);
          }
        } catch (err) {
          // A failed check says nothing about who is there; keep the state.
          console.log("[app] identify skipped:", err);
        }
        await new Promise((resolve) => setTimeout(resolve, identifyIntervalMs));
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [
    step,
    connected,
    viamConn,
    prefillName,
    prefillEmail,
    identifyIntervalMs,
    missesToClear,
  ]);

  function handleDrinkNext() {
    if (!selectedDrink) return;
    setStep("name");
  }

  /** `misspelledName` is shown and spoken; `name` is what the order is filed under. */
  async function placeOrder(
    misspelledName: string,
    drink = selectedDrink,
    beans = chosenBeans,
    fulfillmentOverride?: Fulfillment,
  ): Promise<void> {
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

    try {
      await prepareOrder(viamConn, {
        drink: drink!,
        beans,
        drinkLabel: spokenOrder(drink!, beans, chosenMilk),
        customerName: name.trim(),
        modifiedName: misspelledName,
        // Credits the drink to this customer's history; empty = anonymous
        // (ignored). Required by the backend when fulfillment is delivery.
        customerEmail: email.trim(),
        pronunciation: undefined,
        // Passed in by a usual, whose fulfillment state hasn't rendered yet.
        fulfillment: fulfillmentOverride ?? fulfillment,
        ...(hasMilkChoice(drink!) && chosenMilk && { milk: chosenMilk }),
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

  /** Orders one of the greeted customer's usuals straight away: its drink, beans and fulfillment. */
  async function orderUsual(usual: Usual) {
    if (!name.trim() || loading) return;
    setLoading(true);
    // The selection and fulfillment feed the confirmation screen, and a failed
    // order rewinds to the name screen with them intact for a retry.
    setSelectedDrink(usual.drink);
    setSelectedBeans(usual.beans);
    setFulfillment(usual.fulfillment);
    try {
      await placeOrder(
        misspellName(name.trim()) || name,
        usual.drink,
        usual.beans,
        usual.fulfillment,
      );
    } finally {
      setLoading(false);
    }
  }

  async function handleSubmit() {
    if (!name.trim() || !selectedDrink) return;
    // Delivery orders must be attributable — the delivery bot identifies the
    // recipient by email. The backend rejects these too; catching it here
    // keeps the error on the form instead of a failed order round-trip.
    if (fulfillment === "delivery" && !email.trim()) {
      setAppError("Delivery orders need an email so we know who to deliver to.");
      return;
    }
    setLoading(true);

    try {
      const misspelledName = misspellName(name.trim()) || name;
      setMisspelled(misspelledName);

      // If the customer provided an email and wasn't already identified,
      // offer face registration before proceeding to the order.
      console.log("[app] handleSubmit:", {
        email: email.trim(),
        welcomeBack,
        name,
      });
      if (email.trim() && !welcomeBack) {
        console.log("[app] routing to face-register");
        setStep("face-register");
      } else {
        console.log(
          "[app] skipping face-register:",
          !email.trim() ? "no email" : "returning customer",
        );
        await placeOrder(misspelledName);
      }
    } catch (err) {
      console.error("Order submit error:", err);
      await placeOrder(name);
    } finally {
      setLoading(false);
    }
  }

  // Back from the profile page to a fresh welcome screen. The profile page may
  // have shown someone other than whoever is recognised, and the welcome
  // screen only clears a greeting it set itself, so start it from scratch.
  function leaveProfile() {
    setStep("welcome");
    setName(prefillName);
    setEmail(prefillEmail);
    setWelcomeBack(null);
    setUsuals([]);
  }

  const handleConfirmationDismiss = useCallback(() => {
    setStep("welcome");
    setName(prefillName);
    setEmail(prefillEmail);
    setSelectedDrink(null);
    setSelectedBeans(REGULAR_BEANS);
    setFulfillment("pickup");
    setMilk("");
    setWelcomeBack(null);
    setUsuals([]);
    setAppError(null);
  }, [prefillName, prefillEmail]);

  // The 'auto' tracker fires onEmpty when the queue drains; 'manual' is
  // persistent and never fires this. Always collapse to 'hidden'.
  const handleTrackerEmpty = useCallback(() => {
    setTrackerMode("hidden");
  }, []);

  // × on the manual takeover: if the queue still has work, drop back to the
  // 'auto' right-rail so the customer keeps seeing it; otherwise collapse to
  // 'hidden'. We check the queue here because OrderTracker's hasSeenOrders
  // ref resets when it remounts in non-persistent mode, so we can't rely on
  // its onEmpty path to collapse an already-empty queue.
  const handleTrackerClose = useCallback(async () => {
    if (!viamConn) {
      setTrackerMode("hidden");
      return;
    }
    try {
      const q = await getQueue(viamConn);
      setTrackerMode(q.orders.length > 0 ? "auto" : "hidden");
    } catch {
      setTrackerMode("hidden");
    }
  }, [viamConn]);

  // Escape closes the manual takeover, mirroring the × button.
  useEffect(() => {
    if (trackerMode !== "manual") return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") handleTrackerClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [trackerMode, handleTrackerClose]);

  // --- Render the left panel content based on step ---
  function renderStep() {
    if (step === "profile") {
      return (
        <Profile
          viamConn={viamConn}
          connected={connected}
          recognizedName={welcomeBack}
          recognizedEmail={email}
          onBack={leaveProfile}
        />
      );
    }

    if (step === "face-register") {
      return (
        <FaceRegister
          name={name}
          email={email.trim()}
          viamConn={viamConn}
          onBack={() => setStep("name")}
          onComplete={() => placeOrder(misspelled)}
          onSkip={() => placeOrder(misspelled)}
        />
      );
    }

    if (step === "confirmation") {
      return (
        <OrderConfirmation
          misspelled={misspelled}
          actualName={name}
          drink={selectedDrink ?? ""}
          beans={chosenBeans}
          milk={chosenMilk}
          onDismiss={handleConfirmationDismiss}
          showBack={!kioskMode}
        />
      );
    }

    if (step === "name") {
      return (
        <EnterName
          name={name}
          email={email}
          emailRequired={fulfillment === "delivery"}
          loading={loading}
          connected={connected}
          error={appError}
          onNameChange={(n) => {
            setName(n);
            if (appError) setAppError(null);
          }}
          onEmailChange={(e) => {
            setEmail(e);
            if (appError) setAppError(null);
          }}
          onBack={() => setStep("drink")}
          onSubmit={handleSubmit}
        />
      );
    }

    if (step === "drink") {
      return (
        <ChooseDrink
          selectedDrink={selectedDrink}
          beanOptions={beanOptions}
          selectedBeans={chosenBeans}
          fulfillment={fulfillment}
          milk={chosenMilk}
          milkOptions={milkOptions}
          connected={connected}
          onSelect={setSelectedDrink}
          onBeansChange={setSelectedBeans}
          onFulfillmentChange={setFulfillment}
          onMilkChange={setMilk}
          onBack={() => setStep("welcome")}
          onNext={handleDrinkNext}
        />
      );
    }

    // --- Welcome screen (default) ---
    return (
      <main className="relative h-full bg-white flex flex-col items-center justify-center p-8 font-sans">
        {!kioskMode && (
          <Link
            href="/"
            className="absolute top-4 left-4 text-sm text-neutral-500 hover:text-neutral-900 transition-colors"
          >
            ← Back to Fleet Dashboard
          </Link>
        )}
        <button
          type="button"
          onClick={() => setStep("profile")}
          disabled={!connected}
          aria-label="Edit my profile"
          title="Edit my profile"
          className="anim-in absolute bottom-6 left-6 h-11 w-11 rounded-full border border-neutral-200 bg-white text-neutral-900 transition-colors hover:bg-neutral-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-neutral-400 disabled:text-neutral-300 disabled:hover:bg-white disabled:cursor-not-allowed"
          style={{ animationDelay: "1400ms" }}
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
            <circle cx="12" cy="8" r="4" />
            <path d="M4 21v-1a6 6 0 0 1 6-6h4a6 6 0 0 1 6 6v1" />
          </svg>
        </button>
        <Image
          src="/beans.png"
          alt="coffee beans"
          width={280}
          height={280}
          priority
          className="anim-pop w-[min(280px,40vw)] h-auto mb-6"
        />

        <h1
          className={`anim-in-hero text-3xl sm:text-4xl font-mono font-bold text-neutral-900 mb-4 text-center break-words max-w-full ${machineName ? "" : "invisible"}`}
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
              {
                [
                  "Nice to see you again ☕",
                  "Couldn't stay away, huh? ☕",
                  "Back so soon? We're flattered ☕",
                  "Oh look who it is! Your usual? 👀",
                  "At this point, we should charge rent 💅",
                ][Math.floor(Math.random() * 5)]
              }
            </p>
          </div>
        )}

        {welcomeBack && orderableUsuals.length > 0 && (
          <div className="w-full max-w-sm mb-4 flex flex-col gap-2">
            {orderableUsuals.length > 1 && (
              <p
                className="anim-in-hero text-sm text-neutral-500 text-center"
                style={{ animationDelay: "1200ms" }}
              >
                Your usuals
              </p>
            )}
            {orderableUsuals.map((u, i) => (
              <button
                key={`${u.drink}-${u.beans}-${u.fulfillment}`}
                onClick={() => orderUsual(u)}
                disabled={!connected || loading}
                className="anim-in-hero press w-full px-6 py-4 text-lg font-medium bg-white text-neutral-900 border-2 border-neutral-900 rounded-full transition-colors hover:bg-neutral-100 disabled:opacity-50 disabled:cursor-not-allowed"
                style={{ animationDelay: `${1200 + i * 100}ms` }}
              >
                {orderableUsuals.length === 1 ? "Your usual: " : ""}
                <DrinkName drink={u.drink} beans={u.beans} /> &middot;{" "}
                {u.fulfillment === "delivery" ? "Delivery" : "Pickup"}
              </button>
            ))}
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
          onClick={() => {
            // Start every new order flow fresh. Going back within the flow
            // (name -> drink) still preserves the selection; only entering from
            // welcome clears it.
            setSelectedDrink(null);
            setSelectedBeans(REGULAR_BEANS);
            setFulfillment("pickup");
            setMilk("");
            setStep("drink");
          }}
          disabled={!connected}
          className="anim-in-hero press px-12 sm:px-20 py-4 text-lg font-medium whitespace-nowrap bg-neutral-900 text-white rounded-full transition-colors hover:bg-neutral-800 disabled:bg-neutral-300 disabled:cursor-not-allowed disabled:hover:bg-neutral-300"
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
      <div className="flex h-[var(--app-height,100dvh)]">
        <div className="flex-1 min-w-0 h-full bg-neutral-900">
          <CamFeed viamConn={viamConn} cameraName={camName} fill />
        </div>
        <div
          className={`${TRACKER_PANEL_WIDTH} shrink-0 border-l border-neutral-200`}
        >
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
    <div className="flex h-[var(--app-height,100dvh)]">
      {/* Left panel: ordering flow */}
      <div className="flex-1 min-w-0 relative transition-all duration-500">
        {renderStep()}
      </div>

      {/* Right panel: live cam feed + order tracker */}
      {trackerMode === "auto" && (
        <div
          className={`${TRACKER_PANEL_WIDTH} shrink-0 border-l border-neutral-200 flex flex-col`}
        >
          <CamFeed
            viamConn={viamConn}
            cameraName={camName}
            onExpand={() => setTrackerMode("manual")}
          />
          <div className="flex-1 min-h-0">
            <OrderTracker viamConn={viamConn} onEmpty={handleTrackerEmpty} />
          </div>
        </div>
      )}
    </div>
  );
}
