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
import {
  connectToViam,
  getMachineMetadataKey,
  getMachineName,
  prepareOrder,
  identifyCustomer,
  type ViamConnection,
} from "./lib/viamClient";
import { misspellName } from "./lib/misspell";

type Step = "welcome" | "drink" | "name" | "face-register" | "confirmation";

export default function Home() {
  const [step, setStep] = useState<Step>("welcome");
  const [name, setName] = useState("");
  const [email, setEmail] = useState("");
  const [selectedDrink, setSelectedDrink] = useState<string | null>(null);
  const [misspelled, setMisspelled] = useState("");
  const [loading, setLoading] = useState(false);
  const [drinkRejection, setDrinkRejection] = useState<string | null>(null);
  const [welcomeBack, setWelcomeBack] = useState<string | null>(null);
  const [machineName, setMachineName] = useState("Beanjamin");
  const [showTracker, setShowTracker] = useState(false);

  // Viam connection state
  const viamConn = useRef<ViamConnection | null>(null);
  const anthropicKey = useRef<string>("");
  const [viamError, setViamError] = useState<string | null>(null);

  // Connect to Viam on mount and fetch Anthropic API key from machine metadata
  useEffect(() => {
    let cancelled = false;
    async function init() {
      try {
        console.log("[app] connecting to Viam...");
        const conn = await connectToViam();
        if (cancelled) return;
        viamConn.current = conn;
        console.log("[app] connected to Viam");

        try {
          const name = await getMachineName(conn);
          if (!cancelled && name) setMachineName(name);
        } catch (err) {
          console.log("[app] failed to fetch machine name:", err);
        }

        const key = await getMachineMetadataKey(conn, "anthropic_api_key");
        if (cancelled) return;
        if (!key) {
          setViamError(
            'Machine metadata missing "anthropic_api_key". Set it in the Viam app.'
          );
          return;
        }
        anthropicKey.current = key;

        // Try to identify a returning customer
        try {
          console.log("[app] attempting to identify returning customer...");
          const id = await identifyCustomer(conn);
          console.log("[app] identify result:", id);
          if (!cancelled && id.identified && id.name) {
            setName(id.name);
            setEmail(id.email ?? "");
            setWelcomeBack(id.name);
            console.log("[app] welcome back:", id.name);
          }
        } catch (err) {
          console.log("[app] identify skipped:", err);
        }
      } catch (err) {
        if (cancelled) return;
        console.error("Viam connection failed:", err);
        setViamError(
          `Viam connection failed: ${err instanceof Error ? err.message : String(err)}`
        );
      }
    }
    init();
    return () => {
      cancelled = true;
    };
  }, []);

  async function handleDrinkNext() {
    if (!selectedDrink) return;
    setDrinkRejection(null);

    if (selectedDrink === "espresso") {
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
    if (viamConn.current) {
      try {
        const drink = DRINKS.find((d) => d.id === selectedDrink);
        await prepareOrder(viamConn.current, {
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
      setDrinkRejection("Not connected to the robot yet.");
    }
  }

  function placeOrder(misspelledName: string) {
    console.log("[app] placing order for:", misspelledName);
    setMisspelled(misspelledName);
    setStep("confirmation");
    setShowTracker(true);

    // Enqueue the order — returns immediately with queue position.
    if (viamConn.current) {
      const drink = DRINKS.find((d) => d.id === selectedDrink);
      prepareOrder(viamConn.current, {
        drink: selectedDrink!,
        drinkLabel: drink?.label ?? selectedDrink!,
        customerName: misspelledName,
        pronunciation: undefined,
      }).catch((err) => console.error("prepare_order failed:", err));
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
        placeOrder(result.misspelled || name);
      }
    } catch (err) {
      console.error("Misspell error:", err);
      placeOrder(name);
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
  }, []);

  const handleTrackerEmpty = useCallback(() => {
    setShowTracker(false);
  }, []);

  // --- Render the left panel content based on step ---
  function renderStep() {
    if (step === "face-register") {
      return (
        <FaceRegister
          name={name}
          email={email}
          viamConn={viamConn.current}
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
          onNameChange={setName}
          onEmailChange={setEmail}
          onSubmit={handleSubmit}
        />
      );
    }

    if (step === "drink") {
      return (
        <ChooseDrink
          selectedDrink={selectedDrink}
          rejection={drinkRejection}
          onSelect={(id) => {
            setDrinkRejection(null);
            setSelectedDrink(id);
          }}
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
          className="anim-in-hero text-4xl font-mono font-bold text-neutral-900 mb-4"
          style={{ animationDelay: "500ms" }}
        >
          Hi, I&apos;m {machineName}
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

        <button
          onClick={() => setStep("drink")}
          className="anim-in-hero press px-20 py-4 text-lg font-medium bg-neutral-900 text-white rounded-full transition-colors hover:bg-neutral-800"
          style={{ animationDelay: welcomeBack ? "1400ms" : "1200ms" }}
        >
          Place an order
        </button>
      </main>
    );
  }

  return (
    <div className="h-dvh flex">
      {/* Left panel: ordering flow */}
      <div className="flex-1 min-w-0 relative transition-all duration-500">
        {renderStep()}
      </div>

      {/* Right panel: live order tracker */}
      {showTracker && (
        <div className="w-[340px] shrink-0 border-l border-neutral-200">
          <OrderTracker
            viamConn={viamConn.current}
            onEmpty={handleTrackerEmpty}
          />
        </div>
      )}
    </div>
  );
}
