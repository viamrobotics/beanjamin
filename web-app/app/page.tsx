"use client";

import { useState, useEffect, useRef } from "react";
import Image from "next/image";
import { DRINKS } from "./order/drinks";
import { ChooseDrink } from "./order/choose-drink";
import { EnterName } from "./order/enter-name";
import { Progress } from "./order/progress";
import { OrderResult } from "./order/order-result";
import {
  connectToViam,
  getMachineMetadataKey,
  prepareOrder,
  type ViamConnection,
} from "./lib/viamClient";
import { misspellName } from "./lib/misspell";

type Step = "welcome" | "drink" | "name" | "progress" | "result";

export default function Home() {
  const [step, setStep] = useState<Step>("welcome");
  const [name, setName] = useState("");
  const [selectedDrink, setSelectedDrink] = useState<string | null>("espresso");
  const [misspelled, setMisspelled] = useState("");
  const [loading, setLoading] = useState(false);
  const [queueCount, setQueueCount] = useState(1);
  const [drinkRejection, setDrinkRejection] = useState<string | null>(null);

  // Viam connection state
  const viamConn = useRef<ViamConnection | null>(null);
  const anthropicKey = useRef<string>("");
  const [viamError, setViamError] = useState<string | null>(null);

  useEffect(() => {
    setQueueCount(Math.floor(Math.random() * 5) + 1);
  }, []);

  // Connect to Viam on mount and fetch Anthropic API key from machine metadata
  useEffect(() => {
    let cancelled = false;
    async function init() {
      try {
        const conn = await connectToViam();
        if (cancelled) return;
        viamConn.current = conn;

        const key = await getMachineMetadataKey(conn, "anthropic_api_key");
        if (cancelled) return;
        if (!key) {
          setViamError(
            'Machine metadata missing "anthropic_api_key". Set it in the Viam app.'
          );
          return;
        }
        anthropicKey.current = key;
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
        // The error message from the Go module looks like:
        // 'unsupported drink "latte": A latte? Bold request...'
        const msg = err instanceof Error ? err.message : String(err);
        const colonIdx = msg.indexOf(": ");
        setDrinkRejection(colonIdx >= 0 ? msg.slice(colonIdx + 2) : msg);
      }
    } else {
      setDrinkRejection("Not connected to the robot yet.");
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
      setStep("progress");

      // Fire-and-forget: tell the robot to make coffee + announce via speech.
      // This runs in parallel with the progress animation.
      if (viamConn.current) {
        const drink = DRINKS.find((d) => d.id === selectedDrink);
        prepareOrder(viamConn.current, {
          drink: selectedDrink,
          drinkLabel: drink?.label ?? selectedDrink,
          customerName: result.misspelled || name,
          pronunciation: result.pronunciation,
        }).catch((err) => console.error("prepare_order failed:", err));
      }
    } catch (err) {
      console.error("Misspell error:", err);
      setMisspelled(name);
      setStep("progress");
    } finally {
      setLoading(false);
    }
  }

  // --- Welcome screen ---
  if (step === "welcome") {
    return (
      <main className="h-dvh bg-white flex flex-col items-center justify-center p-8 font-sans">
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
          Hi, I&apos;m Cappuccina
        </h1>
        <p
          className="anim-in-hero text-neutral-500 text-center mb-10 text-lg"
          style={{ animationDelay: "800ms" }}
        >
          Freshly brewed coffee, questionable spelling
        </p>

        {viamError && (
          <p className="text-red-500 text-sm text-center mb-4 max-w-md">
            {viamError}
          </p>
        )}

        <button
          onClick={() => setStep("drink")}
          className="anim-in-hero press px-20 py-4 text-lg font-medium bg-neutral-900 text-white rounded-full transition-colors hover:bg-neutral-800"
          style={{ animationDelay: "1200ms" }}
        >
          Place an order
        </button>
      </main>
    );
  }

  // --- Progress screen ---
  if (step === "progress") {
    return <Progress onComplete={() => setStep("result")} />;
  }

  // --- Result screen ---
  if (step === "result") {
    const drink = DRINKS.find((d) => d.id === selectedDrink);
    return (
      <OrderResult
        misspelled={misspelled}
        actualName={name}
        drinkLabel={drink?.label ?? ""}
        queueCount={queueCount}
        onReset={() => {
          setStep("welcome");
          setName("");
          setSelectedDrink("espresso");
          setQueueCount(Math.floor(Math.random() * 5) + 1);
        }}
      />
    );
  }

  // --- Enter name screen ---
  if (step === "name") {
    return (
      <EnterName
        name={name}
        loading={loading}
        queueCount={queueCount}
        onNameChange={setName}
        onSubmit={handleSubmit}
      />
    );
  }

  // --- Choose drink screen ---
  return (
    <ChooseDrink
      selectedDrink={selectedDrink}
      queueCount={queueCount}
      rejection={drinkRejection}
      onSelect={(id) => {
        setDrinkRejection(null);
        setSelectedDrink(id);
      }}
      onNext={handleDrinkNext}
    />
  );
}
