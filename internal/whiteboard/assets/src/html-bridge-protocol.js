export const HTML_BRIDGE_VERSION = 1;
export const MAX_HTML_BRIDGE_COMPONENTS = 128;
export const MAX_HTML_BRIDGE_ID_BYTES = 256;
export const MAX_HTML_BRIDGE_EPOCH_BYTES = 64;
export const MAX_HTML_BRIDGE_GEOMETRY = 10_000_000;

const encoder = new TextEncoder();
const COMPONENT_TYPES = new Set(["section", "image", "chart", "table", "code", "quote", "component"]);
const EPOCH_PATTERN = /^[A-Za-z0-9_-]+$/;

function record(value) {
  if (value === null || typeof value !== "object" || Array.isArray(value)) return false;
  const prototype = Object.getPrototypeOf(value);
  return prototype === Object.prototype || prototype === null;
}

function exact(value, keys) {
  return record(value) && Object.keys(value).length === keys.length && keys.every((key) => Object.hasOwn(value, key));
}

function boundedString(value, limit) {
  return typeof value === "string" && value.length > 0 && value.trim() === value && !/[\u0000-\u001f\u007f]/u.test(value) && encoder.encode(value).length <= limit;
}

function validEpoch(value) {
  return boundedString(value, MAX_HTML_BRIDGE_EPOCH_BYTES) && EPOCH_PATTERN.test(value);
}

function validID(value) {
  return boundedString(value, MAX_HTML_BRIDGE_ID_BYTES);
}

function validBase(frame, type, keys) {
  return exact(frame, keys) && frame.version === HTML_BRIDGE_VERSION && frame.type === type && validEpoch(frame.epoch);
}

function validComponent(value) {
  return exact(value, ["id", "type"]) && validID(value.id) && COMPONENT_TYPES.has(value.type);
}

function validRect(value) {
  if (!exact(value, ["x", "y", "width", "height"])) return false;
  if (![value.x, value.y, value.width, value.height].every(Number.isFinite)) return false;
  return Math.abs(value.x) <= MAX_HTML_BRIDGE_GEOMETRY && Math.abs(value.y) <= MAX_HTML_BRIDGE_GEOMETRY
    && value.width >= 0 && value.width <= MAX_HTML_BRIDGE_GEOMETRY
    && value.height >= 0 && value.height <= MAX_HTML_BRIDGE_GEOMETRY;
}

function clone(frame) {
  return structuredClone(frame);
}

export function decodeParentBridgeFrame(frame) {
  if (validBase(frame, "manifest", ["version", "type", "epoch", "components"]) && Array.isArray(frame.components)
      && frame.components.length <= MAX_HTML_BRIDGE_COMPONENTS && frame.components.every(validComponent)
      && new Set(frame.components.map(({ id }) => id)).size === frame.components.length) return clone(frame);
  if (validBase(frame, "locate", ["version", "type", "epoch", "id"]) && validID(frame.id)) return clone(frame);
  throw new TypeError("invalid parent HTML bridge frame");
}

export function decodeChildBridgeFrame(frame) {
  if (validBase(frame, "ready", ["version", "type", "epoch"]) || validBase(frame, "clear", ["version", "type", "epoch"])) return clone(frame);
  if (validBase(frame, "candidate", ["version", "type", "epoch", "id", "rect"]) && validID(frame.id) && validRect(frame.rect)) return clone(frame);
  throw new TypeError("invalid child HTML bridge frame");
}
