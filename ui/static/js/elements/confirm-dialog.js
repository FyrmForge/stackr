class ConfirmDialog extends HTMLElement {
    onClick = (e) => {
        const target = e.target;
        const dialog = this.querySelector("dialog");
        if (!dialog || target.closest("confirm-dialog") !== this)
            return;
        const move = target.closest("[data-next], [data-back]");
        if (move)
            return this.step(move);
        if (target.closest("[data-open]")) {
            this.reset();
            dialog.showModal();
            this.querySelector("[data-word]")?.focus();
        }
        else if (target.closest("[data-cancel]")) {
            dialog.close();
        }
        else if (target.closest("[data-confirm]")) {
            if (this.confirmButton()?.disabled)
                return;
            dialog.close();
            this.dispatchEvent(new CustomEvent("confirmed", { bubbles: true }));
        }
    };
    onInput = (e) => {
        const input = e.target;
        if (!input.matches("[data-word]") || input.closest("confirm-dialog") !== this)
            return;
        const button = this.confirmButton();
        if (button)
            button.disabled = input.value !== this.word();
    };
    connectedCallback() {
        this.addEventListener("click", this.onClick);
        this.addEventListener("input", this.onInput);
        if (this.hasAttribute("open")) {
            this.reset();
            this.querySelector("dialog")?.showModal();
        }
    }
    disconnectedCallback() {
        this.removeEventListener("click", this.onClick);
        this.removeEventListener("input", this.onInput);
    }
    step(button) {
        const at = button.closest("[data-step]");
        const to = button.hasAttribute("data-next") ? at?.nextElementSibling : at?.previousElementSibling;
        if (!at || !(to instanceof HTMLElement) || !to.hasAttribute("data-step"))
            return;
        at.hidden = true;
        to.hidden = false;
        to.querySelector("input:not([type=hidden]), button")?.focus();
    }
    word() {
        return this.getAttribute("word") ?? "";
    }
    confirmButton() {
        return this.querySelector("[data-confirm]");
    }
    reset() {
        const input = this.querySelector("[data-word]");
        if (input)
            input.value = "";
        const button = this.confirmButton();
        if (button)
            button.disabled = this.word() !== "";
    }
}
customElements.define("confirm-dialog", ConfirmDialog);
export {};
