// Run with: node --test cmd/bot/miniapp_ui_test.cjs
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { test } = require("node:test");
const source = fs.readFileSync(path.join(__dirname, "miniapp/app.js"), "utf8");

class Element {
  constructor() {
    this.children = [];
    this.listeners = {};
    this.dataset = {};
    this.value = "";
    this.classList = { toggle() {}, add() {}, remove() {} };
  }
  append(...children) { this.children.push(...children); }
  replaceChildren(...children) { this.children = children; }
  addEventListener(name, callback) { this.listeners[name] = callback; }
  setAttribute() {}
  focus() {}
  click() { return this.listeners.click(); }
}

async function fixture({ stock = 0, native = true, failure = false, popupError = false } = {}) {
  const elements = new Map();
  const el = (id) => {
    if (!elements.has(id)) elements.set(id, new Element());
    return elements.get(id);
  };
  el("admin-tab").dataset.view = "admin";
  let products = [{ sku: "KEY", name: "K".repeat(120), priceUsdt: 4.5, stock, active: true }];
  const requests = [];
  const popups = [];
  let browserConfirms = 0;
  const tg = {
    initData: "signed-test-data", ready() {}, expand() {}, setHeaderColor() {}, setBackgroundColor() {},
    isVersionAtLeast: () => native,
    showConfirm(message, callback) {
      if (popupError) throw new Error("Popup unavailable");
      popups.push({ message, callback });
    },
    HapticFeedback: { impactOccurred() {}, notificationOccurred() {} }
  };
  vm.runInNewContext(source, {
    window: { Telegram: { WebApp: tg }, scrollTo() {}, confirm() { browserConfirms++; return true; } },
    document: {
      getElementById: el,
      createElement: () => new Element(),
      querySelectorAll: (selector) => selector === "[data-view]" ? [el("admin-tab")] : []
    },
    async fetch(url, options) {
      requests.push({ url, ...options });
      if (options.method === "DELETE") {
        if (failure) return { ok: false, json: async () => ({ error: "could not delete product" }) };
        products = [];
        return { ok: true, json: async () => { throw new SyntaxError("empty 204 response"); } };
      }
      return { ok: true, json: async () => ({ products, isOwner: true }) };
    }
  });
  await new Promise(setImmediate);
  el("admin-tab").click();
  await new Promise(setImmediate);
  const find = (root, className) => root.className === className ? root : root.children.map((child) => find(child, className)).find(Boolean);
  return {
    el, requests, popups,
    remove: find(el("admin-products"), "delete-product"),
    get browserConfirms() { return browserConfirms; },
    deletes: () => requests.filter((request) => request.method === "DELETE")
  };
}

for (const stock of [0, 3]) {
  test(`Telegram deletion works with ${stock} available stock and waits for native confirmation`, async () => {
    const ui = await fixture({ stock });
    const pending = ui.remove.click();
    assert.equal(ui.popups.length, 1);
    assert.ok(ui.popups[0].message.length <= 256, "Telegram limits popup messages to 256 characters");
    assert.equal(ui.browserConfirms, 0, "must not invoke the suppressed browser dialog");
    assert.equal(ui.deletes().length, 0);
    await ui.remove.click();
    assert.equal(ui.popups.length, 1, "double taps must not open another dialog");
    ui.popups[0].callback(true);
    await pending;
    assert.equal(ui.deletes().length, 1);
    assert.equal(ui.deletes()[0].url, "api/mini-app/admin/products/KEY");
    assert.equal(ui.deletes()[0].headers.Authorization, "tma signed-test-data");
    assert.equal(ui.el("admin-product-metric").textContent, "0");
    assert.equal(ui.el("catalog-count").textContent, "0 products");
    assert.equal(ui.el("notice").textContent, "Product deleted. Existing orders are preserved.");
  });
}

test("cancelling native confirmation leaves the product untouched and permits a retry", async () => {
  const ui = await fixture();
  const pending = ui.remove.click();
  ui.popups[0].callback(false);
  await pending;
  assert.equal(ui.deletes().length, 0);
  assert.equal(ui.el("admin-product-metric").textContent, "1");
  const retry = ui.remove.click();
  assert.equal(ui.popups.length, 2);
  ui.popups[1].callback(true);
  await retry;
  assert.equal(ui.deletes().length, 1);
});

test("server errors are visible and leave the listing in place", async () => {
  const ui = await fixture({ failure: true });
  const pending = ui.remove.click();
  ui.popups[0].callback(true);
  await pending;
  assert.equal(ui.el("notice").textContent, "could not delete product");
  assert.equal(ui.el("admin-product-metric").textContent, "1");
});

test("older clients fall back to a browser confirmation", async () => {
  const ui = await fixture({ native: false });
  await ui.remove.click();
  assert.equal(ui.browserConfirms, 1);
  assert.equal(ui.popups.length, 0);
  assert.equal(ui.deletes().length, 1);
});

test("native popup failure is reported without deleting", async () => {
  const ui = await fixture({ popupError: true });
  await ui.remove.click();
  assert.equal(ui.el("notice").textContent, "Popup unavailable");
  assert.equal(ui.deletes().length, 0);
});
