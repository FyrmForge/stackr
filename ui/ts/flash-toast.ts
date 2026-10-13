// <flash-toast kind="success|warning|error"> shows one message. The server
// fills it (full page or out-of-band swap); htmx request failures fill it
// here, unless the open drawer took one (defaultPrevented). A click
// dismisses it; anything but an error dismisses itself after 4s (v0's
// timing), fading out over FADE_MS through [leaving].
// Empty = hidden (CSS :empty).
// A background request (an `every` poll) that fails says nothing: it
// retries on its own. Only when polls keep failing for RECONNECT_MS does
// one "Reconnecting..." warning show; the next
// poll that works clears it. A panel restart is a few seconds of 502s.
const DISMISS_MS = 4000;
const FADE_MS = 500;
const RECONNECT_MS = 10000;
const RECONNECTING = "Reconnecting\u2026";

type ReqDetail = { elt?: Element; successful?: boolean };

// background: the request comes from an `every` poll. Polls and `load`
// triggers both start without a user event, so the event can't tell them
// apart; a `load` is drawer content the user just opened, its error shows.
function background(d: ReqDetail): boolean {
  const t = d.elt?.closest("[hx-trigger]")?.getAttribute("hx-trigger") ?? "";
  return /\bevery\b/.test(t);
}

class FlashToast extends HTMLElement {
  private timer = 0;
  private failingSince = 0;
  private reconnecting = false;

  private onClick = (): void => this.dismiss();

  private onResponseError = (e: Event): void => {
    if (e.defaultPrevented) return;
    const d = (e as CustomEvent<ReqDetail & { xhr: { status: number; statusText: string } }>).detail;
    if (background(d)) return this.backgroundFailed();
    this.show("error", `Request failed: ${d.xhr.status} ${d.xhr.statusText}`.trim());
  };

  private onSendError = (e: Event): void => {
    if (background((e as CustomEvent<ReqDetail>).detail)) return this.backgroundFailed();
    this.show("error", "The server did not answer. Check your connection and try again.");
  };

  private onAfterRequest = (e: Event): void => {
    const d = (e as CustomEvent<ReqDetail>).detail;
    if (!d.successful || !background(d)) return;
    this.failingSince = 0;
    if (this.reconnecting) {
      this.reconnecting = false;
      this.dismiss();
    }
  };

  private backgroundFailed(): void {
    const now = Date.now();
    if (!this.failingSince) this.failingSince = now;
    if (this.reconnecting || now - this.failingSince < RECONNECT_MS) return;
    this.reconnecting = true;
    this.show("warning", RECONNECTING);
  }

  connectedCallback(): void {
    this.addEventListener("click", this.onClick);
    document.addEventListener("htmx:responseError", this.onResponseError);
    document.addEventListener("htmx:sendError", this.onSendError);
    document.addEventListener("htmx:afterRequest", this.onAfterRequest);
    this.arm();
  }

  disconnectedCallback(): void {
    window.clearTimeout(this.timer);
    this.removeEventListener("click", this.onClick);
    document.removeEventListener("htmx:responseError", this.onResponseError);
    document.removeEventListener("htmx:sendError", this.onSendError);
    document.removeEventListener("htmx:afterRequest", this.onAfterRequest);
  }

  private show(kind: string, message: string): void {
    this.removeAttribute("leaving");
    this.setAttribute("kind", kind);
    this.textContent = message;
    this.arm();
  }

  private arm(): void {
    window.clearTimeout(this.timer);
    if (this.textContent?.trim() === "" || this.getAttribute("kind") === "error" || this.reconnecting) return;
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
