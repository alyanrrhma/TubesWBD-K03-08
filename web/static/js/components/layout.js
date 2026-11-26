import { escapeHTML } from "../router.js";
export function shell(user, title, content) {
  const nav = user ? `<header class="topbar">
    <a class="brand" href="/dashboard" data-link>Database Platform</a>
    <nav aria-label="Navigasi utama">
      <a href="/dashboard" data-link>Database</a>
      <a href="/migrations" data-link>Migration</a>
      ${user.is_platform_admin ? `<a href="/admin/users" data-link>Administrasi</a>` : ""}
    </nav>
    <div class="userbox"><span>${escapeHTML(user.username)}${user.is_platform_admin ? ' <span class="tag">Admin</span>' : ""}</span>
    <button class="btn-quiet" data-logout>Keluar</button></div>
  </header>` : "";
  return `${nav}<main id="main" class="page ${user ? "" : "page-narrow"}"><div class="pagehead"><h1>${escapeHTML(title)}</h1></div>${content}</main>`;
}
export const loading = `<p role="status" class="muted">Memuat data…</p>`;
