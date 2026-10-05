// <password-toggle> wraps a password <input> and its [data-toggle] button;
// the button flips the input between hidden and plain text.
class PasswordToggle extends HTMLElement {
  private onClick = (e: Event): void => {
    const button = (e.target as Element).closest<HTMLButtonElement>("[data-toggle]");
    const input = this.querySelector("input");
    if (!button || !input) return;
    const show = input.type === "password";
    input.type = show ? "text" : "password";
    button.textContent = show ? "Hide" : "Show";
    button.setAttribute("aria-pressed", String(show));
  };

  connectedCallback(): void {
    this.addEventListener("click", this.onClick);
  }

  disconnectedCallback(): void {
    this.removeEventListener("click", this.onClick);
  }
}

customElements.define("password-toggle", PasswordToggle);
