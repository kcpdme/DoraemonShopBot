(() => {
  "use strict";

  const tg = window.Telegram?.WebApp;
  const state = { products: [], selected: null, order: null, isOwner: false, adminProducts: [], stock: [], adminOrders: [], adminSection: "overview", catalogQuery: "", catalogFilter: "all" };
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

  function productInitials(product) {
    const words = String(product.name || product.sku || "").trim().split(/\s+/).filter(Boolean);
    return (words.length > 1 ? `${words[0][0]}${words[1][0]}` : words[0]?.slice(0, 2) || "DG").toUpperCase();
  }

  function productTone(product) {
    const value = Array.from(String(product.sku || product.name || "")).reduce((total, character) => total + character.charCodeAt(0), 0);
    return value % 4;
  }

  function visibleProducts() {
    const query = state.catalogQuery.trim().toLowerCase();
    return state.products.filter((product) => {
      if (state.catalogFilter === "available" && product.stock < 1) return false;
      if (state.catalogFilter === "low" && (product.stock < 1 || product.stock > 5)) return false;
      return !query || [product.name, product.sku, product.description].join(" ").toLowerCase().includes(query);
    });
  }

  function renderCatalog() {
    const root = el("catalog");
    root.replaceChildren();
    const available = state.products.filter((product) => product.stock > 0).length;
    const lowStock = state.products.filter((product) => product.stock > 0 && product.stock <= 5).length;
    el("filter-all-count").textContent = String(state.products.length);
    el("filter-available-count").textContent = String(available);
    el("filter-low-count").textContent = String(lowStock);
    if (!state.products.length) {
      el("catalog-count").textContent = "0 products";
      root.append(empty("No products are available right now. Check again after the next restock."));
      return;
    }
    const products = visibleProducts();
    el("catalog-count").textContent = `${products.length} ${products.length === 1 ? "product" : "products"}`;
    if (!products.length) {
      root.append(empty("No products match those filters. Try another search."));
      return;
    }
    products.forEach((product) => {
      const card = node("article", `product${product.stock ? "" : " unavailable"}`);
      const visual = node("div", `product-visual tone-${productTone(product)}`, productInitials(product));
      visual.setAttribute("aria-hidden", "true");

      const content = node("div", "product-content");
      const heading = node("div", "product-heading");
      const identity = node("div", "product-identity");
      identity.append(node("span", "product-code", product.sku), node("h2", "", product.name));
      const stockClass = product.stock > 5 ? "healthy" : product.stock > 0 ? "low" : "sold-out";
      const stockLabel = product.stock > 0 ? `${product.stock} left` : "Sold out";
      heading.append(identity, node("span", `stock ${stockClass}`, stockLabel));
      content.append(heading, node("p", "product-description", product.description));

      const footer = node("div", "product-footer");
      const price = node("span", "price", `${product.priceUsdt.toFixed(2)} USDT`);
      price.append(node("small", "", "per item"));
      const buy = node("button", `product-buy${product.stock ? "" : " sold-out"}`, product.stock ? "Buy" : "Sold out");
      buy.type = "button";
      buy.disabled = product.stock < 1;
      buy.setAttribute("aria-label", product.stock ? `Buy ${product.name}` : `${product.name} is sold out`);
      if (product.stock) buy.addEventListener("click", () => openCheckout(product));
      footer.append(price, buy);
      content.append(footer);
      card.append(visual, content);
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

  function setAdminSection(name, loadStock = true) {
    state.adminSection = name;
    document.querySelectorAll("[data-admin-panel]").forEach((panel) => panel.classList.toggle("hidden", panel.dataset.adminPanel !== name));
    document.querySelectorAll("[data-admin-section]").forEach((button) => {
      const active = button.dataset.adminSection === name;
      button.classList.toggle("active", active);
      button.setAttribute("aria-current", active ? "page" : "false");
    });
    if (name === "stock" && loadStock) loadAdminStock();
    if (name === "orders" && loadStock) loadAdminOrders();
  }

  function stockPriority(product) {
    if (product.stock === 0) return "Out of stock";
    return `${product.stock} left`;
  }

  function openStockFor(sku, revealForm = false) {
    if (!state.adminProducts.length) {
      setAdminSection("products", false);
      showNotice("Create a product before adding stock.");
      return;
    }
    setAdminSection("stock", false);
    if (sku && state.adminProducts.some((product) => product.sku === sku)) el("stock-sku").value = sku;
    el("stock-form").classList.toggle("hidden", !revealForm);
    loadAdminStock();
    if (revealForm) el("stock-payloads").focus();
  }

  function renderAdminOverview() {
    const products = state.adminProducts;
    const available = products.reduce((total, product) => total + product.stock, 0);
    const lowStock = products.filter((product) => product.active !== false && product.stock <= 2).sort((a, b) => a.stock - b.stock || a.name.localeCompare(b.name));
    el("admin-product-metric").textContent = String(products.length);
    el("admin-stock-metric").textContent = String(available);
    el("admin-low-stock-metric").textContent = String(lowStock.length);

    const root = el("admin-attention");
    root.replaceChildren();
    if (!products.length) {
      root.append(empty("No products yet. Create the first product to start selling."));
      return;
    }
    if (!lowStock.length) {
      const clear = node("div", "attention-clear");
      clear.append(node("span", "attention-check", "✓"), node("div", "", "Every active product has enough stock."));
      root.append(clear);
      return;
    }
    lowStock.forEach((product) => {
      const row = node("button", `attention-item${product.stock === 0 ? " urgent" : ""}`);
      row.type = "button";
      const copy = node("span", "attention-copy");
      copy.append(node("strong", "", product.name), node("small", "", product.sku));
      row.append(copy, node("span", "attention-state", stockPriority(product)));
      row.addEventListener("click", () => openStockFor(product.sku, true));
      root.append(row);
    });
  }

  function renderAdminProducts() {
    const root = el("admin-products");
    root.replaceChildren();
    const query = el("admin-product-search").value.trim().toLowerCase();
    const products = state.adminProducts.filter((product) => !query || product.name.toLowerCase().includes(query) || product.sku.toLowerCase().includes(query));
    el("admin-product-count").textContent = query ? `${products.length} results` : `${products.length} products`;
    if (!state.adminProducts.length) {
      root.append(empty("No products yet. Use New product to create one."));
      return;
    }
    if (!products.length) {
      root.append(empty("No products match this search."));
      return;
    }
    products.forEach((product) => {
      const card = node("article", "admin-product");
      const copy = node("div", "product-main");
      const heading = node("div", "product-admin-heading");
      heading.append(node("strong", "", product.name), node("span", `visibility-status ${product.active ? "active" : "hidden-product"}`, product.active ? "Active" : "Hidden"));
      const facts = node("div", "product-facts");
      facts.append(node("span", "", product.sku), node("span", "", `${product.priceUsdt.toFixed(2)} USDT`), node("span", product.stock <= 2 ? "low" : "", `${product.stock} available`));
      copy.append(heading, facts);
      const edit = node("button", "edit-price", "Edit price");
      edit.type = "button";
      edit.addEventListener("click", () => updateProductPrice(product));
      const remove = node("button", "delete-product", "Delete");
      remove.type = "button";
      remove.addEventListener("click", () => deleteProduct(product));
      const actions = node("div", "admin-actions");
      actions.append(edit, remove);
      card.append(copy, actions);
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
    const available = state.stock.filter((item) => item.state === "available").length;
    el("admin-stock-count").textContent = `${available} available · ${state.stock.length} total`;
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

  function adminStatus(status) {
    return String(status || "unknown").replaceAll("_", " ");
  }

  function adminOrderTime(value) {
    if (!value) return "—";
    const date = new Date(value);
    return Number.isNaN(date.getTime()) ? "—" : date.toLocaleString();
  }

  function transactionURL(order) {
    if (!/^0x[0-9a-f]{64}$/i.test(order.txHash || "")) return "";
    if (order.network === "polygon") return `https://polygonscan.com/tx/${order.txHash}`;
    if (order.network === "bep20") return `https://bscscan.com/tx/${order.txHash}`;
    return "";
  }

  function orderDetail(label, value) {
    const row = node("div", "order-detail");
    if (label === "TxID" || label === "Latest update") row.classList.add("wide");
    row.append(node("span", "", label), node("strong", "", value));
    return row;
  }

  function renderAdminOrders() {
    const root = el("admin-orders");
    root.replaceChildren();
    const query = el("admin-order-search").value.trim().toLowerCase();
    const orders = state.adminOrders.filter((order) => {
      const searchable = [order.id, order.name, order.sku, order.buyer, order.buyerId, order.txHash, order.status, order.paymentMethod].join(" ").toLowerCase();
      return !query || searchable.includes(query);
    });
    el("admin-order-count").textContent = query ? `${orders.length} results` : `${orders.length} orders`;
    if (!state.adminOrders.length) {
      root.append(empty("No orders have been created yet."));
      return;
    }
    if (!orders.length) {
      root.append(empty("No orders match this search."));
      return;
    }
    orders.forEach((order) => {
      const card = node("article", "admin-order");
      const head = node("div", "admin-order-head");
      const title = node("div", "order-main");
      title.append(node("strong", "", `${order.name || order.sku} × ${order.quantity}`), node("small", "", `${order.amount.toFixed(2)} USDT · ${order.id}`));
      head.append(title, node("span", `order-state ${String(order.status || "unknown")}`, adminStatus(order.status)));

      const buyer = node("div", "order-buyer");
      buyer.append(node("span", "", "Buyer"), node("strong", "", `${order.buyer || "Unknown buyer"} · ID ${order.buyerId || "—"}`));

      const details = node("div", "order-details");
      details.append(
        orderDetail("Payment", order.paymentMethod || "—"),
        orderDetail("Created", adminOrderTime(order.createdAt))
      );
      if (order.paidAt) details.append(orderDetail("Paid", adminOrderTime(order.paidAt)));
      if (order.txHash) {
        const tx = orderDetail("TxID", order.txHash);
        const url = transactionURL(order);
        if (url) {
          const link = node("a", "transaction-link", "View transaction");
          link.href = url;
          link.target = "_blank";
          link.rel = "noopener noreferrer";
          tx.append(link);
        }
        details.append(tx);
      }
      if (order.paymentIssue) details.append(orderDetail("Latest update", order.paymentIssue));
      card.append(head, buyer, details);
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

  async function loadAdminOrders() {
    const root = el("admin-orders");
    root.replaceChildren(empty("Loading orders…"));
    try {
      const data = await api("api/mini-app/admin/orders");
      state.adminOrders = data.orders || [];
      renderAdminOrders();
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
    el("admin-attention").replaceChildren(empty("Loading store summary…"));
    try {
      const data = await api("api/mini-app/admin/products");
      state.adminProducts = data.products || [];
      renderAdminOverview();
      renderAdminProducts();
      renderStockSelect();
      setAdminSection(state.adminSection, false);
      if (state.adminSection === "stock") await loadAdminStock();
      if (state.adminSection === "orders") await loadAdminOrders();
    } catch (error) {
      el("admin-products").replaceChildren(empty(error.message));
      el("admin-attention").replaceChildren(empty(error.message));
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
      el("product-form").classList.add("hidden");
      state.adminSection = "products";
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
      state.adminSection = "products";
      haptic("medium");
      await loadAdmin();
      showNotice("Product price updated.");
    } catch (error) {
      showNotice(error.message);
      tg?.HapticFeedback?.notificationOccurred("error");
    }
  }

  async function deleteProduct(product) {
    const confirmed = window.confirm(`Delete ${product.name}? This permanently removes the product and all of its stock items.`);
    if (!confirmed) return;
    showNotice("");
    try {
      await api(`api/mini-app/admin/products/${encodeURIComponent(product.sku)}`, { method: "DELETE" });
      state.adminSection = "products";
      haptic("medium");
      await loadAdmin();
      showNotice("Product and its stock were deleted.");
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
      el("stock-form").classList.add("hidden");
      state.adminSection = "stock";
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
      state.adminSection = "stock";
      haptic("medium");
      await loadAdmin();
      showNotice("Stock item removed.");
    } catch (error) {
      showNotice(error.message);
      tg?.HapticFeedback?.notificationOccurred("error");
    }
  }

  el("refresh").addEventListener("click", loadCatalog);
  el("catalog-search").addEventListener("input", (event) => {
    state.catalogQuery = event.target.value;
    renderCatalog();
  });
  document.querySelectorAll("[data-catalog-filter]").forEach((button) => button.addEventListener("click", () => {
    state.catalogFilter = button.dataset.catalogFilter;
    document.querySelectorAll("[data-catalog-filter]").forEach((option) => option.classList.toggle("active", option === button));
    haptic();
    renderCatalog();
  }));
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
  document.querySelectorAll("[data-admin-section]").forEach((button) => button.addEventListener("click", () => setAdminSection(button.dataset.adminSection)));
  el("overview-new-product").addEventListener("click", () => {
    setAdminSection("products", false);
    el("product-form").classList.remove("hidden");
    el("admin-sku").focus();
  });
  el("open-product-form").addEventListener("click", () => {
    el("product-form").classList.remove("hidden");
    el("admin-sku").focus();
  });
  el("cancel-product-form").addEventListener("click", () => el("product-form").classList.add("hidden"));
  el("overview-add-stock").addEventListener("click", () => openStockFor("", true));
  el("open-stock-form").addEventListener("click", () => openStockFor(selectedAdminSKU(), true));
  el("cancel-stock-form").addEventListener("click", () => el("stock-form").classList.add("hidden"));
  el("admin-product-search").addEventListener("input", renderAdminProducts);
  el("admin-order-search").addEventListener("input", renderAdminOrders);
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
