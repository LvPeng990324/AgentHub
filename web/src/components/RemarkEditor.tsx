import clsx from 'clsx';
import { Check, Pencil, X } from 'lucide-react';
import { useEffect, useState } from 'react';

import { toErrorMessage } from '../api/client';
import { MAX_REMARK_LENGTH } from '../api/types';

interface RemarkEditorProps {
  value: string;
  onSave: (remark: string) => Promise<void>;
  label?: string;
  placeholder?: string;
  className?: string;
}

/** 设备 / agent 共用的就地备注编辑（失焦保存、Enter 保存、Esc 取消、≤500 字）。 */
export function RemarkEditor({
  value,
  onSave,
  label = '备注',
  placeholder = '添加备注…',
  className,
}: RemarkEditorProps) {
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState(value);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!editing) setDraft(value);
  }, [value, editing]);

  const save = async () => {
    if (saving) return;
    if (draft === value) {
      setEditing(false);
      setError(null);
      return;
    }
    setSaving(true);
    setError(null);
    try {
      await onSave(draft);
      setEditing(false);
    } catch (caught) {
      setError(toErrorMessage(caught));
    } finally {
      setSaving(false);
    }
  };

  if (!editing) {
    return (
      <div className={clsx('flex min-w-0 items-center gap-1', className)}>
        <span className="min-w-0 truncate text-xs text-muted" title={value}>
          {value || placeholder}
        </span>
        <button
          type="button"
          onClick={() => {
            setDraft(value);
            setError(null);
            setEditing(true);
          }}
          className="tap-target flex shrink-0 items-center justify-center rounded-card px-1 text-muted hover:text-brand"
          aria-label={`编辑${label}`}
          title={`编辑${label}`}
        >
          <Pencil size={14} aria-hidden="true" />
        </button>
      </div>
    );
  }

  return (
    <div className={clsx('flex min-w-0 flex-col gap-1', className)}>
      <div className="flex min-w-0 items-center gap-1">
        <input
          autoFocus
          value={draft}
          maxLength={MAX_REMARK_LENGTH}
          placeholder={placeholder}
          aria-label={label}
          onChange={(event) => setDraft(event.target.value)}
          onBlur={() => void save()}
          onKeyDown={(event) => {
            if (event.key === 'Enter') {
              event.preventDefault();
              void save();
            } else if (event.key === 'Escape') {
              setDraft(value);
              setEditing(false);
              setError(null);
            }
          }}
          className="min-h-[44px] min-w-0 flex-1 rounded-card border border-line bg-surface px-2 text-sm text-ink"
        />
        <button
          type="button"
          onMouseDown={(event) => event.preventDefault()}
          onClick={() => void save()}
          disabled={saving}
          className="tap-target flex shrink-0 items-center justify-center rounded-card text-success disabled:opacity-50"
          aria-label={`保存${label}`}
        >
          <Check size={18} aria-hidden="true" />
        </button>
        <button
          type="button"
          onMouseDown={(event) => event.preventDefault()}
          onClick={() => {
            setDraft(value);
            setEditing(false);
            setError(null);
          }}
          className="tap-target flex shrink-0 items-center justify-center rounded-card text-muted"
          aria-label={`取消编辑${label}`}
        >
          <X size={18} aria-hidden="true" />
        </button>
      </div>
      {error && <p className="text-2xs text-danger">{error}</p>}
      <p className="text-2xs text-muted">
        {draft.length}/{MAX_REMARK_LENGTH} 字 · 失焦或回车保存
      </p>
    </div>
  );
}
