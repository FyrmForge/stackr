const DISMISS_MS = 4000;
const FADE_MS = 500;
const RECONNECT_MS = 10000;
const RECONNECTING = "Reconnecting\u2026";
function background(d) {
    const t = d.elt?.closest("[hx-trigger]")?.getAttribute("hx-trigger") ?? "";
    return /\bevery\b/.test(t);
}
class FlashToast extends HTMLElement {
    timer = 0;
    failingSince = 0;
    reconnecting = false;
    onClick = () => this.dismiss();
    onResponseError = (e) => {
        if (e.defaultPrevented)
            return;
        const d = e.detail;
        if (background(d))
            return this.backgroundFailed();
        this.show("error", `Request failed: ${d.xhr.status} ${d.xhr.statusText}`.trim());
    };
    onSendError = (e) => {
        if (background(e.detail))
            return this.backgroundFailed();
        this.show("error", "The server did not answer. Check your connection and try again.");
    };
    onAfterRequest = (e) => {
        const d = e.detail;
        if (!d.successful || !background(d))
            return;
        this.failingSince = 0;
        if (this.reconnecting) {
            this.reconnecting = false;
            this.dismiss();
        }
    };
    backgroundFailed() {
        const now = Date.now();
        if (!this.failingSince)
            this.failingSince = now;
        if (this.reconnecting || now - this.failingSince < RECONNECT_MS)
            return;
        this.reconnecting = true;
        this.show("warning", RECONNECTING);
    }
    connectedCallback() {
        this.addEventListener("click", this.onClick);
        document.addEventListener("htmx:responseError", this.onResponseError);
        document.addEventListener("htmx:sendError", this.onSendError);
        document.addEventListener("htmx:afterRequest", this.onAfterRequest);
        this.arm();
    }
    disconnectedCallback() {
        window.clearTimeout(this.timer);
        this.removeEventListener("click", this.onClick);
        document.removeEventListener("htmx:responseError", this.onResponseError);
        document.removeEventListener("htmx:sendError", this.onSendError);
        document.removeEventListener("htmx:afterRequest", this.onAfterRequest);
    }
    show(kind, message) {
        this.removeAttribute("leaving");
        this.setAttribute("kind", kind);
        this.textContent = message;
        this.arm();
    }
    arm() {
        window.clearTimeout(this.timer);
        if (this.textContent?.trim() === "" || this.getAttribute("kind") === "error" || this.reconnecting)
            return;
        this.timer = window.setTimeout(() => this.dismiss(), DISMISS_MS);
    }
    dismiss() {
        window.clearTimeout(this.timer);
        this.setAttribute("leaving", "");
        this.timer = window.setTimeout(() => {
            this.textContent = "";
            this.removeAttribute("leaving");
        }, FADE_MS);
    }
}
customElements.define("flash-toast", FlashToast);
export {};
