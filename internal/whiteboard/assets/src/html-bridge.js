import {
  HTML_BRIDGE_VERSION,
  MAX_HTML_BRIDGE_GEOMETRY,
  decodeParentBridgeFrame,
} from "./html-bridge-protocol.js";

const numberIsFinite = Number.isFinite;
const absolute = Math.abs;

function finiteRect(rect) {
  const value = { x: rect.x, y: rect.y, width: rect.width, height: rect.height };
  return numberIsFinite(value.x) && numberIsFinite(value.y) && numberIsFinite(value.width) && numberIsFinite(value.height)
    && absolute(value.x) <= MAX_HTML_BRIDGE_GEOMETRY
    && absolute(value.y) <= MAX_HTML_BRIDGE_GEOMETRY
    && value.width >= 0 && value.width <= MAX_HTML_BRIDGE_GEOMETRY
    && value.height >= 0 && value.height <= MAX_HTML_BRIDGE_GEOMETRY
    ? value
    : null;
}

export function installHTMLBridge(scope) {
  const parent = scope.parent;
  const document = scope.document;
  const ElementType = scope.Element;
  const getByID = scope.Document.prototype.getElementById;
  const getRect = scope.Element.prototype.getBoundingClientRect;
  const scrollIntoView = scope.Element.prototype.scrollIntoView;
  const send = parent.postMessage.bind(parent);
  const requestFrame = scope.requestAnimationFrame?.bind(scope)
    ?? ((callback) => scope.setTimeout(callback, 16));

  let epoch = "";
  let components = new Map();
  let activeID = "";
  let framePending = false;
  let destroyed = false;

  function post(type, extra = {}) {
    if (!epoch || destroyed) return;
    send({ version: HTML_BRIDGE_VERSION, type, epoch, ...extra }, "*");
  }

  function resolve(id) {
    if (!components.has(id)) return null;
    const element = getByID.call(document, id);
    return element?.isConnected ? element : null;
  }

  function flushGeometry() {
    framePending = false;
    if (!activeID) return;
    const element = resolve(activeID);
    const rect = element && finiteRect(getRect.call(element));
    if (!rect) {
      activeID = "";
      post("clear");
      return;
    }
    post("candidate", { id: activeID, rect });
  }

  function schedule() {
    if (framePending || !activeID) return;
    framePending = true;
    requestFrame(flushGeometry);
  }

  function candidateFromEvent(event) {
    const path = typeof event.composedPath === "function" ? event.composedPath() : [];
    let element = path.find((node) => node instanceof ElementType && components.has(node.id));
    if (!element && event.target instanceof ElementType) {
      element = event.target;
      while (element && !components.has(element.id)) element = element.parentElement;
    }
    const nextID = element?.id ?? "";
    if (!nextID) {
      if (activeID) {
        activeID = "";
        post("clear");
      }
      return;
    }
    activeID = nextID;
    schedule();
  }

  function clearCandidate() {
    if (!activeID) return;
    activeID = "";
    post("clear");
  }

  function receive(event) {
    if (event.source !== parent) return;
    let frame;
    try {
      frame = decodeParentBridgeFrame(event.data);
    } catch {
      return;
    }
    if (frame.type === "manifest") {
      const previousEpoch = epoch;
      const hadCandidate = Boolean(activeID);
      epoch = frame.epoch;
      components = new Map(frame.components.map((component) => [component.id, component.type]));
      activeID = "";
      framePending = false;
      if (hadCandidate && previousEpoch) {
        const nextEpoch = epoch;
        epoch = previousEpoch;
        post("clear");
        epoch = nextEpoch;
      }
      post("ready");
      return;
    }
    if (frame.epoch !== epoch || frame.type !== "locate") return;
    const element = resolve(frame.id);
    if (!element) return;
    if (typeof scrollIntoView === "function") scrollIntoView.call(element, { block: "center", inline: "nearest" });
    activeID = frame.id;
    schedule();
  }

  const listenerSpecs = [
    [scope, "message", receive, false],
    [document, "pointermove", candidateFromEvent, true],
    [document, "focusin", candidateFromEvent, true],
    [document, "pointerleave", clearCandidate, true],
    [document, "focusout", clearCandidate, true],
    [scope, "scroll", schedule, true],
    [scope, "resize", schedule, true],
    [scope, "pagehide", clearCandidate, true],
  ];
  const removers = listenerSpecs.map(([target, type, listener, options]) => {
    const removeListener = target.removeEventListener.bind(target);
    target.addEventListener.bind(target)(type, listener, options);
    return () => removeListener(type, listener, options);
  });

  return {
    destroy() {
      if (destroyed) return;
      destroyed = true;
      for (const removeListener of removers) removeListener();
      components.clear();
      activeID = "";
      epoch = "";
    },
  };
}

if (typeof window !== "undefined" && window.parent !== window) installHTMLBridge(window);
