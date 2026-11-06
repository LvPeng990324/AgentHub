import type { Config } from 'tailwindcss';

// md(768px) 是唯一主断点；语义色用 CSS 变量组织，便于 P2 追加 [data-theme=dark]。
export default {
  content: ['./index.html', './src/**/*.{js,ts,jsx,tsx}'],
  theme: {
    extend: {
      screens: {
        md: '768px',
      },
      colors: {
        surface: 'var(--surface)',
        'surface-muted': 'var(--surface-muted)',
        line: 'var(--border)',
        brand: 'var(--brand)',
        'brand-soft': 'var(--brand-soft)',
        ink: 'var(--text)',
        muted: 'var(--muted)',
        success: 'var(--success)',
        danger: 'var(--danger)',
        warning: 'var(--warning)',
      },
      fontFamily: {
        sans: [
          'Inter',
          '"PingFang SC"',
          '"Microsoft YaHei"',
          '"Noto Sans CJK SC"',
          'sans-serif',
        ],
      },
      borderRadius: {
        card: '8px',
      },
      fontSize: {
        '2xs': ['12px', '18px'],
        xs: ['13px', '20px'],
        sm: ['14px', '22px'],
        base: ['16px', '24px'],
      },
      keyframes: {
        fadeIn: {
          from: { opacity: '0' },
          to: { opacity: '1' },
        },
        blink: {
          '0%, 100%': { opacity: '1' },
          '50%': { opacity: '0.2' },
        },
      },
      animation: {
        'fade-in': 'fadeIn 200ms ease-out both',
        blink: 'blink 1s step-end infinite',
      },
    },
  },
  plugins: [],
} satisfies Config;
