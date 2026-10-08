// <term-pane url="/api/v1/.../terminal?container=id"> is a shell in the
// browser: xterm.js over one websocket to the terminal endpoint. Binary
// frames are bytes both ways; text frames are JSON, resize out and exit in.
// xterm and its fit addon are vendored classic scripts the page loads first
// (they are globals); the session cookie authenticates the socket.
type Term = {
  cols: number;
  rows: number;
  open(el: HTMLElement): void;
  write(data: string | Uint8Array): void;
  loadAddon(a: unknown): void;
  onData(f: (d: string) => void): void;
  onResize(f: (s: { cols: number; rows: number }) => void): void;
  dispose(): void;
};
declare const Terminal: new (opts: object) => Term;
declare const FitAddon: { FitAddon: new () => { fit(): void } };

class TermPane extends HTMLElement {
  private ws: WebSocket | null = null;
  private term: Term | null = null;
  private watch: ResizeObserver | null = null;
  private code: number | null = null;

  connectedCallback(): void {
    // htmx injects the classic scripts async: wait until xterm has loaded.
    if (typeof Terminal === "undefined" || typeof FitAddon === "undefined") {
      setTimeout(() => this.isConnected && !this.term && this.connectedCallback(), 50);
      return;
    }
    // Literal black on purpose: a terminal is black in both themes, and the
    // output assumes a dark background.
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
    const resize = (cols: number, rows: number): void => {
      if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: "resize", cols, rows }));
    };
    ws.onopen = () => resize(term.cols, term.rows);
    ws.onmessage = (e) => {
      if (typeof e.data === "string") {
        const m = JSON.parse(e.data) as { type?: string; code?: number };
        if (m.type === "exit") this.code = m.code ?? null;
        return;
      }
      term.write(new Uint8Array(e.data as ArrayBuffer));
    };
    ws.onclose = () => {
      const why = this.code === null ? "" : ` (exit ${this.code})`;
      term.write(`\r\n[disconnected]${why}\r\n`);
    };
    term.onData((d) => {
      if (ws.readyState === WebSocket.OPEN) ws.send(new TextEncoder().encode(d));
    });
    term.onResize((s) => resize(s.cols, s.rows));
    this.watch = new ResizeObserver(() => fit.fit());
    this.watch.observe(this);
  }

  disconnectedCallback(): void {
    this.watch?.disconnect();
    this.watch = null;
    if (this.ws) this.ws.onclose = null;
    this.ws?.close();
    this.ws = null;
    this.term?.dispose();
    this.term = null;
  }
}

customElements.define("term-pane", TermPane);
