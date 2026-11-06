import clsx from 'clsx';

interface StatusDotProps {
  online: boolean;
  label?: string;
  className?: string;
}

/** 在线绿点 / 离线灰点（含 aria-label，便于读屏）。 */
export function StatusDot({ online, label, className }: StatusDotProps) {
  const text = label ?? (online ? '在线' : '离线');
  return (
    <span
      className={clsx('inline-flex items-center gap-1.5 whitespace-nowrap text-xs', className)}
      aria-label={text}
      role="status"
    >
      <span
        aria-hidden="true"
        className={clsx(
          'inline-block h-2 w-2 shrink-0 rounded-full',
          online ? 'bg-success' : 'bg-slate-300',
        )}
      />
      <span className={online ? 'text-ink' : 'text-muted'}>{text}</span>
    </span>
  );
}
