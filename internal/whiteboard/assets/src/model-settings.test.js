import { beforeEach, describe, expect, test, vi } from "vitest";

import {
  CODEX_SETTINGS_STORAGE_KEY,
  CURSOR_SETTINGS_STORAGE_KEY,
  PI_SETTINGS_STORAGE_KEY,
  createSettingsDraftState,
  createModelSettingsControl,
  editSettingsDraft,
  executionSettingsEqual,
  formatExecutionSettings,
  modelCompatibility,
  readSettingsPreference,
  reconcileSettingsDraft,
  recordSettingsSubmission,
  settingsCompatibility,
  writeSettingsPreference,
} from "./model-settings.js";

const sol = {
  model: "gpt-5.6-sol",
  model_display_name: "5.6 Sol",
  description: "Strong general-purpose coding model.",
  default_effort: "high",
  supported_reasoning_efforts: [
    { effort: "medium", description: "Balanced reasoning." },
    { effort: "high", description: "Deeper reasoning." },
    { effort: "xhigh", description: "Maximum reasoning." },
  ],
  supports_images: true,
  default: true,
  supports_fast: true,
};
const luna = {
  model: "gpt-5.6-luna",
  model_display_name: "5.6 Luna",
  description: "Fast focused coding model.",
  default_effort: "medium",
  supported_reasoning_efforts: [{ effort: "medium", description: "Balanced reasoning." }],
  supports_images: false,
  default: false,
  supports_fast: false,
};
const catalog = [sol, luna];
const fastSol = { model: sol.model, effort: "high", speed: "fast" };
const standardLuna = { model: luna.model, effort: "medium", speed: "standard" };
const presentedSol = { ...fastSol, model_display_name: sol.model_display_name, selectable: true };

beforeEach(() => {
  document.body.innerHTML = "";
  localStorage.clear();
});

