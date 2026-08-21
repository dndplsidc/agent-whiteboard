export const CODEX_SETTINGS_STORAGE_KEY = "agent-whiteboard-codex-settings-v1";
export const PI_SETTINGS_STORAGE_KEY = "agent-whiteboard-pi-settings-v1";

export function settingsStorageKey(provider) {
  if (provider === "codex") return CODEX_SETTINGS_STORAGE_KEY;
  if (provider === "pi") return PI_SETTINGS_STORAGE_KEY;
  throw new TypeError("invalid settings provider");
}

const encoder = new TextEncoder();
const MAX_MODELS = 64;
const MAX_EFFORTS = 16;
const MAX_MODEL_BYTES = 256;
const MAX_EFFORT_BYTES = 64;
const MAX_TITLE_BYTES = 512;
const MAX_MODEL_DESCRIPTION_BYTES = 8 * 1024;
const MAX_EFFORT_DESCRIPTION_BYTES = 2 * 1024;
const MAX_CATALOG_BYTES = 128 * 1024;
const SPEEDS = new Set(["standard", "fast"]);

function isRecord(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function exactObject(value, required) {
  return isRecord(value) && Object.keys(value).length === required.length && required.every((key) => Object.hasOwn(value, key));
}

function validText(value, maximum, nonempty = true) {
  if (typeof value !== "string" || (nonempty && value.length === 0) || encoder.encode(value).length > maximum) return false;
  for (const character of value) {
    const code = character.codePointAt(0);
    if (code < 0x20 && character !== "\t" && character !== "\n" && character !== "\r") return false;
  }
  return true;
}

export function validExecutionSettings(value) {
  return exactObject(value, ["model", "effort", "speed"])
    && validText(value.model, MAX_MODEL_BYTES)
    && validText(value.effort, MAX_EFFORT_BYTES)
    && SPEEDS.has(value.speed);
}

export function validPresentedExecutionSettings(value) {
  return exactObject(value, ["model", "effort", "speed", "model_display_name", "selectable"])
    && validExecutionSettings({ model: value.model, effort: value.effort, speed: value.speed })
    && validText(value.model_display_name, MAX_TITLE_BYTES)
    && typeof value.selectable === "boolean";
}

function validEffortOption(value) {
  return exactObject(value, ["effort", "description"])
    && validText(value.effort, MAX_EFFORT_BYTES)
    && validText(value.description, MAX_EFFORT_DESCRIPTION_BYTES, false);
}

function validCatalogModel(value) {
  if (!exactObject(value, ["model", "model_display_name", "description", "default_effort", "supported_reasoning_efforts", "supports_images", "default", "supports_fast"])
    || !validText(value.model, MAX_MODEL_BYTES)
    || !validText(value.model_display_name, MAX_TITLE_BYTES)
    || !validText(value.description, MAX_MODEL_DESCRIPTION_BYTES, false)
    || !validText(value.default_effort, MAX_EFFORT_BYTES)
    || !Array.isArray(value.supported_reasoning_efforts)
    || value.supported_reasoning_efforts.length === 0
    || value.supported_reasoning_efforts.length > MAX_EFFORTS
    || !value.supported_reasoning_efforts.every(validEffortOption)
    || typeof value.supports_images !== "boolean"
    || typeof value.default !== "boolean"
    || typeof value.supports_fast !== "boolean") return false;
  const efforts = value.supported_reasoning_efforts.map(({ effort }) => effort);
  return new Set(efforts).size === efforts.length && efforts.includes(value.default_effort);
}

export function validModelCatalog(value) {
  if (!Array.isArray(value) || value.length > MAX_MODELS || !value.every(validCatalogModel)) return false;
  const models = value.map(({ model }) => model);
  if (new Set(models).size !== models.length || (value.length > 0 && value.filter(({ default: isDefault }) => isDefault).length !== 1)) return false;
  let total = 0;
  for (const model of value) {
    total += encoder.encode(model.model + model.model_display_name + model.description + model.default_effort).length;
    for (const effort of model.supported_reasoning_efforts) total += encoder.encode(effort.effort + effort.description).length;
    if (total > MAX_CATALOG_BYTES) return false;
  }
  return true;
}

export function cloneExecutionSettings(value) {
  return value === null || value === undefined ? null : { model: value.model, effort: value.effort, speed: value.speed };
}

export function executionSettingsEqual(left, right) {
  return left === null || left === undefined
    ? right === null || right === undefined
    : right !== null && right !== undefined && left.model === right.model && left.effort === right.effort && left.speed === right.speed;
}

function catalogModel(catalog, modelValue) {
  return catalog.find(({ model }) => model === modelValue) ?? null;
}

export function settingsCompatibility(catalog, settings) {
  if (!validModelCatalog(catalog) || !validExecutionSettings(settings)) return { compatible: false, reason: "catalog_invalid" };
  const model = catalogModel(catalog, settings.model);
  if (!model) return { compatible: false, reason: "model_unavailable" };
  if (!model.supported_reasoning_efforts.some(({ effort }) => effort === settings.effort)) return { compatible: false, reason: "effort_unsupported" };
  if (settings.speed === "fast" && !model.supports_fast) return { compatible: false, reason: "fast_unsupported" };
  return { compatible: true, reason: null };
}

export function modelCompatibility(catalog, settings, modelValue) {
  if (!validModelCatalog(catalog) || !validExecutionSettings(settings)) return { compatible: false, reason: "catalog_invalid", explanation: "Model options unavailable." };
  const model = catalogModel(catalog, modelValue);
  if (!model) return { compatible: false, reason: "model_unavailable", explanation: "This model is no longer available." };
  const effortBlocked = !model.supported_reasoning_efforts.some(({ effort }) => effort === settings.effort);
  const speedBlocked = settings.speed === "fast" && !model.supports_fast;
  if (effortBlocked && speedBlocked) return { compatible: false, reason: "effort_and_speed", explanation: "Choose a supported effort and Standard speed first." };
  if (effortBlocked) return { compatible: false, reason: "effort", explanation: "Choose a supported effort first." };
  if (speedBlocked) return { compatible: false, reason: "speed", explanation: "Choose Standard speed first." };
  return { compatible: true, reason: null, explanation: "" };
}

export function formatEffort(value) {
  if (value === "xhigh") return "Extra high";
  return value.length === 0 ? "" : `${value[0].toUpperCase()}${value.slice(1)}`;
}

export function formatExecutionSettings(settings) {
  if (!validPresentedExecutionSettings(settings)) return { visible: "Model options unavailable", accessible: "Model options unavailable", fast: false };
  const effort = formatEffort(settings.effort);
  const speed = settings.speed === "fast" ? "Fast" : "Standard";
  return {
    visible: `${settings.model_display_name} · ${effort}`,
    accessible: `Model ${settings.model_display_name}, effort ${effort}, speed ${speed}`,
    fast: settings.speed === "fast",
  };
}

export function readSettingsPreference(storage, provider) {
  try {
    const raw = storage?.getItem(settingsStorageKey(provider));
    if (raw === null || raw === undefined || encoder.encode(raw).length > 1024) return null;
    const decoded = JSON.parse(raw);
    return validExecutionSettings(decoded) ? cloneExecutionSettings(decoded) : null;
  } catch {
    return null;
  }
}

export function writeSettingsPreference(storage, provider, settings) {
  if (!validExecutionSettings(settings)) throw new TypeError("invalid settings preference");
  try {
    storage?.setItem(settingsStorageKey(provider), JSON.stringify(cloneExecutionSettings(settings)));
  } catch {
    // Model controls remain usable when browser storage is disabled.
  }
}

export function createSettingsDraftState(initialSettings = null) {
  if (initialSettings !== null && !validExecutionSettings(initialSettings)) throw new TypeError("invalid initial settings");
  return {
    identity: null,
    settingsState: null,
    baseline: null,
    effectivePresentation: null,
    draft: cloneExecutionSettings(initialSettings),
    catalog: [],
    dirty: initialSettings !== null,
    revision: 0,
    submissions: new Map(),
  };
}

export function editSettingsDraft(state, patch) {
  if (!isRecord(patch) || !state?.draft) throw new TypeError("settings are unavailable");
  const next = { ...state.draft, ...patch };
  if (!validExecutionSettings(next) || !settingsCompatibility(state.catalog, next).compatible) throw new TypeError("incompatible settings");
  state.draft = next;
  state.revision += 1;
  state.dirty = !executionSettingsEqual(state.draft, state.baseline);
  return cloneExecutionSettings(next);
}

export function recordSettingsSubmission(state, turnID) {
  if (typeof turnID !== "string" || turnID.length === 0 || !validExecutionSettings(state?.draft)) throw new TypeError("invalid submission settings");
  state.submissions.set(turnID, state.revision);
  while (state.submissions.size > 128) state.submissions.delete(state.submissions.keys().next().value);
  return state.revision;
}

export function reconcileSettingsDraft(state, { identity, settingsState, effectiveSettings, catalog, acceptedTurnID = null }) {
  if (typeof identity !== "string" || identity.length === 0 || !["verified", "unverified"].includes(settingsState) || !validModelCatalog(catalog)) throw new TypeError("invalid settings state");
  if (settingsState === "verified" && !validPresentedExecutionSettings(effectiveSettings) || settingsState === "unverified" && effectiveSettings !== null) throw new TypeError("invalid effective settings");
  const identityChanged = state.identity !== null && state.identity !== identity;
  const firstIdentity = state.identity === null;
  const wasClean = executionSettingsEqual(state.draft, state.baseline);
  state.identity = identity;
  state.settingsState = settingsState;
  state.catalog = catalog.map((model) => ({ ...model, supported_reasoning_efforts: model.supported_reasoning_efforts.map((effort) => ({ ...effort })) }));
  if (settingsState === "unverified") return state;

  const effective = cloneExecutionSettings(effectiveSettings);
  const submittedRevision = acceptedTurnID === null ? undefined : state.submissions.get(acceptedTurnID);
  const ownAcceptedWithoutLaterEdit = submittedRevision !== undefined && submittedRevision === state.revision;
  state.baseline = effective;
  state.effectivePresentation = { ...effectiveSettings };
  if (identityChanged || firstIdentity || wasClean || ownAcceptedWithoutLaterEdit) state.draft = effective;
  state.dirty = !executionSettingsEqual(state.draft, state.baseline);
  if (acceptedTurnID !== null) state.submissions.delete(acceptedTurnID);
  return state;
}

function menuButton(doc, { role = "menuitem", label, description = "", checked, disabled = false }) {
  const button = doc.createElement("button");
  button.type = "button";
  button.setAttribute("role", role);
  if (disabled) button.setAttribute("aria-disabled", "true");
  if (checked !== undefined) button.setAttribute("aria-checked", String(checked));
  const indicator = doc.createElement("span");
  indicator.className = "agent-model-menu-check";
  indicator.setAttribute("aria-hidden", "true");
  indicator.textContent = checked ? "✓" : "";
  const copy = doc.createElement("span");
  copy.className = "agent-model-menu-copy";
  const title = doc.createElement("strong");
  title.textContent = label;
  copy.append(title);
  if (description) {
    const detail = doc.createElement("small");
    detail.textContent = description;
    copy.append(detail);
  }
  button.append(indicator, copy);
  return button;
}

export function createModelSettingsControl({ doc = document, onSelect = () => {} } = {}) {
  const element = doc.createElement("div");
  element.className = "agent-model-control";
  const button = doc.createElement("button");
  button.type = "button";
  button.className = "agent-model-pill";
  button.setAttribute("aria-haspopup", "menu");
  button.setAttribute("aria-expanded", "false");
  const menu = doc.createElement("div");
  menu.className = "agent-model-menu";
  menu.setAttribute("role", "menu");
  menu.hidden = true;
  element.append(button, menu);

  let current = { visible: false, enabled: false, settings: null, presentation: null, catalog: [] };
  let view = "root";
  let returnSection = null;

  function focusableItems() {
    return [...menu.querySelectorAll('[role^="menuitem"]')];
  }

  function close({ restoreFocus = false } = {}) {
    menu.hidden = true;
    button.setAttribute("aria-expanded", "false");
    view = "root";
    if (restoreFocus) button.focus();
  }

  function openSubmenu(section) {
    view = section;
    returnSection = section;
    renderMenu();
    (menu.querySelector('[role="menuitemradio"][aria-checked="true"]') ?? menu.querySelector('[role="menuitemradio"]') ?? focusableItems()[0])?.focus();
  }

  function renderRoot() {
    const model = catalogModel(current.catalog, current.settings.model);
    const presentedModel = current.presentation?.model === current.settings.model ? current.presentation.model_display_name : null;
    const values = {
      model: presentedModel ?? model?.model_display_name ?? current.settings.model,
      effort: formatEffort(current.settings.effort),
      speed: current.settings.speed === "fast" ? "Fast" : "Standard",
    };
    const sections = current.catalog.some(({ supports_fast: supportsFast }) => supportsFast) ? ["model", "effort", "speed"] : ["model", "effort"];
    for (const section of sections) {
      const row = menuButton(doc, { label: `${section[0].toUpperCase()}${section.slice(1)}`, description: values[section] });
      row.dataset.settingsSection = section;
      row.setAttribute("aria-haspopup", "menu");
      row.addEventListener("click", () => openSubmenu(section));
      menu.append(row);
    }
  }

  function addBack() {
    const back = menuButton(doc, { label: "Back" });
    back.dataset.settingsBack = "true";
    back.addEventListener("click", () => {
      const section = returnSection;
      view = "root";
      renderMenu();
      menu.querySelector(`[data-settings-section="${section}"]`)?.focus();
    });
    menu.append(back);
  }

  function choose(next) {
    onSelect(cloneExecutionSettings(next));
    view = "root";
    renderMenu();
    menu.querySelector(`[data-settings-section="${returnSection}"]`)?.focus();
  }

  function renderModelChoices() {
    addBack();
    for (const model of current.catalog) {
      const compatibility = modelCompatibility(current.catalog, current.settings, model.model);
      const choice = menuButton(doc, {
        role: "menuitemradio",
        label: model.model_display_name,
        description: compatibility.compatible ? model.description : compatibility.explanation,
        checked: model.model === current.settings.model,
        disabled: !compatibility.compatible,
      });
      choice.dataset.settingsValue = model.model;
      choice.addEventListener("click", () => { if (compatibility.compatible) choose({ ...current.settings, model: model.model }); });
      menu.append(choice);
    }
  }

  function renderEffortChoices() {
    addBack();
    const model = catalogModel(current.catalog, current.settings.model);
    for (const effort of model?.supported_reasoning_efforts ?? []) {
      const choice = menuButton(doc, { role: "menuitemradio", label: formatEffort(effort.effort), description: effort.description, checked: effort.effort === current.settings.effort });
      choice.dataset.settingsValue = effort.effort;
      choice.addEventListener("click", () => choose({ ...current.settings, effort: effort.effort }));
      menu.append(choice);
    }
  }

  function renderSpeedChoices() {
    addBack();
    const model = catalogModel(current.catalog, current.settings.model);
    for (const speed of ["standard", "fast"]) {
      const disabled = speed === "fast" && !model?.supports_fast;
      const choice = menuButton(doc, {
        role: "menuitemradio",
        label: speed === "fast" ? "Fast" : "Standard",
        description: disabled ? "This model does not advertise Fast speed." : speed === "fast" ? "Use faster service when supported." : "Use standard service.",
        checked: speed === current.settings.speed,
        disabled,
      });
      choice.dataset.settingsValue = speed;
      choice.addEventListener("click", () => { if (!disabled) choose({ ...current.settings, speed }); });
      menu.append(choice);
    }
  }

  function renderMenu() {
    menu.replaceChildren();
    menu.dataset.view = view;
    if (view === "root") renderRoot();
    else if (view === "model") renderModelChoices();
    else if (view === "effort") renderEffortChoices();
    else renderSpeedChoices();
  }

  function render(next) {
    current = {
      visible: next.visible === true,
      enabled: next.enabled === true,
      settings: cloneExecutionSettings(next.settings),
      presentation: next.presentation ? { ...next.presentation } : null,
      catalog: Array.isArray(next.catalog) ? next.catalog : [],
    };
    element.hidden = !current.visible;
    const settingsValid = validExecutionSettings(current.settings);
    const compatible = settingsValid && settingsCompatibility(current.catalog, current.settings).compatible;
    const usable = current.enabled && compatible && current.catalog.length > 0;
    button.disabled = !usable;
    button.replaceChildren();
    if (!settingsValid) {
      button.textContent = "Model options unavailable";
      button.setAttribute("aria-label", "Model options unavailable");
      close();
      return;
    }
    const presented = current.presentation && current.presentation.model === current.settings.model
      ? { ...current.settings, model_display_name: current.presentation.model_display_name, selectable: current.presentation.selectable }
      : { ...current.settings, model_display_name: catalogModel(current.catalog, current.settings.model)?.model_display_name ?? current.settings.model, selectable: true };
    const formatted = formatExecutionSettings(presented);
    const label = doc.createElement("span");
    label.className = "agent-model-pill-label";
    label.textContent = formatted.visible;
    button.append(label);
    if (formatted.fast) {
      const lightning = doc.createElement("span");
      lightning.className = "agent-model-pill-fast";
      lightning.setAttribute("aria-hidden", "true");
      lightning.textContent = "⚡";
      button.append(lightning);
    }
    button.setAttribute("aria-label", usable ? formatted.accessible : `${formatted.accessible}. Model options unavailable`);
    button.title = usable ? "" : "Model options unavailable";
    if (!usable) close();
    else if (!menu.hidden) renderMenu();
  }

  function onButtonClick() {
    if (button.disabled) return;
    const opening = menu.hidden;
    if (!opening) { close({ restoreFocus: true }); return; }
    view = "root";
    renderMenu();
    menu.hidden = false;
    button.setAttribute("aria-expanded", "true");
    focusableItems()[0]?.focus();
  }

  function onPointerDown(event) {
    if (!element.contains(event.target)) close();
  }

  function onKeyDown(event) {
    if (menu.hidden) return;
    const target = event.target;
    if (event.key === "Escape") {
      event.preventDefault();
      close({ restoreFocus: true });
      return;
    }
    if (view === "root" && event.key === "ArrowRight" && target?.dataset?.settingsSection) {
      event.preventDefault();
      openSubmenu(target.dataset.settingsSection);
      return;
    }
    if (view !== "root" && event.key === "ArrowLeft") {
      event.preventDefault();
      const section = returnSection;
      view = "root";
      renderMenu();
      menu.querySelector(`[data-settings-section="${section}"]`)?.focus();
      return;
    }
    if (!["ArrowDown", "ArrowUp", "Home", "End"].includes(event.key) || !target?.getAttribute?.("role")?.startsWith("menuitem")) return;
    const items = focusableItems();
    const index = items.indexOf(target);
    if (items.length === 0) return;
    event.preventDefault();
    const next = event.key === "Home" ? 0 : event.key === "End" ? items.length - 1 : event.key === "ArrowDown" ? (index + 1) % items.length : (index - 1 + items.length) % items.length;
    items[next].focus();
  }

  button.addEventListener("click", onButtonClick);
  doc.addEventListener("pointerdown", onPointerDown);
  doc.addEventListener("keydown", onKeyDown);

  return {
    element,
    button,
    menu,
    render,
    close,
    destroy() {
      button.removeEventListener("click", onButtonClick);
      doc.removeEventListener("pointerdown", onPointerDown);
      doc.removeEventListener("keydown", onKeyDown);
      element.remove();
    },
  };
}
