// WebSocket auto-reconnect helper for stackr.
//
// Usage:
//   const ws = new HamrWS("/ws");
//   ws.onmessage = function(event) { ... };

(function() {
    "use strict";

    var MIN_DELAY = 1000;
    var MAX_DELAY = 30000;

    /**
     * HamrWS wraps a WebSocket with auto-reconnect and exponential backoff.
     * @param {string} url - WebSocket URL path (e.g. "/ws"). Uses the page's transport security.
     */
    function HamrWS(url) {
        this._url = resolveURL(url);
        this._delay = MIN_DELAY;
        this._ws = null;
        this.onopen = null;
        this.onmessage = null;
        this._connect();
    }

    HamrWS.prototype._connect = function() {
        var self = this;
        var ws = new WebSocket(this._url);

        ws.onopen = function(e) {
            self._delay = MIN_DELAY;
            self._ws = ws;
            if (self.onopen) self.onopen(e);
        };

        ws.onmessage = function(e) {
            if (self.onmessage) self.onmessage(e);
        };

        ws.onclose = function() {
            self._ws = null;
            self._reconnect();
        };

        ws.onerror = function() {
            ws.close();
        };
    };

    HamrWS.prototype._reconnect = function() {
        var self = this;
        var delay = self._delay + Math.random() * 500;
        setTimeout(function() {
            self._connect();
        }, delay);
        self._delay = Math.min(self._delay * 2, MAX_DELAY);
    };

    /** Send data through the WebSocket. Silently dropped if not connected. */
    HamrWS.prototype.send = function(data) {
        if (this._ws && this._ws.readyState === WebSocket.OPEN) {
            this._ws.send(data);
        }
    };

    function resolveURL(path) {
        var protocol = location.protocol === "https:" ? "wss:" : "ws:";
        return protocol + "//" + location.host + path;
    }

    window.HamrWS = HamrWS;
})();