describe("provider settings helpers", () => {
  test("validates compatibility without silently changing effort or speed", () => {
    expect(settingsCompatibility(catalog, fastSol)).toEqual({ compatible: true, reason: null });
    expect(settingsCompatibility(catalog, { ...fastSol, effort: "minimal" })).toEqual({ compatible: false, reason: "effort_unsupported" });
    expect(settingsCompatibility(catalog, { ...standardLuna, speed: "fast" })).toEqual({ compatible: false, reason: "fast_unsupported" });
    expect(modelCompatibility(catalog, fastSol, luna.model)).toEqual({
      compatible: false,
      reason: "effort_and_speed",
      explanation: "Choose a supported effort and Standard speed first.",
    });
    expect(formatExecutionSettings(presentedSol)).toEqual({ visible: "5.6 Sol · High", accessible: "Model 5.6 Sol, effort High, speed Fast", fast: true });
  });

  test("uses isolated provider keys while retaining the exact Pi and Codex keys", () => {
    writeSettingsPreference(localStorage, "codex", fastSol);
    writeSettingsPreference(localStorage, "cursor", { ...fastSol, speed: "standard" });
    writeSettingsPreference(localStorage, "pi", standardLuna);
    expect(CODEX_SETTINGS_STORAGE_KEY).toBe("agent-whiteboard-codex-settings-v1");
    expect(CURSOR_SETTINGS_STORAGE_KEY).toBe("agent-whiteboard-cursor-settings-v1");
    expect(PI_SETTINGS_STORAGE_KEY).toBe("agent-whiteboard-pi-settings-v1");
    expect(readSettingsPreference(localStorage, "codex")).toEqual(fastSol);
    expect(readSettingsPreference(localStorage, "cursor")).toEqual({ ...fastSol, speed: "standard" });
    expect(readSettingsPreference(localStorage, "pi")).toEqual(standardLuna);
    expect(Object.keys(localStorage).sort()).toEqual([CODEX_SETTINGS_STORAGE_KEY, CURSOR_SETTINGS_STORAGE_KEY, PI_SETTINGS_STORAGE_KEY].sort());
    expect(() => readSettingsPreference(localStorage, "other")).not.toThrow();
    expect(readSettingsPreference(localStorage, "other")).toBeNull();
    expect(readSettingsPreference(localStorage, "toString")).toBeNull();
    expect(() => writeSettingsPreference(localStorage, "other", fastSol)).toThrow(TypeError);
  });

  test("reads and writes only a complete bounded semantic preference", () => {
    writeSettingsPreference(localStorage, "codex", fastSol);
    expect(localStorage.getItem(CODEX_SETTINGS_STORAGE_KEY)).toBe(JSON.stringify(fastSol));
    expect(readSettingsPreference(localStorage, "codex")).toEqual(fastSol);

    localStorage.setItem(CODEX_SETTINGS_STORAGE_KEY, JSON.stringify({ ...fastSol, native_tier: "priority" }));
    expect(readSettingsPreference(localStorage, "codex")).toBeNull();
    localStorage.setItem(CODEX_SETTINGS_STORAGE_KEY, "{bad json");
    expect(readSettingsPreference(localStorage, "codex")).toBeNull();
    localStorage.setItem(CODEX_SETTINGS_STORAGE_KEY, "x".repeat(1025));
    expect(readSettingsPreference(localStorage, "codex")).toBeNull();

    const disabled = { getItem: vi.fn(() => { throw new Error("disabled"); }), setItem: vi.fn(() => { throw new Error("disabled"); }) };
    expect(readSettingsPreference(disabled, "codex")).toBeNull();
    expect(() => writeSettingsPreference(disabled, "codex", fastSol)).not.toThrow();
    expect(() => writeSettingsPreference(localStorage, "codex", { ...fastSol, speed: "priority" })).toThrow(TypeError);
  });

  test("preserves later local intent while advancing accepted effective state", () => {
    const draft = createSettingsDraftState(fastSol);
    reconcileSettingsDraft(draft, { identity: "conversation-a", settingsState: "verified", effectiveSettings: presentedSol, catalog });
    expect(draft.draft).toEqual(fastSol);
    expect(draft.dirty).toBe(false);

    editSettingsDraft(draft, { effort: "xhigh" });
    recordSettingsSubmission(draft, "turn-a");
    editSettingsDraft(draft, { speed: "standard" });
    const accepted = { ...presentedSol, effort: "xhigh" };
    reconcileSettingsDraft(draft, { identity: "conversation-a", settingsState: "verified", effectiveSettings: accepted, catalog, acceptedTurnID: "turn-a" });
    expect(draft.baseline).toEqual({ model: sol.model, effort: "xhigh", speed: "fast" });
    expect(draft.draft).toEqual({ model: sol.model, effort: "xhigh", speed: "standard" });
    expect(draft.dirty).toBe(true);

    reconcileSettingsDraft(draft, { identity: "conversation-a", settingsState: "verified", effectiveSettings: { ...accepted, speed: "standard" }, catalog });
    expect(draft.draft).toEqual({ model: sol.model, effort: "xhigh", speed: "standard" });
    expect(draft.dirty).toBe(false);
  });

  test("resets only on a real conversation identity transition and retains dirty state while unverified", () => {
    const draft = createSettingsDraftState(null);
    reconcileSettingsDraft(draft, { identity: "conversation-a", settingsState: "verified", effectiveSettings: presentedSol, catalog });
    editSettingsDraft(draft, { effort: "xhigh" });
    reconcileSettingsDraft(draft, { identity: "conversation-a", settingsState: "unverified", effectiveSettings: null, catalog });
    expect(draft.draft.effort).toBe("xhigh");

    reconcileSettingsDraft(draft, {
      identity: "conversation-b",
      settingsState: "verified",
      effectiveSettings: { ...standardLuna, model_display_name: luna.model_display_name, selectable: true },
      catalog,
    });
    expect(draft.identity).toBe("conversation-b");
    expect(draft.draft).toEqual(standardLuna);
    expect(draft.dirty).toBe(false);
    expect(executionSettingsEqual(draft.baseline, standardLuna)).toBe(true);
  });
});

