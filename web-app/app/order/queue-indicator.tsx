export function QueueIndicator({ count }: { count: number }) {
  return (
    <div className="anim-fade absolute top-8 right-8" style={{ animationDelay: "300ms" }}>
      <div className="w-10 h-10 rounded-full bg-neutral-100 flex items-center justify-center">
        <span className="font-mono text-xs font-medium text-neutral-500">
          {count}
        </span>
      </div>
    </div>
  );
}
