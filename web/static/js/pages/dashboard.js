import { request } from "../api/client.js";
import { escapeHTML } from "../router.js";
import { loading } from "../components/layout.js";
export async function dashboardPage(root, shell, user) {
  root.innerHTML = shell(user,"Database",loading);
  const items = await request("/api/databases");
  const content = items.length ? `<div class="cards">${items.map(db=>`<a class="panel db-card" href="/databases/${db.id}" data-link>
    <h2>${escapeHTML(db.name)}</h2><p class="muted">${escapeHTML(db.host)}:${db.port} / ${escapeHTML(db.database)}</p>
    <span class="tag">${db.level === "VIEW_EDIT" ? "View + Edit" : "View Only"}</span>
    ${db.active ? '<span class="status-ok">Terhubung</span>' : '<span class="status-warn">Tidak aktif</span>'}
  </a>`).join("")}</div>` : `<div class="empty-state"><h2>Belum ada database</h2><p>Belum ada database yang dapat diakses oleh akun ini.</p></div>`;
  root.innerHTML = shell(user,"Database",content);
}
