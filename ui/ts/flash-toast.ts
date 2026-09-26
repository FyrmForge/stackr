// <flash-toast kind="success|warning|error"> shows one message. The server
// fills it (full page or out-of-band swap); htmx request failures fill it
// here. A click dismisses it; anything but an error dismisses itself after
// 4s (v0's timing), fading out over FADE_MS through [leaving].
// Empty = hidden (CSS :empty).
const DISMISS_MS = 4000;
const FADE_MS = 500;

class FlashToast extends HTMLElement {
  private timer = 0;

  private onClick = (): void => this.dismiss();

  private onResponseError = (e: Event): void => {
    const xhr = (e as CustomEvent<{ xhr: { status: number; statusText: string } }>).detail.xhr;
    this.show("error", `Request failed: ${xhr.status} ${xhr.statusText}`.trim());
  };

  private onSendError = (): void => {
    this.show("error", "The server did not answer. Check your connection and try again.");
  };

  connectedCallback(): void {
    this.addEventListener("click", this.onClick);
    document.addEventListener("htmx:responseError", this.onResponseError);
    document.addEventListener("htmx:sendError", this.onSendError);
    this.arm();
  }

  disconnectedCallback(): void {
    window.clearTimeout(this.timer);
    this.removeEventListener("click", this.onClick);
    document.removeEventListener("htmx:responseError", this.onResponseError);
    document.removeEventListener("htmx:sendError", this.onSendError);
  }

  private show(kind: string, message: string): void {
    this.removeAttribute("leaving");
    this.setAttribute("kind", kind);
    this.textContent = message;
    this.arm();
  }

  private arm(): void {
    window.clearTimeout(this.timer);
    if (this.textContent?.trim() === "" || this.getAttribute("kind") === "error") return;
    this.timer = window.setTimeout(() => this.dismiss(), DISMISS_MS);
  }

  private dismiss(): void {
    window.clearTimeout(this.timer);
    this.setAttribute("leaving", "");
    this.timer = window.setTimeout(() => {
      this.textContent = "";
      this.removeAttribute("leaving");
    }, FADE_MS);
  }
}

customElements.define("flash-toast", FlashToast);
