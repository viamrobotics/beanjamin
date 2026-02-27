"use client";

import { useState, useEffect } from "react";
import { DRINKS } from "./drinks";
import { ChooseDrink } from "./choose-drink";
import { EnterName } from "./enter-name";
import { Progress } from "./progress";
import { OrderResult } from "./order-result";

type Step = "drink" | "name" | "progress" | "result";

export default function OrderPage() {
  const [step, setStep] = useState<Step>("drink");
  const [name, setName] = useState("");
  const [selectedDrink, setSelectedDrink] = useState<string | null>("espresso");
  const [misspelled, setMisspelled] = useState("");
  const [pronunciation, setPronunciation] = useState("");
  const [loading, setLoading] = useState(false);
  const [queueCount, setQueueCount] = useState(1);
  useEffect(() => {
    setQueueCount(Math.floor(Math.random() * 5) + 1);
  }, []);

  async function handleSubmit() {
    if (!name.trim() || !selectedDrink) return;
    setLoading(true);

    try {
      const res = await fetch("/api/misspell", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ name: name.trim(), chaos: "medium" }),
      });
      const data = await res.json();
      setMisspelled(data.misspelled || name);
      setPronunciation(data.pronunciation || "");
      setStep("progress");
    } catch {
      setMisspelled(name);
      setStep("progress");
    } finally {
      setLoading(false);
    }
  }

  if (step === "progress") {
    return <Progress onComplete={() => setStep("result")} />;
  }

  if (step === "result") {
    const drink = DRINKS.find((d) => d.id === selectedDrink);
    return (
      <OrderResult
        misspelled={misspelled}
        actualName={name}
        drinkLabel={drink?.label ?? ""}
        queueCount={queueCount}
        onReset={() => {
          setStep("drink");
          setName("");
          setSelectedDrink(null);
        }}
      />
    );
  }

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

  return (
    <ChooseDrink
      selectedDrink={selectedDrink}
      queueCount={queueCount}
      onSelect={setSelectedDrink}
      onNext={() => selectedDrink && setStep("name")}
    />
  );
}
