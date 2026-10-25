// <confirm-dialog word="uploads"> guards a destructive action. It opens its
// <dialog> from [data-open], closes on [data-cancel], and on [data-confirm]
// fires a bubbling "confirmed" event; the wrapper's hx-trigger does the post.
// With a word set, [data-confirm] stays disabled until [data-word] matches.
// As a guided dialog (the add connector one), [data-next]/[data-back] show
// the next or previous [data-step] sibling; the server renders which one
// starts visible. A confirm nested inside keeps its clicks to itself.
class ConfirmDialog extends HTMLElement {
  private onClick = (e: Event): void => {
    const target = e.target as Element;
    const dialog = this.querySelector("dialog");
    if (!dialog || target.closest("confirm-dialog") !== this) return;
    const move = target.closest("[data-next], [data-back]");
    if (move) return this.step(move);
    if (target.closest("[data-open]")) {
      // a confirm inside a form asks only once the form's own fields are valid
      const bad = [...(this.closest("form")?.elements ?? [])].find(
        (f) => !this.contains(f) && !(f as HTMLInputElement).checkValidity?.(),
      ) as HTMLInputElement | undefined;
      if (bad) return void bad.reportValidity();
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
    if (!input.matches("[data-word]") || input.closest("confirm-dialog") !== this) return;
    const button = this.confirmButton();
    if (button) button.disabled = input.value !== this.word();
  };

  connectedCallback(): void {
    this.addEventListener("click", this.onClick);
    this.addEventListener("input", this.onInput);
    // open: the server answered with the question (a promote's plan).
    if (this.hasAttribute("open")) {
      this.reset();
      this.querySelector("dialog")?.showModal();
    }
  }

  disconnectedCallback(): void {
    this.removeEventListener("click", this.onClick);
    this.removeEventListener("input", this.onInput);
  }

  private step(button: Element): void {
    const at = button.closest<HTMLElement>("[data-step]");
    const to = button.hasAttribute("data-next") ? at?.nextElementSibling : at?.previousElementSibling;
    if (!at || !(to instanceof HTMLElement) || !to.hasAttribute("data-step")) return;
    at.hidden = true;
    to.hidden = false;
    to.querySelector<HTMLElement>("input:not([type=hidden]), button")?.focus();
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
