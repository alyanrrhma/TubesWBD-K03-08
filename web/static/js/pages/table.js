import { request } from "../api/client.js";
import { escapeHTML } from "../router.js";
import { loading } from "../components/layout.js";

export async function tablePage(root, shell, user, { id, schema, table, tab }) {
  const base = `/databases/${id}/tables/${encodeURIComponent(schema)}/${encodeURIComponent(table)}`;
  const query = new URLSearchParams(location.search);
  const activeTab = tab || (query.get("tab") === "data" ? "data" : "structure");
  const page = Math.max(1, Number(query.get("page")) || 1);
  root.innerHTML = shell(user, `${schema}.${table}`, loading);

  const requests = [
    request(`/api/databases/${id}`),
    request(`/api/databases/${id}/tables/${encodeURIComponent(schema)}/${encodeURIComponent(table)}`),
  ];
  if (activeTab === "data") {
    requests.push(request(`/api/databases/${id}/tables/${encodeURIComponent(schema)}/${encodeURIComponent(table)}/data?page=${page}`));
  }
  const [databaseResult, detail, dataResult] = await Promise.all(requests);
  const db = databaseResult.database;
  const columns = detail.Columns ?? detail.columns ?? [];
  const constraints = detail.Constraints ?? detail.constraints ?? [];
  const indexes = detail.Indexes ?? detail.indexes ?? [];

  let content = `<nav class="crumbs" aria-label="Lokasi">
    <a href="/dashboard" data-link>Database</a> /
    <a href="/databases/${id}" data-link>${escapeHTML(db.name)}</a> /
    <span class="mono">${escapeHTML(schema)}.${escapeHTML(table)}</span>
  </nav>
  <header class="pagehead"><h1 class="mono">${escapeHTML(schema)}.${escapeHTML(table)}</h1></header>
  <nav class="tabs" aria-label="Bagian tabel">
    <a href="${base}" data-link${activeTab === "structure" ? ' aria-current="page"' : ""}>Struktur</a>
    <a href="${base}?tab=data&page=1" data-link${activeTab === "data" ? ' aria-current="page"' : ""}>Data</a>
  </nav>`;

  if (activeTab === "structure") {
    content += `<h2>Kolom</h2>${columns.length ? `<div class="scroll"><table>
      <thead><tr><th class="num">#</th><th>Nama</th><th>Tipe</th><th>Boleh NULL</th></tr></thead>
      <tbody>${columns.map(c => `<tr>
        <td class="num muted">${escapeHTML(c.Position ?? c.position)}</td>
        <td class="mono">${escapeHTML(c.Name ?? c.name)}</td>
        <td class="mono">${escapeHTML(c.DataType ?? c.data_type)}</td>
        <td>${(c.IsNullable ?? c.is_nullable) ? "Ya" : "Tidak"}</td>
      </tr>`).join("")}</tbody>
    </table></div>` : `<div class="empty"><p>Tidak ada kolom untuk ditampilkan.</p></div>`}
    <h2>Constraint</h2>${constraints.length ? `<div class="scroll"><table>
      <thead><tr><th>Nama</th><th>Jenis</th><th>Definisi</th></tr></thead>
      <tbody>${constraints.map(c => `<tr><td class="mono">${escapeHTML(c.Name ?? c.name)}</td><td>${escapeHTML(c.Type ?? c.type)}</td><td class="mono">${escapeHTML(c.Definition ?? c.definition)}</td></tr>`).join("")}</tbody>
    </table></div>` : `<p class="muted">Tidak ada constraint.</p>`}
    <h2>Index</h2>${indexes.length ? `<div class="scroll"><table>
      <thead><tr><th>Nama</th><th>Jenis</th><th>Definisi</th></tr></thead>
      <tbody>${indexes.map(i => `<tr>
        <td class="mono">${escapeHTML(i.Name ?? i.name)}</td>
        <td>${(i.IsPrimary ?? i.is_primary) ? "Primary" : (i.IsUnique ?? i.is_unique) ? "Unik" : "Biasa"}</td>
        <td class="mono">${escapeHTML(i.Definition ?? i.definition)}</td>
      </tr>`).join("")}</tbody>
    </table></div>` : `<p class="muted">Tidak ada index.</p>`}`;
  } else {
    const data = dataResult.data;
    const rows = data.Rows ?? data.rows ?? [];
    const dataColumns = data.Columns ?? data.columns ?? [];
    const currentPage = data.Page ?? data.page ?? page;
    const total = data.Total ?? data.total ?? 0;
    const pageSize = data.PageSize ?? data.page_size ?? 50;
    const pages = dataResult.pages ?? Math.max(1, Math.ceil(total / pageSize));
    const from = total ? (currentPage - 1) * pageSize + 1 : 0;
    const to = Math.min(currentPage * pageSize, total);
    content += rows.length ? `<p class="muted">Baris ${from}–${to} dari ${total}</p>
      <div class="scroll datagrid"><table>
        <thead><tr>${dataColumns.map(c => `<th class="mono">${escapeHTML(c)}</th>`).join("")}</tr></thead>
        <tbody>${rows.map(row => `<tr>${row.map(cell => `<td class="mono">${cell.Null ?? cell.null ? '<span class="null">NULL</span>' : escapeHTML(cell.Text ?? cell.text ?? "")}</td>`).join("")}</tr>`).join("")}</tbody>
      </table></div>
      ${pages > 1 ? `<nav class="pager" aria-label="Halaman data">
        ${currentPage > 1 ? `<a data-link href="${base}?tab=data&page=${currentPage - 1}" rel="prev">Sebelumnya</a>` : `<span class="muted">Sebelumnya</span>`}
        <span>Halaman ${currentPage} dari ${pages}</span>
        ${currentPage < pages ? `<a data-link href="${base}?tab=data&page=${currentPage + 1}" rel="next">Berikutnya</a>` : `<span class="muted">Berikutnya</span>`}
      </nav>` : ""}` : `<div class="empty"><p>Tabel ini belum berisi data.</p></div>`;
  }

  root.innerHTML = shell(user, `${schema}.${table}`, content);
}
