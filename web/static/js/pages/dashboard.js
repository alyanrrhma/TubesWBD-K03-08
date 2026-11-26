import { request } from "../api/client.js";
import { escapeHTML } from "../router.js";
import { loading } from "../components/layout.js";
export async function dashboardPage(root, shell, user) {
  root.innerHTML = shell(user,"Database",loading);
  const items = await request("/api/databases");
  const content = items.length ? `<p class="lede">Database yang dapat Anda akses, sesuai group Anda.</p>
    <div class="scroll"><table>
      <thead><tr><th>Nama</th><th>Alamat</th><th>Akses</th><th></th></tr></thead>
      <tbody>${items.map(db=>`<tr>
        <td><a data-link href="/databases/${db.id}"><strong>${escapeHTML(db.name)}</strong></a>${db.active?"":' <span class="status status-idle">Koneksi tidak aktif</span>'}</td>
        <td class="mono">${escapeHTML(db.host)}:${db.port} / ${escapeHTML(db.database)}</td>
        <td><span class="level ${db.level==="VIEW_EDIT"?"level-edit":"level-view"}">${db.level==="VIEW_EDIT"?"View + Edit":"View Only"}</span></td>
        <td class="actions"><a data-link href="/databases/${db.id}">Jelajahi</a> <a data-link href="/databases/${db.id}/migrations">Migration</a></td>
      </tr>`).join("")}</tbody>
    </table></div>` : `<div class="empty">
      <p><strong>Belum ada database yang bisa Anda akses.</strong></p>
      <p>Akses diberikan lewat group. Minta Platform Admin memasukkan akun Anda ke group yang punya akses ke database.</p>
      ${user.is_platform_admin?'<p><a data-link href="/admin/groups#groups">Atur group dan akses</a></p>':""}
    </div>`;
  root.innerHTML = shell(user,"Database",`<h1>Database</h1>${content}`);
}
