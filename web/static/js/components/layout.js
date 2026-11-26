import { escapeHTML } from "../router.js";
export function shell(user, title, content) {
  const path = location.pathname;
  const active = (route) => path === route ? ' aria-current="page"' : "";
  document.title = `${title} – Database Platform`;
  const nav = user ? `<header class="topbar">
    <a class="brand" href="/dashboard" data-link>Database Platform</a>
    <nav aria-label="Navigasi utama">
      <a href="/dashboard" data-link${path === "/dashboard" || path.startsWith("/databases/") ? ' aria-current="page"' : ""}>Database</a>
      ${user.is_platform_admin ? `
        <a href="/admin/users#users" data-link${active("/admin/users")}>Pengguna</a>
        <a href="/admin/groups#groups" data-link${active("/admin/groups")}>Group</a>
        <a href="/admin/connections#connections" data-link${active("/admin/connections")}>Koneksi</a>` : ""}
    </nav>
    <div class="userbox"><span>${escapeHTML(user.username)}${user.is_platform_admin ? ' <span class="tag">Admin</span>' : ""}</span>
    <button class="btn-quiet" data-logout>Keluar</button></div>
  </header>` : "";
  const pageClass = user ? "page" : "page page-narrow";
  return `${nav}<main id="main" class="${pageClass}">${content}</main>`;
}
export const loading = `<p role="status" class="muted">Memuat data…</p>`;
