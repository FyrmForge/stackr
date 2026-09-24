/** @type {import('tailwindcss').Config} */
module.exports = {
  darkMode: 'class',
  content: [
    "../internal/web/**/*.templ",
    "../internal/web/**/*.go",
    "!../internal/web/components/staticmanifest.go",
  ],
  theme: {
    extend: {
      colors: {
        hamr: {
          metal: {
            dark: "#1F2326",
            mid: "#3A3F45",
            light: "#7A8188",
            steel: "#B8BEC6",
          },
          fire: {
            yellow: "#FFD166",
            orange: "#FF8C32",
            ember: "#FF5A1F",
            deep: "#C92E0A",
            glow: "#FFB347",
          },
        },
      },
    },
  },
  plugins: [],
}
