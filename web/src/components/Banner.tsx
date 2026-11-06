import clsx from 'clsx';
import { AlertTriangle, CheckCircle2, Info, WifiOff, XCircle } from 'lucide-react';
import type { ReactNode } from 'react';

type BannerVariant = 'offline' | 'error' | 'warning' | 'info' | 'success';

const VARIANT_STYLE: Record<BannerVariant, { wrap: string; icon: typeof Info }> = {
  offline: { wrap: 'border-amber-200 bg-amber-50 text-amber-900', icon: WifiOff },
  error: { wrap: 'border-red-200 bg-red-50 text-red-900', icon: XCircle },
  warning: { wrap: 'border-orange-200 bg-orange-50 text-orange-900', icon: AlertTriangle },
  info: { wrap: 'border-indigo-200 bg-indigo-50 text-indigo-900', icon: Info },
  success: { wrap: 'border-emerald-200 bg-emerald-50 text-emerald-900', icon: CheckCircle2 },
};

interface BannerProps {
  variant?: BannerVariant;
  title?: string;
  children?: ReactNode;
  action?: ReactNode;
  className?: string;
}

/** 离线 / 错误 / 提示横幅（R-10 / R-13 / R-18）。 */
export function Banner({ variant = 'info', title, children, action, className }: BannerProps) {
  const style = VARIANT_STYLE[variant];
  const Icon = style.icon;
  return (
    <div
      role="alert"
      className={clsx(
        'flex flex-wrap items-center gap-2 rounded-card border px-3 py-2 text-xs animate-fade-in',
        style.wrap,
        className,
      )}
    >
      <Icon size={16} className="shrink-0" aria-hidden="true" />
      <div className="min-w-0 flex-1">
        {title && <p className="font-medium">{title}</p>}
        {children && <div className="break-words">{children}</div>}
      </div>
      {action && <div className="shrink-0">{action}</div>}
    </div>
  );
}
