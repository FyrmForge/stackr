// <theme-toggle> flips the theme. <html> always carries exactly one of
// "dark" / "light": the v0 palette reads no class as follow-the-OS, and the
// unswept dark: utilities key on "dark", so a bare <html> would paint one
// page in two themes. The choice lives in localStorage "theme" ("light" |
// "dark"); storage can throw (private mode, blocked site data), so the
// toggle still works without it.
// ponytail: the server renders dark, so a light user sees dark until this
// module runs; an inline head script would fix it but ui-plan §6 bans them.
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

function apply(theme: string): void {
  const root = document.documentElement;
  root.classList.toggle("dark", theme === "dark");
  root.classList.toggle("light", theme !== "dark");
}

// At load, not in connectedCallback: signed-out pages have no toggle.
const initial = stored();
if (initial) apply(initial);

class ThemeToggle extends HTMLElement {
  private onClick = (e: Event): void => {
    if (!(e.target as Element).closest("button")) return;
    const theme = document.documentElement.classList.contains("dark") ? "light" : "dark";
    apply(theme);
    store(theme);
  };

  connectedCallback(): void {
    this.addEventListener("click", this.onClick);
  }

  disconnectedCallback(): void {
    this.removeEventListener("click", this.onClick);
  }
}

customElements.define("theme-toggle", ThemeToggle);
