export function EnterName({
  name,
  email,
  loading,
  onNameChange,
  onEmailChange,
  onSubmit,
}: {
  name: string;
  email: string;
  loading: boolean;
  onNameChange: (name: string) => void;
  onEmailChange: (email: string) => void;
  onSubmit: () => void;
}) {
  return (
    <main className="relative h-full bg-white flex flex-col items-center justify-center p-8 font-sans">
      <div className="w-full max-w-[512px] flex flex-col gap-10">
        <h1 className="anim-in text-2xl font-semibold text-neutral-900 text-center">
          What&apos;s your name?
        </h1>

        <div className="flex flex-col gap-4">
          <input
            type="text"
            value={name}
            onChange={(e) => onNameChange(e.target.value)}
            onKeyDown={(e) => e.key === "Enter" && onSubmit()}
            placeholder="Your name"
            autoFocus
            className="anim-in w-full px-5 py-4 bg-neutral-50 border-2 border-neutral-200 rounded-2xl text-neutral-900 text-base outline-none focus:border-neutral-400 transition-colors font-sans"
            style={{ animationDelay: "80ms" }}
          />

          <input
            type="email"
            value={email}
            onChange={(e) => onEmailChange(e.target.value)}
            onKeyDown={(e) => e.key === "Enter" && onSubmit()}
            placeholder="Email (optional, for loyalty)"
            className="anim-in w-full px-5 py-4 bg-neutral-50 border-2 border-neutral-200 rounded-2xl text-neutral-900 text-base outline-none focus:border-neutral-400 transition-colors font-sans"
            style={{ animationDelay: "120ms" }}
          />
        </div>

        <button
          onClick={onSubmit}
          disabled={!name.trim() || loading}
          className="anim-in press w-full py-5 text-base font-medium bg-black text-white rounded-full hover:bg-neutral-800 transition-colors disabled:opacity-30"
          style={{ animationDelay: "200ms" }}
        >
          {loading ? "Placing order..." : "Place order"}
        </button>
      </div>
    </main>
  );
}
