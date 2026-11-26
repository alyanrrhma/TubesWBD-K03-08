const routes = [];
export function route(pattern, render) { routes.push({ pattern, render }); }
export function navigate(path) {
  history.pushState({}, "", path);
  window.dispatchEvent(new PopStateEvent("popstate"));
}
function match(path) {
  for (const item of routes) {
    const names = [];
    const source = item.pattern.replace(/:[A-Za-z0-9_]+/g, (part) => {
      names.push(part.slice(1)); return "([^/]+)";
    });
    const found = path.match(new RegExp(`^${source}/?$`));
    if (found) return { render: item.render, params: Object.fromEntries(names.map((n,i)=>[n,decodeURIComponent(found[i+1])])) };
  }
  return null;
}
export function startRouter(root, shell) {
  async function render() {
    const path = location.pathname;
    const found = match(path);
    if (!found) { root.innerHTML = shell("Halaman tidak ditemukan", `<p>Rute <code>${escapeHTML(path)}</code> tidak tersedia.</p>`); return; }
    try { await found.render(root, found.params); }
    catch (error) { root.innerHTML = shell("Terjadi kesalahan", `<div class="flash flash-error" role="alert">${escapeHTML(error.message)}</div><button class="btn" data-back>Kembali</button>`); }
  }
  window.addEventListener("popstate", render);
  root.addEventListener("click", (event) => {
    const link = event.target.closest("a[data-link]");
    if (link && link.origin === location.origin) { event.preventDefault(); navigate(link.pathname + link.search); }
  });
  root.addEventListener("click", (event) => {
    if (event.target.closest("[data-back]")) history.back();
  });
  render();
}
export function escapeHTML(value) {
  return String(value ?? "").replace(/[&<>"']/g, c => ({ "&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;" }[c]));
}
