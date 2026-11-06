import clsx from 'clsx';

interface SpinnerProps {
  label?: string;
  className?: string;
}

/** 轻量加载指示（150–250ms 淡入）。 */
export function Spinner({ label = '加载中…', className }: SpinnerProps) {
  return (
    <div
      className={clsx('flex items-center justify-center gap-2 py-8 text-xs text-muted animate-fade-in', className)}
      role="status"
      aria-live="polite"
    >
      <span
        aria-hidden="true"
        className="inline-block h-4 w-4 animate-spin rounded-full border-2 border-line border-t-brand"
      />
      <span>{label}</span>
    </div>
  );
}
