import clsx from 'clsx';

import { AGENT_TYPE, AGENT_TYPE_LABELS } from '../api/types';

/** 9 种 Agent 类型的彩色徽标映射（§8.3）。 */
const TYPE_CLASS: Record<string, string> = {
  [AGENT_TYPE.CLAUDE]: 'bg-orange-50 text-orange-700 border-orange-200',
  [AGENT_TYPE.CODEBUDDY]: 'bg-indigo-50 text-indigo-700 border-indigo-200',
  [AGENT_TYPE.QWEN]: 'bg-violet-50 text-violet-700 border-violet-200',
  [AGENT_TYPE.CURSOR]: 'bg-sky-50 text-sky-700 border-sky-200',
  [AGENT_TYPE.HERMES]: 'bg-emerald-50 text-emerald-700 border-emerald-200',
  [AGENT_TYPE.KIMI]: 'bg-pink-50 text-pink-700 border-pink-200',
  [AGENT_TYPE.CODEX]: 'bg-teal-50 text-teal-700 border-teal-200',
  [AGENT_TYPE.OPENCODE]: 'bg-amber-50 text-amber-700 border-amber-200',
  [AGENT_TYPE.OPENCLAW]: 'bg-rose-50 text-rose-700 border-rose-200',
};

const FALLBACK_CLASS = 'bg-slate-50 text-slate-700 border-slate-200';

interface TypeBadgeProps {
  type: string;
  displayName?: string;
  className?: string;
}

export function TypeBadge({ type, displayName, className }: TypeBadgeProps) {
  return (
    <span
      className={clsx(
        'inline-flex max-w-full items-center truncate rounded border px-2 py-0.5 text-2xs font-medium',
        TYPE_CLASS[type] ?? FALLBACK_CLASS,
        className,
      )}
      title={type}
    >
      {displayName || AGENT_TYPE_LABELS[type] || type}
    </span>
  );
}
