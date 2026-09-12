(() => {
  "use strict";

  const tg = window.Telegram?.WebApp;
  const state = { products: [], selected: null, order: null };
  const el = (id) => document.getElementById(id);
  const views = ["shop", "orders", "checkout", "success"];

  if (tg) {
    tg.ready();
    tg.expand();
    tg.setHeaderColor("secondary_bg_color");
    tg.setBackgroundColor("secondary_bg_color");
  }

  function haptic(kind = "light") { tg?.HapticFeedback?.impactOccurred(kind); }
  function showNotice(message) {
    el("notice").textContent = message;
    el("notice").classList.toggle("hidden", !message);
  }
  function showView(name) {
    views.forEach((view) => el(`${view}-view`).classList.toggle("hidden", view !== name));
    el("tabs").classList.toggle("hidden", name === "checkout" || name === "success");
    document.querySelectorAll("[data-view]").forEach((button) => button.classList.toggle("active", button.dataset.view === name));
    el("title").textContent = name === "orders" ? "My orders" : "Digital goods";
    window.scrollTo({ top: 0, behavior: "smooth" });
  }

  async function api(path, options = {}) {
    if (!tg?.initData) throw new Error("Open this Mini App from the Telegram bot to continue.");
    const response = await fetch(path, {
      ...options,
      headers: { "Authorization": `tma ${tg.initData}`, "Content-Type": "application/json", ...(options.headers || {}) }
    });
    const body = await response.json().catch(() => ({}));
    if (!response.ok) throw new Error(body.error || "The shop is temporarily unavailable.");
    return body;
  }

  function node(tag, className, text) {
    const value = document.createElement(tag);
    if (className) value.className = className;
    if (text !== undefined) value.textContent = text;
    return value;
  }

  function empty(message) {
    return node("div", "empty", message);
  }

  function renderCatalog() {
    const root = el("catalog");
    root.replaceChildren();
    if (!state.products.length) {
      root.append(empty("No products are available right now. Check again after the next restock."));
      return;
    }
    state.products.forEach((product) => {
      const card = node("button", "product");
      card.type = "button";
      card.disabled = product.stock < 1;
      card.append(node("span", "product-code", product.sku));
      card.append(node("h2", "", product.name));
      card.append(node("p", "product-description", product.description));
      const footer = node("div", "product-footer");
      const price = node("span", "price", `${product.priceUsdt.toFixed(2)} USDT`);
      price.append(node("small", "", "per item"));
      const stock = node("span", `stock${product.stock ? "" : " sold-out"}`, product.stock ? `${product.stock} available` : "Sold out");
      footer.append(price, stock);
      card.append(footer);
      if (product.stock) card.addEventListener("click", () => openCheckout(product));
      root.append(card);
    });
  }

  async function loadCatalog() {
    showNotice("");
    el("catalog").replaceChildren(empty("Loading current stock…"));
    try {
      const data = await api("api/mini-app/catalog");
      state.products = data.products || [];
      renderCatalog();
    } catch (error) {
      el("catalog").replaceChildren(empty(error.message));
      showNotice(error.message);
    }
  }

  function openCheckout(product) {
    state.selected = product;
    el("checkout-name").textContent = product.name;
    el("checkout-description").textContent = product.description;
    el("quantity").value = "1";
    el("quantity").max = String(Math.min(product.stock, 50));
    updateTotal();
    haptic();
    showView("checkout");
  }

  function quantity() {
    const limit = Math.min(state.selected?.stock || 1, 50);
    const value = Math.max(1, Math.min(limit, Number.parseInt(el("quantity").value, 10) || 1));
    el("quantity").value = String(value);
    return value;
  }

  function updateTotal() {
    el("checkout-total").textContent = `${((state.selected?.priceUsdt || 0) * quantity()).toFixed(2)} USDT`;
  }

  function receiptRow(label, value) {
    const row = node("div", "receipt-row");
    row.append(node("span", "", label), node("strong", "", value));
    return row;
  }

  function showOrder(order) {
    state.order = order;
    const summary = el("order-summary");
    summary.replaceChildren(
      receiptRow("Order", order.id),
      receiptRow("Product", `${order.name || order.sku} × ${order.quantity}`),
      receiptRow("Amount", `${order.amount.toFixed(2)} USDT`),
      receiptRow("Network", order.network === "polygon" ? "Polygon" : "BEP-20"),
      receiptRow("Wallet", order.wallet || "See bot chat")
    );
    showView("success");
  }

  async function placeOrder() {
    const button = el("place-order");
    button.disabled = true;
    button.textContent = "Reserving…";
    showNotice("");
    try {
      const network = document.querySelector('input[name="network"]:checked').value;
      const data = await api("api/mini-app/orders", { method: "POST", body: JSON.stringify({ sku: state.selected.sku, quantity: quantity(), network }) });
      haptic("medium");
      showOrder(data.order);
    } catch (error) {
      showNotice(error.message);
      tg?.HapticFeedback?.notificationOccurred("error");
    } finally {
      button.disabled = false;
      button.textContent = "Reserve stock";
    }
  }

  async function loadOrders() {
    showView("orders");
    showNotice("");
    const root = el("orders");
    root.replaceChildren(empty("Loading your orders…"));
    try {
      const data = await api("api/mini-app/orders");
      root.replaceChildren();
      if (!data.orders?.length) {
        root.append(empty("You have no orders yet."));
        return;
      }
      data.orders.forEach((order) => {
        const card = node("article", "order");
        const head = node("div", "order-head");
        head.append(node("strong", "", order.name || order.sku), node("span", "order-status", order.status));
        const meta = node("div", "order-meta");
        meta.append(node("span", "", `${order.quantity} × · ${order.amount.toFixed(2)} USDT`), node("span", "", order.id));
        card.append(head, meta);
        root.append(card);
      });
    } catch (error) {
      root.replaceChildren(empty(error.message));
      showNotice(error.message);
    }
  }

  el("refresh").addEventListener("click", loadCatalog);
  el("checkout-back").addEventListener("click", () => showView("shop"));
  el("minus").addEventListener("click", () => { el("quantity").value = String(quantity() - 1); updateTotal(); });
  el("plus").addEventListener("click", () => { el("quantity").value = String(quantity() + 1); updateTotal(); });
  el("quantity").addEventListener("change", updateTotal);
  el("place-order").addEventListener("click", placeOrder);
  el("copy-wallet").addEventListener("click", async () => {
    if (!state.order?.wallet) return;
    await navigator.clipboard.writeText(state.order.wallet);
    el("copy-wallet").textContent = "Wallet copied";
    haptic();
  });
  el("close-app").addEventListener("click", () => tg?.close());
  document.querySelectorAll("[data-view]").forEach((button) => button.addEventListener("click", () => button.dataset.view === "orders" ? loadOrders() : showView("shop")));

  loadCatalog();
})();
