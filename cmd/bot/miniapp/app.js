(() => {
  "use strict";

  const tg = window.Telegram?.WebApp;
  const state = { products: [], selected: null, order: null, isOwner: false, adminProducts: [], stock: [] };
  const el = (id) => document.getElementById(id);
  const views = ["shop", "orders", "checkout", "success", "admin"];

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
    const titles = { shop: "Digital goods", orders: "My orders", checkout: "Checkout", success: "Payment", admin: "Store admin" };
    el("title").textContent = titles[name] || "Digital goods";
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
      state.isOwner = Boolean(data.isOwner);
      el("admin-tab").classList.toggle("hidden", !state.isOwner);
      el("tabs").classList.toggle("admin-enabled", state.isOwner);
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
    button.textContent = "Buying…";
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
      button.textContent = "Buy";
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

  function selectedAdminSKU() {
    return el("stock-sku").value || state.adminProducts[0]?.sku || "";
  }

  function renderAdminProducts() {
    const root = el("admin-products");
    root.replaceChildren();
    el("admin-product-count").textContent = `${state.adminProducts.length} products`;
    if (!state.adminProducts.length) {
      root.append(empty("Create your first product above."));
      return;
    }
    state.adminProducts.forEach((product) => {
      const card = node("article", "admin-product");
      const copy = node("div");
      copy.append(node("strong", "", product.name));
      copy.append(node("small", "", `${product.sku} · ${product.priceUsdt.toFixed(2)} USDT · ${product.stock} available · ${product.active ? "active" : "hidden"}`));
      const edit = node("button", "edit-price", "Edit price");
      edit.type = "button";
      edit.addEventListener("click", () => updateProductPrice(product));
      card.append(copy, edit);
      root.append(card);
    });
  }

  function renderStockSelect() {
    const select = el("stock-sku");
    const prior = select.value;
    select.replaceChildren();
    state.adminProducts.forEach((product) => {
      const option = node("option", "", `${product.name} (${product.sku})`);
      option.value = product.sku;
      select.append(option);
    });
    if (prior && state.adminProducts.some((product) => product.sku === prior)) select.value = prior;
  }

  function renderAdminStock() {
    const root = el("admin-stock");
    root.replaceChildren();
    el("admin-stock-count").textContent = `${state.stock.length} items`;
    if (!state.stock.length) {
      root.append(empty("No stock items for this product."));
      return;
    }
    state.stock.forEach((item) => {
      const card = node("article", "admin-stock-item");
      const copy = node("div");
      copy.append(node("strong", "", item.id));
      copy.append(node("small", "", `${item.sku} · added ${new Date(item.addedAt).toLocaleString()}`));
      const actions = node("div");
      actions.append(node("span", `state ${item.state}`, item.state));
      if (item.state === "available") {
        const remove = node("button", "remove-stock", "Remove");
        remove.type = "button";
        remove.addEventListener("click", () => removeStock(item.id));
        actions.append(remove);
      }
      card.append(copy, actions);
      root.append(card);
    });
  }

  async function loadAdminStock() {
    const sku = selectedAdminSKU();
    if (!sku) {
      state.stock = [];
      renderAdminStock();
      return;
    }
    const root = el("admin-stock");
    root.replaceChildren(empty("Loading stock…"));
    try {
      const data = await api(`api/mini-app/admin/stock?sku=${encodeURIComponent(sku)}`);
      state.stock = data.stock || [];
      renderAdminStock();
    } catch (error) {
      root.replaceChildren(empty(error.message));
      showNotice(error.message);
    }
  }

  async function loadAdmin() {
    if (!state.isOwner) return;
    showView("admin");
    showNotice("");
    el("admin-products").replaceChildren(empty("Loading products…"));
    try {
      const data = await api("api/mini-app/admin/products");
      state.adminProducts = data.products || [];
      renderAdminProducts();
      renderStockSelect();
      await loadAdminStock();
    } catch (error) {
      el("admin-products").replaceChildren(empty(error.message));
      showNotice(error.message);
    }
  }

  async function createProduct(event) {
    event.preventDefault();
    const button = el("create-product");
    button.disabled = true;
    showNotice("");
    try {
      await api("api/mini-app/admin/products", { method: "POST", body: JSON.stringify({
        sku: el("admin-sku").value,
        name: el("admin-name").value,
        priceUsdt: Number(el("admin-price").value),
        description: el("admin-description").value,
        deliveryInstruction: el("admin-instructions").value
      }) });
      el("product-form").reset();
      haptic("medium");
      await loadAdmin();
      showNotice("Product created.");
    } catch (error) {
      showNotice(error.message);
      tg?.HapticFeedback?.notificationOccurred("error");
    } finally {
      button.disabled = false;
    }
  }

  async function updateProductPrice(product) {
    const entered = window.prompt(`New USDT price for ${product.name}`, product.priceUsdt.toFixed(2));
    if (entered === null) return;
    const priceUsdt = Number(entered);
    if (!Number.isFinite(priceUsdt) || priceUsdt <= 0) {
      showNotice("Enter a positive price in USDT.");
      return;
    }
    showNotice("");
    try {
      await api(`api/mini-app/admin/products/${encodeURIComponent(product.sku)}`, { method: "PATCH", body: JSON.stringify({ priceUsdt }) });
      haptic("medium");
      await loadAdmin();
      showNotice("Product price updated.");
    } catch (error) {
      showNotice(error.message);
      tg?.HapticFeedback?.notificationOccurred("error");
    }
  }

  async function addStock(event) {
    event.preventDefault();
    const button = el("add-stock");
    button.disabled = true;
    showNotice("");
    try {
      const data = await api("api/mini-app/admin/stock", { method: "POST", body: JSON.stringify({ sku: selectedAdminSKU(), payloads: el("stock-payloads").value }) });
      el("stock-payloads").value = "";
      haptic("medium");
      await loadAdmin();
      showNotice(`${data.added} stock item${data.added === 1 ? "" : "s"} added.`);
    } catch (error) {
      showNotice(error.message);
      tg?.HapticFeedback?.notificationOccurred("error");
    } finally {
      button.disabled = false;
    }
  }

  async function removeStock(id) {
    if (!window.confirm("Remove this available stock item? This cannot be undone.")) return;
    showNotice("");
    try {
      await api(`api/mini-app/admin/stock/${encodeURIComponent(id)}`, { method: "DELETE" });
      haptic("medium");
      await loadAdmin();
      showNotice("Stock item removed.");
    } catch (error) {
      showNotice(error.message);
      tg?.HapticFeedback?.notificationOccurred("error");
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
  el("admin-refresh").addEventListener("click", loadAdmin);
  el("product-form").addEventListener("submit", createProduct);
  el("stock-form").addEventListener("submit", addStock);
  el("stock-sku").addEventListener("change", loadAdminStock);
  document.querySelectorAll("[data-view]").forEach((button) => button.addEventListener("click", () => {
    if (button.dataset.view === "orders") loadOrders();
    else if (button.dataset.view === "admin") loadAdmin();
    else showView("shop");
  }));

  loadCatalog();
})();
