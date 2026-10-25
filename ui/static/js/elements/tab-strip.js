class TabStrip extends HTMLElement {
    row() {
        return this.querySelector("[data-row]");
    }
    update = () => {
        const row = this.row();
        if (!row)
            return;
        const max = row.scrollWidth - row.clientWidth;
        this.querySelector("[data-prev]")?.toggleAttribute("hidden", row.scrollLeft <= 1);
        this.querySelector("[data-next]")?.toggleAttribute("hidden", row.scrollLeft >= max - 1);
    };
    onWheel = (e) => {
        const row = this.row();
        if (!row || row.scrollWidth <= row.clientWidth)
            return;
        if (Math.abs(e.deltaY) <= Math.abs(e.deltaX))
            return;
        e.preventDefault();
        row.scrollLeft += e.deltaY;
    };
    onClick = (e) => {
        const button = e.target.closest("[data-prev], [data-next]");
        const row = this.row();
        if (!button || !row)
            return;
        const page = row.clientWidth * 0.8;
        row.scrollBy({ left: button.hasAttribute("data-prev") ? -page : page, behavior: "smooth" });
    };
    resize = new ResizeObserver(this.update);
    connectedCallback() {
        const row = this.row();
        if (!row)
            return;
        const cur = row.querySelector("[aria-current]");
        if (cur && row.scrollWidth > row.clientWidth) {
            const pad = parseFloat(getComputedStyle(row).scrollPaddingLeft) || 0;
            const left = cur.getBoundingClientRect().left - row.getBoundingClientRect().left + row.scrollLeft;
            if (left < row.scrollLeft + pad || left + cur.offsetWidth > row.scrollLeft + row.clientWidth - pad) {
                row.scrollLeft = left - pad;
            }
        }
        row.addEventListener("scroll", this.update, { passive: true });
        this.addEventListener("wheel", this.onWheel, { passive: false });
        this.addEventListener("click", this.onClick);
        this.resize.observe(row);
        this.update();
    }
    disconnectedCallback() {
        this.row()?.removeEventListener("scroll", this.update);
        this.removeEventListener("wheel", this.onWheel);
        this.removeEventListener("click", this.onClick);
        this.resize.disconnect();
    }
}
customElements.define("tab-strip", TabStrip);
export {};