describe("provider settings menu", () => {
  test("renders one accessible pill and nested keyboard menus with disabled compatibility reasons", () => {
    let selected = fastSol;
    let control;
    const onSelect = vi.fn((next) => {
      selected = next;
      control.render({ visible: true, enabled: true, settings: selected, presentation: presentedSol, catalog });
    });
    control = createModelSettingsControl({ doc: document, onSelect });
    document.body.append(control.element);
    control.render({ visible: true, enabled: true, settings: selected, presentation: presentedSol, catalog });

    expect(control.button.textContent).toContain("5.6 Sol · High");
    expect(control.button.textContent).toContain("⚡");
    expect(control.button.getAttribute("aria-label")).toBe("Model 5.6 Sol, effort High, speed Fast");
    control.button.click();
    expect([...control.menu.querySelectorAll('[role="menuitem"]')].map((item) => item.dataset.settingsSection)).toEqual(["model", "effort", "speed"]);
    expect(control.menu.querySelector('[data-settings-section="model"] small')?.textContent).toBe("5.6 Sol");

    const modelRow = control.menu.querySelector('[data-settings-section="model"]');
    modelRow.dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowRight", bubbles: true, cancelable: true }));
    expect(control.menu.dataset.view).toBe("model");
    const lunaChoice = control.menu.querySelector(`[data-settings-value="${luna.model}"]`);
    expect(lunaChoice.getAttribute("aria-disabled")).toBe("true");
    expect(lunaChoice.textContent).toContain("Choose a supported effort and Standard speed first.");
    lunaChoice.click();
    expect(onSelect).not.toHaveBeenCalled();

    document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    expect(control.menu.hidden).toBe(true);
    expect(document.activeElement).toBe(control.button);
    control.destroy();
  });

  test("omits Effort and Speed when the selected model has only one effective choice", () => {
    const singletonCatalog = [{ ...luna, default: true }];
    const control = createModelSettingsControl({ doc: document, onSelect: vi.fn() });
    document.body.append(control.element);
    control.render({ visible: true, enabled: true, settings: standardLuna, presentation: { ...standardLuna, model_display_name: luna.model_display_name, selectable: true }, catalog: singletonCatalog });
    control.button.click();
    expect([...control.menu.querySelectorAll('[data-settings-section]')].map(({ dataset }) => dataset.settingsSection)).toEqual(["model"]);
    control.destroy();
  });

  test("retains Pi's multiple-effort menu while omitting its synthetic Speed choice", () => {
    const piCatalog = catalog.map((model) => ({ ...model, supports_fast: false }));
    const settings = { ...fastSol, speed: "standard" };
    const control = createModelSettingsControl({ doc: document, onSelect: vi.fn() });
    document.body.append(control.element);
    control.render({ visible: true, enabled: true, settings, presentation: { ...presentedSol, speed: "standard" }, catalog: piCatalog });
    control.button.click();
    expect([...control.menu.querySelectorAll('[data-settings-section]')].map(({ dataset }) => dataset.settingsSection)).toEqual(["model", "effort"]);
    control.destroy();
  });

  test("uses the selected draft model in both the pill and root menu when effective presentation is stale", () => {
    const control = createModelSettingsControl({ doc: document, onSelect: vi.fn() });
    document.body.append(control.element);
    control.render({ visible: true, enabled: true, settings: fastSol, presentation: { ...standardLuna, model_display_name: "GPT-5.5", selectable: true }, catalog });

    expect(control.button.getAttribute("aria-label")).toContain("5.6 Sol");
    control.button.click();
    expect(control.menu.querySelector('[data-settings-section="model"] small')?.textContent).toBe("5.6 Sol");
    expect(control.menu.textContent).not.toContain("GPT-5.5");
    control.destroy();
  });

  test("returns focus through submenus, keeps native text inert, and dismisses outside", () => {
    const hostile = { ...sol, model_display_name: "<img src=x onerror=alert(1)>", description: "<script>bad()</script>" };
    const safeCatalog = [hostile, luna];
    const settings = { model: hostile.model, effort: "high", speed: "standard" };
    const control = createModelSettingsControl({ doc: document, onSelect: vi.fn() });
    document.body.append(control.element);
    control.render({ visible: true, enabled: true, settings, presentation: { ...settings, model_display_name: hostile.model_display_name, selectable: true }, catalog: safeCatalog });
    control.button.click();
    control.menu.querySelector('[data-settings-section="effort"]').click();
    expect(control.menu.querySelector("img, script")).toBeNull();
    expect(document.activeElement?.getAttribute("role")).toBe("menuitemradio");
    control.menu.querySelector('[data-settings-back]').click();
    expect(document.activeElement?.dataset.settingsSection).toBe("effort");
    document.body.dispatchEvent(new Event("pointerdown", { bubbles: true }));
    expect(control.menu.hidden).toBe(true);
    control.destroy();
  });

  test("hides for Pi and disables unavailable, unverified, or removed Codex options while retaining presentation", () => {
    const control = createModelSettingsControl({ doc: document, onSelect: vi.fn() });
    document.body.append(control.element);
    control.render({ visible: false, enabled: false, settings: null, presentation: null, catalog: [] });
    expect(control.element.hidden).toBe(true);
    control.render({ visible: true, enabled: false, settings: fastSol, presentation: presentedSol, catalog: [] });
    expect(control.button.disabled).toBe(true);
    expect(control.button.getAttribute("aria-label")).toBe("Model 5.6 Sol, effort High, speed Fast. Model options unavailable");

    const removed = { model: "gpt-removed", effort: "high", speed: "standard", model_display_name: "Retired model", selectable: false };
    control.render({ visible: true, enabled: true, settings: removed, presentation: removed, catalog });
    expect(control.button.disabled).toBe(true);
    expect(control.button.textContent).toContain("Retired model · High");
    expect(control.button.getAttribute("aria-label")).toBe("Model Retired model, effort High, speed Standard. Model options unavailable");
    control.destroy();
  });
});
