// RemoteSession: the remote-desktop client shared by the browser page and the
// desktop app. Renders into a <canvas>, decodes H.264 with WebCodecs (hardware)
// or falls back to JPEG, and forwards mouse/keyboard input.
(function () {
  const h264Supported = (async () => {
    if (!('VideoDecoder' in window)) return false;
    try { return (await VideoDecoder.isConfigSupported({ codec: 'avc1.640034' })).supported; } catch { return false; }
  })();

  // Exact codec string (avc1.PPCCLL) from the SPS in an Annex-B keyframe.
  function codecFromSPS(d) {
    for (let i = 0; i + 6 < d.length; i++) {
      if (d[i] === 0 && d[i + 1] === 0 && d[i + 2] === 1 && (d[i + 3] & 0x1f) === 7) {
        const h = x => x.toString(16).padStart(2, '0');
        return 'avc1.' + h(d[i + 4]) + h(d[i + 5]) + h(d[i + 6]);
      }
    }
    return 'avc1.640034';
  }

  class RemoteSession {
    /**
     * @param {HTMLCanvasElement} canvas
     * @param {object} o  { wsURL, code, onOpen, onInfo, onStats, onClose }
     *   wsURL: base URL of the host's /ws endpoint (without query)
     */
    constructor(canvas, o) {
      this.cv = canvas;
      this.ctx = canvas.getContext('2d');
      this.o = o;
      this.ws = null;
      this.opened = false;
      this.codec = '';
      this.error = '';
      this.control = false;
      this.rtt = 0;
      this.frames = 0; this.bytes = 0;
      this.dec = null; this.decCodec = ''; this.needKey = true; this.ts = 0;
      this.cfg = { q: 70, fps: 15, w: 1920 };
      this.down = new Set();
      this.pendingMove = null; this.lastMove = 0;
      this._timers = [];
      this._listeners = [];
    }

    async connect() {
      const h264 = await h264Supported;
      const sep = this.o.wsURL.includes('?') ? '&' : '?';
      this.ws = new WebSocket(`${this.o.wsURL}${sep}code=${encodeURIComponent(this.o.code)}${h264 ? '&codec=h264' : ''}`);
      this.ws.binaryType = 'arraybuffer';
      this.ws.onopen = () => {
        this.opened = true;
        this.sendCfg();
        this.cv.focus();
        this.o.onOpen && this.o.onOpen();
      };
      this.ws.onmessage = ev => this._onMessage(ev);
      this.ws.onclose = () => this._teardown();
      this._bindInput();
      this._timers.push(setInterval(() => {
        this.send({ t: 'ping', ts: performance.now() });
        this.o.onStats && this.o.onStats({
          codec: this.codec, fps: this.frames, mbps: this.bytes * 8 / 1e6, rtt: this.rtt,
          control: this.control, error: this.error,
        });
        this.frames = 0; this.bytes = 0;
      }, 1000));
      this._timers.push(setInterval(() => { if (this.pendingMove) { this.send(this.pendingMove); this.pendingMove = null; } }, 30));
    }

    send(o) { if (this.ws && this.ws.readyState === 1) this.ws.send(JSON.stringify(o)); }
    setConfig(c) { Object.assign(this.cfg, c); this.sendCfg(); }
    sendCfg() { this.send({ t: 'cfg', ...this.cfg }); }
    close() { if (this.ws) this.ws.close(); else this._teardown(); }

    /** Send a key combination, e.g. ['ControlLeft','AltLeft','Delete']. */
    sendKeys(codes) {
      codes.forEach(c => this.send({ t: 'kd', c }));
      codes.slice().reverse().forEach(c => this.send({ t: 'ku', c }));
    }

    _teardown() {
      if (this._closed) return;
      this._closed = true;
      this._closeDecoder();
      this._timers.forEach(clearInterval);
      this._listeners.forEach(([el, ev, fn, opt]) => el.removeEventListener(ev, fn, opt));
      this.ws = null;
      this.o.onClose && this.o.onClose(this.opened);
    }

    _draw(img) {
      const w = img.displayWidth || img.width, h = img.displayHeight || img.height;
      if (this.cv.width !== w || this.cv.height !== h) { this.cv.width = w; this.cv.height = h; }
      this.ctx.drawImage(img, 0, 0);
      this.frames++;
    }

    _closeDecoder() {
      if (this.dec && this.dec.state !== 'closed') try { this.dec.close(); } catch { }
      this.dec = null; this.needKey = true;
    }

    _resync() { this._closeDecoder(); this.send({ t: 'kf' }); }

    _onH264(key, data) {
      if (key) {
        const c = codecFromSPS(data);
        if (!this.dec || this.dec.state === 'closed' || c !== this.decCodec) {
          this._closeDecoder();
          this.dec = new VideoDecoder({
            output: f => { this._draw(f); f.close(); },
            error: e => { console.warn('decoder', e); this._resync(); },
          });
          this.dec.configure({ codec: c, optimizeForLatency: true, hardwareAcceleration: 'prefer-hardware' });
          this.decCodec = c;
        }
        this.needKey = false;
      }
      if (this.needKey || !this.dec) return;
      if (this.dec.decodeQueueSize > 8) { this._resync(); return; } // falling behind
      this.dec.decode(new EncodedVideoChunk({ type: key ? 'key' : 'delta', timestamp: (this.ts += 16666), data }));
    }

    async _onMessage(ev) {
      if (typeof ev.data === 'string') {
        const m = JSON.parse(ev.data);
        if (m.t === 'info') { this.control = m.control; this.o.onInfo && this.o.onInfo(m); }
        else if (m.t === 'video') { this.codec = m.codec === 'h264' ? 'H.264' : 'JPEG'; this._closeDecoder(); }
        else if (m.t === 'pong') { this.rtt = Math.round(performance.now() - m.ts); }
        else if (m.t === 'error') { this.error = m.msg; }
        return;
      }
      const u8 = new Uint8Array(ev.data), payload = u8.subarray(1);
      this.bytes += u8.length;
      if (u8[0] === 1) {
        const bmp = await createImageBitmap(new Blob([payload], { type: 'image/jpeg' }));
        this._draw(bmp); bmp.close();
      } else if (u8[0] === 2 || u8[0] === 3) {
        this._onH264(u8[0] === 2, payload);
      }
    }

    _on(el, ev, fn, opt) { el.addEventListener(ev, fn, opt); this._listeners.push([el, ev, fn, opt]); }

    _bindInput() {
      const cv = this.cv;
      const pos = e => { const r = cv.getBoundingClientRect(); return { x: (e.clientX - r.left) / r.width, y: (e.clientY - r.top) / r.height }; };
      this._on(cv, 'mousemove', e => {
        this.pendingMove = { t: 'mm', ...pos(e) };
        const now = performance.now();
        if (now - this.lastMove > 16) { this.send(this.pendingMove); this.pendingMove = null; this.lastMove = now; }
      });
      this._on(cv, 'mousedown', e => { cv.focus(); e.preventDefault(); this.send({ t: 'md', b: e.button, ...pos(e) }); });
      this._on(window, 'mouseup', e => this.send({ t: 'mu', b: e.button, ...pos(e) }));
      this._on(cv, 'contextmenu', e => e.preventDefault());
      this._on(cv, 'wheel', e => {
        e.preventDefault();
        const k = e.deltaMode === 1 ? 33 : e.deltaMode === 2 ? 600 : 1;
        this.send({ t: 'wh', dx: e.deltaX * k, dy: e.deltaY * k });
      }, { passive: false });
      this._on(cv, 'keydown', e => { e.preventDefault(); this.down.add(e.code); this.send({ t: 'kd', c: e.code }); });
      this._on(cv, 'keyup', e => { e.preventDefault(); this.down.delete(e.code); this.send({ t: 'ku', c: e.code }); });
      this._on(cv, 'blur', () => { this.down.forEach(c => this.send({ t: 'ku', c })); this.down.clear(); });
    }
  }

  window.RemoteSession = RemoteSession;
})();
