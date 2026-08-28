// Keeps the browser asset unit tests portable across Node runtimes.
//
// Node >= 23 installs a process-wide `localStorage` getter that returns
// undefined unless `--localstorage-file` is provided, and newer runtimes keep
// that binding even under `--no-experimental-webstorage`. The getter shadows
// the jsdom storage that the viewer and model settings tests rely on. Rebind
// it to a hermetic in-memory storage only when the current binding is
// unusable, so runtimes that already expose a working storage keep it.
//
// The fixture mirrors the Storage exotic object: entries live on as own
// enumerable properties in insertion order, while the API methods stay on the
// prototype so `Object.keys(storage)` lists only stored keys.

function usableStorage(value) {
  return (
    typeof value === "object" &&
    value !== null &&
    typeof value.getItem === "function" &&
    typeof value.setItem === "function" &&
    typeof value.removeItem === "function" &&
    typeof value.clear === "function"
  );
}

class MemoryStorage {
  getItem(key) {
    const name = String(key);
    return Object.prototype.hasOwnProperty.call(this, name) ? this[name] : null;
  }
  setItem(key, value) {
    this[String(key)] = String(value);
  }
  removeItem(key) {
    delete this[String(key)];
  }
  clear() {
    for (const name of Object.keys(this)) delete this[name];
  }
  key(index) {
    return Object.keys(this)[index] ?? null;
  }
  get length() {
    return Object.keys(this).length;
  }
}

if (!usableStorage(globalThis.localStorage)) {
  Object.defineProperty(globalThis, "localStorage", {
    configurable: true,
    writable: true,
    value: new MemoryStorage(),
  });
}
