/** @type {import('tailwindcss').Config} */
module.exports = {
  darkMode: 'class',
  content: [
    "../internal/web/**/*.templ",
    "../internal/web/**/*.go",
    "../internal/ui/**/*.templ",
    "../internal/ui/**/*.go",
    "!../internal/ui/components/staticmanifest.go",
  ],
  theme: {
    extend: {
      colors: {
        // Values live in css/input.css as RGB triplets, so one class on
        // <html> swaps the whole palette. See :root / html.light there.
        rw: {
          bg:       "rgb(var(--rw-bg) / <alpha-value>)",
          surface:  "rgb(var(--rw-surface) / <alpha-value>)",
          raised:   "rgb(var(--rw-raised) / <alpha-value>)",
          inset:    "rgb(var(--rw-inset) / <alpha-value>)",
          border:   "rgb(var(--rw-border) / <alpha-value>)",
          strong:   "rgb(var(--rw-strong) / <alpha-value>)",
          text:     "rgb(var(--rw-text) / <alpha-value>)",
          muted:    "rgb(var(--rw-muted) / <alpha-value>)",
          faint:    "rgb(var(--rw-faint) / <alpha-value>)",
          accent:   "rgb(var(--rw-accent) / <alpha-value>)",
          accentHi: "rgb(var(--rw-accentHi) / <alpha-value>)",
          accentDeep: "rgb(var(--rw-accentDeep) / <alpha-value>)",
          accentPress: "rgb(var(--rw-accentPress) / <alpha-value>)",
          success:  "rgb(var(--rw-success) / <alpha-value>)",
          danger:   "rgb(var(--rw-danger) / <alpha-value>)",
          warn:     "rgb(var(--rw-warn) / <alpha-value>)",
          // Alias: templates use both spellings; without this, every
          // rw-warning utility silently generates no CSS.
          warning:  "rgb(var(--rw-warn) / <alpha-value>)",
        },
      },
      fontFamily: {
        sans: ['ui-sans-serif', 'system-ui', '-apple-system', 'Segoe UI', 'Roboto', 'Inter', 'sans-serif'],
        mono: ['ui-monospace', 'SFMono-Regular', 'Menlo', 'Consolas', 'monospace'],
      },
      boxShadow: {
        'rw':      'var(--rw-shadow)',
        'rw-glow': 'var(--rw-glow)',
      },
    },
  },
  plugins: [],
}
