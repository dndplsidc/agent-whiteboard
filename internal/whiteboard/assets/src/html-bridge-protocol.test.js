import { describe, expect, test } from "vitest";
import {
  HTML_BRIDGE_VERSION,
  MAX_HTML_BRIDGE_COMPONENTS,
  MAX_HTML_BRIDGE_EPOCH_BYTES,
  MAX_HTML_BRIDGE_GEOMETRY,
  MAX_HTML_BRIDGE_ID_BYTES,
  decodeChildBridgeFrame,
  decodeParentBridgeFrame,
} from "./html-bridge-protocol.js";

const epoch = "load_epoch-1";

describe("HTML bridge protocol", () => {
  test("accepts only bounded direction-specific manifest and locate frames", () => {
    expect(HTML_BRIDGE_VERSION).toBe(1);
    expect(decodeParentBridgeFrame({ version: 1, type: "manifest", epoch, components: [
      { id: "summary", type: "section" },
      { id: "chart", type: "chart" },
    ] })).toEqual({ version: 1, type: "manifest", epoch, components: [
      { id: "summary", type: "section" },
      { id: "chart", type: "chart" },
    ] });
    expect(decodeParentBridgeFrame({ version: 1, type: "locate", epoch, id: "summary" })).toEqual({ version: 1, type: "locate", epoch, id: "summary" });
    expect(() => decodeParentBridgeFrame({ version: 1, type: "manifest", epoch, components: [{ id: "same", type: "section" }, { id: "same", type: "table" }] })).toThrow(TypeError);
    expect(() => decodeParentBridgeFrame({ version: 1, type: "manifest", epoch, components: Array.from({ length: MAX_HTML_BRIDGE_COMPONENTS + 1 }, (_, i) => ({ id: `item-${i}`, type: "section" })) })).toThrow(TypeError);
    expect(() => decodeParentBridgeFrame({ version: 1, type: "locate", epoch, id: "summary", command: "submit" })).toThrow(TypeError);
  });

  test("enforces epoch and ID byte boundaries", () => {
    const boundaryEpoch = "e".repeat(MAX_HTML_BRIDGE_EPOCH_BYTES);
    expect(decodeChildBridgeFrame({ version: 1, type: "ready", epoch: boundaryEpoch })).toEqual({ version: 1, type: "ready", epoch: boundaryEpoch });
    expect(() => decodeChildBridgeFrame({ version: 1, type: "ready", epoch: `${boundaryEpoch}e` })).toThrow(TypeError);

    const boundaryID = "é".repeat(MAX_HTML_BRIDGE_ID_BYTES / 2);
    expect(decodeParentBridgeFrame({ version: 1, type: "locate", epoch, id: boundaryID })).toEqual({ version: 1, type: "locate", epoch, id: boundaryID });
    expect(() => decodeParentBridgeFrame({ version: 1, type: "locate", epoch, id: `${boundaryID}é` })).toThrow(TypeError);
  });

  test("accepts ready, clear, and finite bounded candidate geometry for the current epoch shape", () => {
    expect(decodeChildBridgeFrame({ version: 1, type: "ready", epoch })).toEqual({ version: 1, type: "ready", epoch });
    expect(decodeChildBridgeFrame({ version: 1, type: "clear", epoch })).toEqual({ version: 1, type: "clear", epoch });
    const candidate = { version: 1, type: "candidate", epoch, id: "chart", rect: { x: -12.5, y: 4, width: 320, height: 180 } };
    expect(decodeChildBridgeFrame(candidate)).toEqual(candidate);
    for (const bad of [NaN, Infinity, -Infinity]) {
      expect(() => decodeChildBridgeFrame({ ...candidate, rect: { ...candidate.rect, x: bad } })).toThrow(TypeError);
    }
    expect(() => decodeChildBridgeFrame({ ...candidate, rect: { ...candidate.rect, width: -1 } })).toThrow(TypeError);
    const ceiling = { ...candidate, rect: { x: MAX_HTML_BRIDGE_GEOMETRY, y: MAX_HTML_BRIDGE_GEOMETRY, width: MAX_HTML_BRIDGE_GEOMETRY, height: MAX_HTML_BRIDGE_GEOMETRY } };
    expect(decodeChildBridgeFrame(ceiling)).toEqual(ceiling);
    expect(() => decodeChildBridgeFrame({ ...candidate, rect: { ...candidate.rect, x: MAX_HTML_BRIDGE_GEOMETRY + 1 } })).toThrow(TypeError);
    expect(() => decodeChildBridgeFrame({ ...candidate, rect: { ...candidate.rect, extra: 0 } })).toThrow(TypeError);
    expect(() => decodeChildBridgeFrame({ ...candidate, source: "<html>" })).toThrow(TypeError);
    expect(() => decodeChildBridgeFrame({ version: 1, type: "locate", epoch, id: "chart" })).toThrow(TypeError);
    expect(() => decodeChildBridgeFrame({ ...candidate, version: 5 })).toThrow(TypeError);
  });
});
