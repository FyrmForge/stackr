const DISMISS_MS = 5000;
class FlashToast extends HTMLElement {
    timer = 0;
    onClick = () => this.dismiss();
    onResponseError = (e) => {
        const xhr = e.detail.xhr;
        this.show("error", `Request failed: ${xhr.status} ${xhr.statusText}`.trim());
    };
    onSendError = () => {
        this.show("error", "The server did not answer. Check your connection and try again.");
    };
    connectedCallback() {
        this.addEventListener("click", this.onClick);
        document.addEventListener("htmx:responseError", this.onResponseError);
        document.addEventListener("htmx:sendError", this.onSendError);
        this.arm();
    }
    disconnectedCallback() {
        window.clearTimeout(this.timer);
        this.removeEventListener("click", this.onClick);
        document.removeEventListener("htmx:responseError", this.onResponseError);
        document.removeEventListener("htmx:sendError", this.onSendError);
    }
    show(kind, message) {
        this.setAttribute("kind", kind);
        this.textContent = message;
        this.arm();
    }
    arm() {
        window.clearTimeout(this.timer);
        if (this.textContent?.trim() === "" || this.getAttribute("kind") === "error")
            return;
        this.timer = window.setTimeout(() => this.dismiss(), DISMISS_MS);
    }
    dismiss() {
        window.clearTimeout(this.timer);
        this.textContent = "";
    }
}
customElements.define("flash-toast", FlashToast);
export {};
