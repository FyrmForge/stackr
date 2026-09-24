// <theme-toggle> flips <html class="dark">. The choice lives in
// localStorage "theme" ("light" | "dark"); storage can throw (private
// mode, blocked site data), so the toggle still works without it.
// ponytail: the server renders dark, so a light user sees dark until this
// module runs; an inline head script would fix it but the CSP has none.
const KEY = "theme";

function stored(): string | null {
  try {
    return localStorage.getItem(KEY);
  } catch {
    return null;
  }
}

function store(theme: string): void {
  try {
    localStorage.setItem(KEY, theme);
  } catch {
    // Not stored: the choice lasts until the next full page load.
  }
}

class ThemeToggle extends HTMLElement {
  private onClick = (e: Event): void => {
    if (!(e.target as Element).closest("button")) return;
    const dark = document.documentElement.classList.toggle("dark");
    store(dark ? "dark" : "light");
  };

  connectedCallback(): void {
    const theme = stored();
    if (theme) document.documentElement.classList.toggle("dark", theme === "dark");
    this.addEventListener("click", this.onClick);
  }

  disconnectedCallback(): void {
    this.removeEventListener("click", this.onClick);
  }
}

customElements.define("theme-toggle", ThemeToggle);
