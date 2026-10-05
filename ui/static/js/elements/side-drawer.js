const FOCUSABLE = 'a[href], button:not([disabled]), input:not([disabled]), select, textarea, [tabindex]:not([tabindex="-1"])';
class SideDrawer extends HTMLElement {
    opener = null;
    dirty = false;
    onSwap = (e) => {
        const target = e.detail?.target;
        const body = this.body();
        if (!body || !target || !body.contains(target))
            return;
        this.dirty = false;
        const was = document.activeElement;
        if (!this.hasAttribute("open") && was && !this.contains(was))
            this.opener = was;
        this.setAttribute("open", "");
        const tab = new URLSearchParams(location.search).get("tab");
        if (tab)
            this.setAttribute("tab", tab);
        else
            this.removeAttribute("tab");
        this.querySelector("[data-close]")?.focus();
    };
    onClick = (e) => {
        if (e.target.closest("[data-close], [data-backdrop]"))
            this.close();
    };
    onEdit = (e) => {
        const el = e.target;
        if (el.closest("form") && !el.closest("dialog") && this.body()?.contains(el))
            this.dirty = true;
    };
    onError = (e) => {
        const d = e.detail;
        const body = this.body();
        if (!this.hasAttribute("open") || !body || !d?.target || !body.contains(d.target))
            return;
        e.preventDefault();
        if (d.xhr?.status === 404)
            return this.close(true);
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
        document.addEventListener("htmx:afterSwap", this.onSwap);
        document.addEventListener("htmx:responseError", this.onError, true);
        document.addEventListener("keydown", this.onKey);
        this.addEventListener("click", this.onClick);
        this.addEventListener("input", this.onEdit);
        this.addEventListener("change", this.onEdit);
    }
    disconnectedCallback() {
        document.removeEventListener("htmx:afterSwap", this.onSwap);
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
        if (this.dirty && !force && !window.confirm("Discard your unsaved changes?"))
            return;
        this.dirty = false;
        this.removeAttribute("open");
        this.removeAttribute("tab");
        this.body()?.replaceChildren();
        const url = new URL(location.href);
        url.searchParams.delete("drawer");
        url.searchParams.delete("tab");
        history.replaceState(history.state, "", url);
        this.dispatchEvent(new CustomEvent("drawer-closed", { bubbles: true }));
        if (this.opener?.isConnected)
            this.opener.focus();
        this.opener = null;
    }
}
customElements.define("side-drawer", SideDrawer);
export {};
