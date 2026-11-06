import clsx from 'clsx';
import { Search, X } from 'lucide-react';

export interface FilterOption {
  value: string;
  label: string;
}

export interface FilterGroup {
  key: string;
  label: string;
  value: string;
  options: FilterOption[];
  onChange: (value: string) => void;
}

interface SearchFilterBarProps {
  q: string;
  onQueryChange: (value: string) => void;
  groups?: FilterGroup[];
  placeholder?: string;
  className?: string;
}

/** 搜索框 + 横向滚动筛选芯片（R-15 / 移动端 §6.7）。 */
export function SearchFilterBar({
  q,
  onQueryChange,
  groups = [],
  placeholder = '搜索名称 / 备注',
  className,
}: SearchFilterBarProps) {
  return (
    <div className={clsx('flex min-w-0 flex-col gap-2', className)}>
      <div className="relative min-w-0">
        <Search
          size={16}
          className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-muted"
          aria-hidden="true"
        />
        <input
          type="search"
          value={q}
          onChange={(event) => onQueryChange(event.target.value)}
          placeholder={placeholder}
          aria-label={placeholder}
          className="min-h-[44px] w-full rounded-card border border-line bg-surface pl-9 pr-10 text-sm text-ink placeholder:text-muted"
        />
        {q && (
          <button
            type="button"
            onClick={() => onQueryChange('')}
            className="tap-target absolute right-0 top-1/2 flex -translate-y-1/2 items-center justify-center text-muted"
            aria-label="清空搜索"
          >
            <X size={16} aria-hidden="true" />
          </button>
        )}
      </div>

      {groups.length > 0 && (
        <div className="ah-scroll flex min-w-0 gap-4 overflow-x-auto pb-1">
          {groups.map((group) => (
            <div key={group.key} className="flex shrink-0 items-center gap-1.5">
              <span className="shrink-0 text-2xs text-muted">{group.label}</span>
              <div className="flex shrink-0 gap-1">
                {[{ value: '', label: '全部' }, ...group.options].map((option) => (
                  <button
                    key={option.value}
                    type="button"
                    onClick={() => group.onChange(option.value)}
                    className={clsx(
                      'min-h-[44px] whitespace-nowrap rounded-full border px-3 text-xs',
                      group.value === option.value
                        ? 'border-brand bg-brand-soft font-medium text-brand'
                        : 'border-line bg-surface text-muted hover:text-ink',
                    )}
                    aria-pressed={group.value === option.value}
                  >
                    {option.label}
                  </button>
                ))}
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
