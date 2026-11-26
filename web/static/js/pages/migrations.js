import { request } from "../api/client.js";
import { escapeHTML } from "../router.js";
import { loading } from "../components/layout.js";
export async function migrationsPage(root,shell,user,{id}) {
 root.innerHTML=shell(user,"Migration history",loading);
 const result=await request(`/api/databases/${id}/migrations`);
 const items=result.items||[];
 root.innerHTML=shell(user,"Migration history",`
  <p class="lede">Riwayat eksekusi linear database ini.</p>
  ${items.length?`<div class="table-wrap"><table><thead><tr><th>Versi</th><th>Status</th><th>Pelaksana</th><th>Waktu</th><th></th></tr></thead><tbody>${items.map(m=>`<tr><td>${m.Version??m.version}</td><td><span class="tag">${escapeHTML(m.Status??m.status)}</span></td><td>${escapeHTML(m.ExecutedBy??m.executed_by)}</td><td>${escapeHTML(m.ExecutedAt??m.executed_at)}</td><td><a data-link href="/databases/${id}/migrations/${m.Version??m.version}">Detail</a></td></tr>`).join("")}</tbody></table></div>`:'<p class="empty-state">Belum ada migration.</p>'}`);
}
