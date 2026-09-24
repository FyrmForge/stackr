class SideDrawer extends HTMLElement {
    onSwap = (e) => {
        const target = e.detail?.target;
        const body = this.body();
        if (!body || !target || !body.contains(target))
            return;
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
    onKey = (e) => {
        if (e.key === "Escape" && !document.querySelector("dialog[open]"))
            this.close();
    };
    connectedCallback() {
        document.addEventListener("htmx:afterSwap", this.onSwap);
        document.addEventListener("keydown", this.onKey);
        this.addEventListener("click", this.onClick);
    }
    disconnectedCallback() {
        document.removeEventListener("htmx:afterSwap", this.onSwap);
        document.removeEventListener("keydown", this.onKey);
        this.removeEventListener("click", this.onClick);
    }
    body() {
        return this.querySelector("#drawer-body");
    }
    close() {
        if (!this.hasAttribute("open"))
            return;
        this.removeAttribute("open");
        this.removeAttribute("tab");
        this.body()?.replaceChildren();
        const url = new URL(location.href);
        url.searchParams.delete("drawer");
        url.searchParams.delete("tab");
        history.replaceState(history.state, "", url);
        this.dispatchEvent(new CustomEvent("drawer-closed", { bubbles: true }));
    }
}
customElements.define("side-drawer", SideDrawer);
export {};
