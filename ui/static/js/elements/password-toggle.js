class PasswordToggle extends HTMLElement {
    onClick = (e) => {
        const button = e.target.closest("[data-toggle]");
        const input = this.querySelector("input");
        if (!button || !input)
            return;
        const show = input.type === "password";
        input.type = show ? "text" : "password";
        button.textContent = show ? "Hide" : "Show";
        button.setAttribute("aria-pressed", String(show));
    };
    connectedCallback() {
        this.addEventListener("click", this.onClick);
    }
    disconnectedCallback() {
        this.removeEventListener("click", this.onClick);
    }
}
customElements.define("password-toggle", PasswordToggle);
export {};
