"use client";

import { useRouter } from "next/navigation";
import Image from "next/image";

export default function Home() {
  const router = useRouter();

  return (
    <main className="h-dvh bg-white flex flex-col items-center justify-center p-8 font-sans">
      <Image
        src="/beans.png"
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

      <button
        onClick={() => router.push("/order")}
        className="anim-in-hero press px-20 py-4 text-lg font-medium bg-neutral-900 text-white rounded-full transition-colors hover:bg-neutral-800"
        style={{ animationDelay: "1200ms" }}
      >
        Place an order
      </button>
    </main>
  );
}
