import { request } from "../api/client.js";
import { escapeHTML } from "../router.js";
import { loading } from "../components/layout.js";
export async function databasePage(root,shell,user,{id}) {
  root.innerHTML=shell(user,"Database",loading);
  const result=await request(`/api/databases/${id}`);
  const db=result.database;
  const schema=result.schemas[0] || "";
  const tables=schema ? await request(`/api/databases/${id}/tables?schema=${encodeURIComponent(schema)}`) : [];
  root.innerHTML=shell(user,db.name,`
    <p class="lede">${escapeHTML(db.host)}:${db.port} / ${escapeHTML(db.database)} · ${escapeHTML(db.level)}</p>
    <div class="toolbar"><label>Schema<select id="schema-select">${result.schemas.map(s=>`<option value="${escapeHTML(s)}" ${s===schema?"selected":""}>${escapeHTML(s)}</option>`).join("")}</select></label></div>
    <section><h2>Tables</h2>${tables.length?`<div class="table-wrap"><table><thead><tr><th>Nama tabel</th><th>Aksi</th></tr></thead><tbody>${tables.map(t=>`<tr><td>${escapeHTML(t.Name ?? t.name)}</td><td><a data-link href="/databases/${id}/tables/${encodeURIComponent(schema)}/${encodeURIComponent(t.Name ?? t.name)}">Lihat struktur</a></td></tr>`).join("")}</tbody></table></div>`:'<p class="empty-state">Tidak ada tabel pada schema ini.</p>'}</section>
    <p><a data-link href="/databases/${id}/migrations">Lihat migration history →</a></p>`);
  root.querySelector("#schema-select")?.addEventListener("change", async e=> {
    const next=await request(`/api/databases/${id}/tables?schema=${encodeURIComponent(e.target.value)}`);
    const body=root.querySelector("tbody");
    if(body) body.innerHTML=next.map(t=>`<tr><td>${escapeHTML(t.Name??t.name)}</td><td><a data-link href="/databases/${id}/tables/${encodeURIComponent(e.target.value)}/${encodeURIComponent(t.Name??t.name)}">Lihat struktur</a></td></tr>`).join("");
  });
}
