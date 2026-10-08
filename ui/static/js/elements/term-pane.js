class TermPane extends HTMLElement {
    ws = null;
    term = null;
    watch = null;
    code = null;
    connectedCallback() {
        if (typeof Terminal === "undefined" || typeof FitAddon === "undefined") {
            setTimeout(() => this.isConnected && !this.term && this.connectedCallback(), 50);
            return;
        }
        const term = new Terminal({ fontSize: 14, cursorBlink: true, theme: { background: "#000000" } });
        const fit = new FitAddon.FitAddon();
        term.loadAddon(fit);
        term.open(this);
        this.term = term;
        fit.fit();
        const url = new URL(this.getAttribute("url") ?? "", location.href);
        url.protocol = location.protocol === "https:" ? "wss:" : "ws:";
        const ws = new WebSocket(url);
        ws.binaryType = "arraybuffer";
        this.ws = ws;
        const resize = (cols, rows) => {
            if (ws.readyState === WebSocket.OPEN)
                ws.send(JSON.stringify({ type: "resize", cols, rows }));
        };
        ws.onopen = () => resize(term.cols, term.rows);
        ws.onmessage = (e) => {
            if (typeof e.data === "string") {
                const m = JSON.parse(e.data);
                if (m.type === "exit")
                    this.code = m.code ?? null;
                return;
            }
            term.write(new Uint8Array(e.data));
        };
        ws.onclose = () => {
            const why = this.code === null ? "" : ` (exit ${this.code})`;
            term.write(`\r\n[disconnected]${why}\r\n`);
        };
        term.onData((d) => {
            if (ws.readyState === WebSocket.OPEN)
                ws.send(new TextEncoder().encode(d));
        });
        term.onResize((s) => resize(s.cols, s.rows));
        this.watch = new ResizeObserver(() => fit.fit());
        this.watch.observe(this);
    }
    disconnectedCallback() {
        this.watch?.disconnect();
        this.watch = null;
        if (this.ws)
            this.ws.onclose = null;
        this.ws?.close();
        this.ws = null;
        this.term?.dispose();
        this.term = null;
    }
}
customElements.define("term-pane", TermPane);
export {};
