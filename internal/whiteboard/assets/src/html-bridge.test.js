import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { installHTMLBridge } from "./html-bridge.js";

const epoch = "epoch-1";
const manifest = {
  version: 1,
  type: "manifest",
  epoch,
  components: [
    { id: "outer", type: "section" },
    { id: "inner", type: "chart" },
  ],
};

function parentFrame(data, source = window) {
  window.dispatchEvent(new MessageEvent("message", { data, source }));
}

function sentFrames(postMessage) {
  return postMessage.mock.calls.map(([frame]) => frame);
}

describe("HTML rendered-child bridge", () => {
  let postMessage;
  let animationFrames;
  let restoreRect;

  beforeEach(() => {
    document.body.innerHTML = `<section id="outer"><div id="inner"></div></section><div id="unknown"></div>`;
    postMessage = vi.spyOn(window, "postMessage").mockImplementation(() => {});
    animationFrames = [];
    vi.stubGlobal("requestAnimationFrame", (callback) => {
      animationFrames.push(callback);
      return animationFrames.length;
    });
    const rects = {
      outer: { x: 1, y: 2, width: 300, height: 200 },
      inner: { x: 10, y: 20, width: 100, height: 80 },
    };
    const original = Element.prototype.getBoundingClientRect;
    Element.prototype.getBoundingClientRect = function getBoundingClientRect() {
      return { ...rects[this.id], top: rects[this.id]?.y, left: rects[this.id]?.x, right: 0, bottom: 0, toJSON() {} };
    };
    restoreRect = () => { Element.prototype.getBoundingClientRect = original; };
  });

  afterEach(() => {
    restoreRect();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  test("accepts only exact parent frames and reports the deepest manifested pointer candidate", () => {
    const bridge = installHTMLBridge(window);
    parentFrame(manifest, {});
    parentFrame({ ...manifest, broker: { submit: true } });
    expect(sentFrames(postMessage)).toEqual([]);

    parentFrame(manifest);
    expect(sentFrames(postMessage)).toEqual([{ version: 1, type: "ready", epoch }]);
    document.querySelector("#inner").dispatchEvent(new PointerEvent("pointermove", { bubbles: true }));
    expect(animationFrames).toHaveLength(1);
    animationFrames.shift()(0);
    expect(sentFrames(postMessage).at(-1)).toEqual({
      version: 1, type: "candidate", epoch, id: "inner", rect: { x: 10, y: 20, width: 100, height: 80 },
    });
    bridge.destroy();
  });

  test("captures primitives before hostile publisher changes and bounds locate to manifest IDs", () => {
    const scrollIntoView = vi.fn();
    const originalScroll = Element.prototype.scrollIntoView;
    Element.prototype.scrollIntoView = scrollIntoView;
    const bridge = installHTMLBridge(window);
    Element.prototype.scrollIntoView = () => { throw new Error("publisher override"); };
    Element.prototype.getBoundingClientRect = () => ({ x: Infinity, y: 0, width: 1, height: 1 });
    window.postMessage = () => { throw new Error("publisher override"); };

    parentFrame(manifest);
    parentFrame({ version: 1, type: "locate", epoch, id: "unknown" });
    expect(scrollIntoView).not.toHaveBeenCalled();
    parentFrame({ version: 1, type: "locate", epoch, id: "inner" });
    expect(scrollIntoView).toHaveBeenCalledOnce();
    animationFrames.shift()(0);
    expect(sentFrames(postMessage).at(-1)).toMatchObject({ type: "candidate", epoch, id: "inner" });

    bridge.destroy();
    Element.prototype.scrollIntoView = originalScroll;
  });

  test("coalesces geometry, clears removed candidates, and resets on a new epoch", () => {
    const bridge = installHTMLBridge(window);
    parentFrame(manifest);
    const inner = document.querySelector("#inner");
    inner.dispatchEvent(new FocusEvent("focusin", { bubbles: true }));
    window.dispatchEvent(new Event("scroll"));
    window.dispatchEvent(new Event("resize"));
    expect(animationFrames).toHaveLength(1);
    inner.remove();
    animationFrames.shift()(0);
    expect(sentFrames(postMessage).at(-1)).toEqual({ version: 1, type: "clear", epoch });

    parentFrame({ version: 1, type: "manifest", epoch: "epoch-2", components: [{ id: "outer", type: "section" }] });
    expect(sentFrames(postMessage).slice(-2)).toEqual([
      { version: 1, type: "clear", epoch },
      { version: 1, type: "ready", epoch: "epoch-2" },
    ]);
    bridge.destroy();
  });
});
