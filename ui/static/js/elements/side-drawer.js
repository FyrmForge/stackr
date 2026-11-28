const FOCUSABLE = 'a[href], button:not([disabled]), input:not([disabled]), select, textarea, [tabindex]:not([tabindex="-1"])';
class SideDrawer extends HTMLElement {
    opener = null;
    dirty = false;
    fullByLink = false;
    saves = new WeakSet();
    edited = new Set();
    onSwap = (e) => {
        const d = e.detail;
        const target = d?.target;
        const body = this.body();
        if (!body || !target || !body.contains(target))
            return;
        const x = d.xhr;
        const refused = !!x && this.saves.has(x) && x.status >= 400;
        if (refused) {
            this.edited.clear();
            for (const f of target.querySelectorAll("form:not([data-hold], [hx-get], [hx-trigger*=change])"))
                this.edited.add(f);
            this.dirty = true;
        }
        else if (target === body) {
            this.dirty = false;
            this.edited.clear();
        }
        else {
            for (const f of this.edited)
                if (!body.contains(f))
                    this.edited.delete(f);
            this.dirty = this.edited.size > 0;
        }
        const was = document.activeElement;
        if (!this.hasAttribute("open") && was && !this.contains(was))
            this.opener = was;
        this.setAttribute("open", "");
        if (d.requestConfig?.elt?.closest("[data-full]") && !this.hasAttribute("full"))
            this.expand(true, true);
        const tab = new URLSearchParams(location.search).get("tab");
        if (tab)
            this.setAttribute("tab", tab);
        else
            this.removeAttribute("tab");
        if (target === body)
            this.querySelector("[data-close]")?.focus({ preventScroll: true });
    };
    onBefore = (e) => {
        const body = this.body();
        const d = e.detail;
        const form = d?.elt?.closest("form");
        const save = (form && this.edited.has(form) && !d?.elt?.closest("[data-hold]")) || d?.elt?.closest('form[id$="-held"]');
        if (body && d?.xhr && d.elt && save && body.contains(d.elt) && (d.requestConfig?.verb ?? "get").toLowerCase() !== "get")
            this.saves.add(d.xhr);
        if (!body || d?.target !== body || this.hasAttribute("open"))
            return;
        const was = document.activeElement;
        if (was && !this.contains(was))
            this.opener = was;
        const p = document.createElement("p");
        p.className = "p-6 text-sm text-rw-muted";
        p.setAttribute("aria-busy", "true");
        p.dataset.loading = "";
        p.textContent = "Loading…";
        body.replaceChildren(p);
        this.setAttribute("open", "");
    };
    onLost = () => {
        if (this.body()?.querySelector("[data-loading]"))
            this.close(true);
    };
    onDone = (e) => {
        const d = e.detail;
        if (d?.xhr && this.saves.delete(d.xhr) && d.successful && d.xhr.status < 400) {
            this.dirty = false;
            this.edited.clear();
        }
        if (d?.target === this.body())
            this.onLost();
    };
    onClick = (e) => {
        const el = e.target;
        if (el.closest("[data-expand]"))
            return this.expand(!this.hasAttribute("full"), false);
        if (el.closest("[data-close], [data-backdrop]"))
            return this.close();
        const ask = this.querySelector("dialog[data-discard]");
        if (el.closest("[data-keep]"))
            ask?.close();
        else if (el.closest("[data-discard-yes]")) {
            ask?.close();
            this.close(true);
        }
    };
    expand(on, byLink = false) {
        this.fullByLink = byLink;
        this.toggleAttribute("full", on);
        const top = document.getElementById("shell-header")?.getBoundingClientRect().bottom ?? 0;
        this.style.setProperty("--shell-top", `${top}px`);
        const label = on ? "Exit full screen" : "Full screen";
        const btn = this.querySelector("[data-expand]");
        btn?.setAttribute("aria-pressed", String(on));
        btn?.setAttribute("aria-label", label);
        if (btn)
            btn.title = label;
    }
    onSettle = () => this.expand(this.hasAttribute("full"), this.fullByLink);
    onEdit = (e) => {
        const el = e.target;
        const form = el.closest("form");
        if (form && !el.closest('dialog, [data-hold], form[hx-get], [hx-trigger*="change"]') && this.body()?.contains(el)) {
            this.edited.add(form);
            this.dirty = true;
        }
    };
    onError = (e) => {
        const d = e.detail;
        const body = this.body();
        if (!this.hasAttribute("open") || !body || !d?.target || !body.contains(d.target))
            return;
        e.preventDefault();
        if (d.xhr?.status === 404)
            return this.close(true);
        body.querySelector("[data-loading]")?.remove();
        body.querySelector("[data-request-error]")?.remove();
        const p = document.createElement("p");
        p.className = "banner banner-danger";
        p.setAttribute("role", "alert");
        p.dataset.requestError = "";
        const id = d.xhr?.getResponseHeader("X-Request-ID");
        p.textContent = "Something went wrong. Try again." + (id ? ` Request id: ${id}` : "");
        (body.querySelector("#drawer-view")?.lastElementChild ?? body).prepend(p);
    };
    onKey = (e) => {
        if (!this.hasAttribute("open") || document.querySelector("dialog[open]"))
            return;
        if (e.key === "Escape")
            this.close();
        if (e.key !== "Tab")
            return;
        const all = [...this.querySelectorAll(FOCUSABLE)].filter((el) => el.offsetParent !== null);
        if (!all.length)
            return;
        const first = all[0], last = all[all.length - 1], at = document.activeElement;
        if (e.shiftKey ? at === first || !this.contains(at) : at === last || !this.contains(at)) {
            e.preventDefault();
            (e.shiftKey ? last : first).focus();
        }
    };
    connectedCallback() {
        document.addEventListener("htmx:beforeRequest", this.onBefore);
        document.addEventListener("htmx:sendError", this.onLost);
        document.addEventListener("htmx:timeout", this.onLost);
        document.addEventListener("htmx:afterRequest", this.onDone);
        document.addEventListener("htmx:afterSwap", this.onSwap);
        document.addEventListener("htmx:afterSettle", this.onSettle);
        document.addEventListener("htmx:responseError", this.onError, true);
        document.addEventListener("keydown", this.onKey);
        this.addEventListener("click", this.onClick);
        this.addEventListener("input", this.onEdit);
        this.addEventListener("change", this.onEdit);
    }
    disconnectedCallback() {
        document.removeEventListener("htmx:beforeRequest", this.onBefore);
        document.removeEventListener("htmx:sendError", this.onLost);
        document.removeEventListener("htmx:timeout", this.onLost);
        document.removeEventListener("htmx:afterRequest", this.onDone);
        document.removeEventListener("htmx:afterSwap", this.onSwap);
        document.removeEventListener("htmx:afterSettle", this.onSettle);
        document.removeEventListener("htmx:responseError", this.onError, true);
        document.removeEventListener("keydown", this.onKey);
        this.removeEventListener("click", this.onClick);
        this.removeEventListener("input", this.onEdit);
        this.removeEventListener("change", this.onEdit);
    }
    body() {
        return this.querySelector("#drawer-body");
    }
    close(force = false) {
        if (!this.hasAttribute("open"))
            return;
        if ((this.dirty || this.querySelector('#drawer-body form[id$="-held"]')) && !force) {
            const ask = this.querySelector("dialog[data-discard]");
            if (ask)
                return void (ask.open || ask.showModal());
        }
        this.dirty = false;
        this.edited.clear();
        this.querySelector("dialog[data-discard]")?.close();
        if (this.fullByLink)
            this.expand(false);
        this.removeAttribute("open");
        this.removeAttribute("tab");
        this.body()?.replaceChildren();
        const url = new URL(location.href);
        url.searchParams.delete("drawer");
        url.searchParams.delete("tab");
        url.searchParams.delete("open");
        history.replaceState(history.state, "", url);
        this.dispatchEvent(new CustomEvent("drawer-closed", { bubbles: true }));
        if (this.opener?.isConnected)
            this.opener.focus();
        this.opener = null;
    }
}
customElements.define("side-drawer", SideDrawer);
export {};
