// <confirm-dialog word="uploads"> guards a destructive action. It opens its
// <dialog> from [data-open], closes on [data-cancel], and on [data-confirm]
// fires a bubbling "confirmed" event; the wrapper's hx-trigger does the post.
// With a word set, [data-confirm] stays disabled until [data-word] matches.
class ConfirmDialog extends HTMLElement {
  private onClick = (e: Event): void => {
    const target = e.target as Element;
    const dialog = this.querySelector("dialog");
    if (!dialog) return;
    if (target.closest("[data-open]")) {
      this.reset();
      dialog.showModal();
      this.querySelector<HTMLInputElement>("[data-word]")?.focus();
    } else if (target.closest("[data-cancel]")) {
      dialog.close();
    } else if (target.closest("[data-confirm]")) {
      if (this.confirmButton()?.disabled) return;
      dialog.close();
      this.dispatchEvent(new CustomEvent("confirmed", { bubbles: true }));
    }
  };

  private onInput = (e: Event): void => {
    const input = e.target as HTMLInputElement;
    if (!input.matches("[data-word]")) return;
    const button = this.confirmButton();
    if (button) button.disabled = input.value !== this.word();
  };

  connectedCallback(): void {
    this.addEventListener("click", this.onClick);
    this.addEventListener("input", this.onInput);
  }

  disconnectedCallback(): void {
    this.removeEventListener("click", this.onClick);
    this.removeEventListener("input", this.onInput);
  }

  private word(): string {
    return this.getAttribute("word") ?? "";
  }

  private confirmButton(): HTMLButtonElement | null {
    return this.querySelector<HTMLButtonElement>("[data-confirm]");
  }

  // Every open starts blank: a typed word never carries to the next open.
  private reset(): void {
    const input = this.querySelector<HTMLInputElement>("[data-word]");
    if (input) input.value = "";
    const button = this.confirmButton();
    if (button) button.disabled = this.word() !== "";
  }
}

customElements.define("confirm-dialog", ConfirmDialog);
