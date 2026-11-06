import clsx from 'clsx';
import { AlertTriangle } from 'lucide-react';
import { useEffect, type ReactNode } from 'react';

interface ConfirmDialogProps {
  open: boolean;
  title: string;
  description: ReactNode;
  confirmText?: string;
  cancelText?: string;
  danger?: boolean;
  busy?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}

/**
 * 二次确认：桌面居中 dialog；<md 改底部 Sheet（圆角 + safe-area + 危险色确认）。
 * 取消注册的文案由 DiscoverPage 传入，必须明写「清空会话数据、不可恢复」（R-06）。
 */
export function ConfirmDialog({
  open,
  title,
  description,
  confirmText = '确认',
  cancelText = '取消',
  danger = false,
  busy = false,
  onConfirm,
  onCancel,
}: ConfirmDialogProps) {
  useEffect(() => {
    if (!open) return undefined;
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape' && !busy) onCancel();
    };
    document.addEventListener('keydown', onKeyDown);
    return () => document.removeEventListener('keydown', onKeyDown);
  }, [open, busy, onCancel]);

  if (!open) return null;

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4 max-md:items-end max-md:p-0"
      role="dialog"
      aria-modal="true"
      aria-label={title}
    >
      <button
        type="button"
        className="absolute inset-0 h-full w-full cursor-default"
        onClick={() => {
          if (!busy) onCancel();
        }}
        aria-label="关闭确认弹窗"
      />
      <div className="ah-safe-bottom relative w-full max-w-md rounded-card border border-line bg-surface p-5 shadow-xl animate-fade-in max-md:rounded-b-none max-md:rounded-t-2xl">
        <div className="flex items-start gap-3">
          {danger && (
            <span className="mt-0.5 flex h-9 w-9 shrink-0 items-center justify-center rounded-full bg-red-50 text-danger">
              <AlertTriangle size={18} aria-hidden="true" />
            </span>
          )}
          <div className="min-w-0 flex-1">
            <h2 className="text-sm font-semibold text-ink">{title}</h2>
            <div className="mt-2 text-xs leading-relaxed text-muted">{description}</div>
          </div>
        </div>
        <div className="mt-5 flex flex-row-reverse gap-2">
          <button
            type="button"
            onClick={onConfirm}
            disabled={busy}
            className={clsx(
              'tap-target min-w-[96px] rounded-card px-4 text-sm font-medium text-white disabled:opacity-60',
              danger ? 'bg-danger hover:bg-red-700' : 'bg-brand hover:bg-indigo-700',
            )}
          >
            {busy ? '处理中…' : confirmText}
          </button>
          <button
            type="button"
            onClick={onCancel}
            disabled={busy}
            className="tap-target min-w-[96px] rounded-card border border-line bg-surface px-4 text-sm text-ink disabled:opacity-60"
          >
            {cancelText}
          </button>
        </div>
      </div>
    </div>
  );
}
