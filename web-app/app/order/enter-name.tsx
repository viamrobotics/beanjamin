export function EnterName({
  name,
  email,
  loading,
  onNameChange,
  onEmailChange,
  onBack,
  onSubmit,
}: {
  name: string;
  email: string;
  loading: boolean;
  onNameChange: (name: string) => void;
  onEmailChange: (email: string) => void;
  onBack: () => void;
  onSubmit: () => void;
}) {
  return (
    <main className="relative h-full bg-white flex flex-col items-center justify-center p-4 font-sans">
      <button
        type="button"
        onClick={onBack}
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
      <div className="w-full max-w-[512px] flex flex-col gap-5">
        <h1 className="anim-in text-2xl font-semibold text-neutral-900 text-center">
          What&apos;s your name?
        </h1>

        <div className="flex flex-col gap-3">
          <input
            type="text"
            value={name}
            onChange={(e) => onNameChange(e.target.value)}
            onKeyDown={(e) => e.key === "Enter" && onSubmit()}
            placeholder="Your name"
            autoFocus
            className="anim-in w-full px-5 py-3 bg-neutral-50 border-2 border-neutral-200 rounded-2xl text-neutral-900 text-base outline-none focus:border-neutral-400 transition-colors font-sans"
            style={{ animationDelay: "80ms" }}
          />

          <input
            type="email"
            value={email}
            onChange={(e) => onEmailChange(e.target.value)}
            onKeyDown={(e) => e.key === "Enter" && onSubmit()}
            placeholder="Email (optional, for loyalty)"
            className="anim-in w-full px-5 py-3 bg-neutral-50 border-2 border-neutral-200 rounded-2xl text-neutral-900 text-base outline-none focus:border-neutral-400 transition-colors font-sans"
            style={{ animationDelay: "120ms" }}
          />
        </div>

        <button
          onClick={onSubmit}
          disabled={!name.trim() || loading}
          className="anim-in press w-full py-3 text-base font-medium bg-black text-white rounded-full hover:bg-neutral-800 transition-colors disabled:opacity-30"
          style={{ animationDelay: "200ms" }}
        >
          {loading ? "Placing order..." : "Place order"}
        </button>
      </div>
    </main>
  );
}
