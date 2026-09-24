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
class ThemeToggle extends HTMLElement {
    onClick = (e) => {
        if (!e.target.closest("button"))
            return;
        const dark = document.documentElement.classList.toggle("dark");
        store(dark ? "dark" : "light");
    };
    connectedCallback() {
        const theme = stored();
        if (theme)
            document.documentElement.classList.toggle("dark", theme === "dark");
        this.addEventListener("click", this.onClick);
    }
    disconnectedCallback() {
        this.removeEventListener("click", this.onClick);
    }
}
customElements.define("theme-toggle", ThemeToggle);
export {};
