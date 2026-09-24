const KEY = "theme";
function stored() {
    try {
        return localStorage.getItem(KEY);
    }
    catch {
        return null;
    }
}
function store(theme) {
    try {
        localStorage.setItem(KEY, theme);
    }
    catch {
    }
}
function apply(theme) {
    const root = document.documentElement;
    root.classList.toggle("dark", theme === "dark");
    root.classList.toggle("light", theme !== "dark");
}
const initial = stored();
if (initial)
    apply(initial);
class ThemeToggle extends HTMLElement {
    onClick = (e) => {
        if (!e.target.closest("button"))
            return;
        const theme = document.documentElement.classList.contains("dark") ? "light" : "dark";
        apply(theme);
        store(theme);
    };
    connectedCallback() {
        this.addEventListener("click", this.onClick);
    }
    disconnectedCallback() {
        this.removeEventListener("click", this.onClick);
    }
}
customElements.define("theme-toggle", ThemeToggle);
export {};
