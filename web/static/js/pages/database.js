import { request } from "../api/client.js";
import { escapeHTML } from "../router.js";
import { loading } from "../components/layout.js";
export async function databasePage(root,shell,user,{id}) {
  root.innerHTML=shell(user,"Database",loading);
  const result=await request(`/api/databases/${id}`);
  const db=result.database;
  const schemas=result.schemas||[];
  const requestedSchema=new URLSearchParams(location.search).get("schema");
  const schema=schemas.includes(requestedSchema)?requestedSchema:(schemas[0]||"");
  const tables=schema ? await request(`/api/databases/${id}/tables?schema=${encodeURIComponent(schema)}`) : [];
  root.innerHTML=shell(user,db.name,`
    <nav class="crumbs" aria-label="Lokasi"><a href="/dashboard" data-link>Database</a> / <span>${escapeHTML(db.name)}</span></nav>
    <header class="pagehead"><h1>${escapeHTML(db.name)}</h1><span class="level ${db.level==="VIEW_EDIT"?"level-edit":"level-view"}">${db.level==="VIEW_EDIT"?"View + Edit":"View Only"}</span></header>
    <p class="mono muted">${escapeHTML(db.host)}:${db.port} / ${escapeHTML(db.database)}</p>
    <nav class="tabs" aria-label="Bagian database">
      <a href="/databases/${id}" data-link aria-current="page">Schema dan tabel</a>
      <a href="/databases/${id}/migrations" data-link>Migration</a>
    </nav>
    ${schemas.length?`<div class="split">
      <nav class="schemalist" aria-label="Schema"><h2>Schema</h2><ul>${schemas.map(s=>`<li><a class="mono" data-link href="/databases/${id}?schema=${encodeURIComponent(s)}"${s===schema?' aria-current="true"':""}>${escapeHTML(s)}</a></li>`).join("")}</ul></nav>
      <section><h2>Tabel di <span class="mono">${escapeHTML(schema)}</span></h2>
       ${tables.length?`<div class="scroll"><table><thead><tr><th>Nama</th></tr></thead><tbody>${tables.map(t=>`<tr><td><a class="mono" data-link href="/databases/${id}/tables/${encodeURIComponent(schema)}/${encodeURIComponent(t.Name??t.name)}">${escapeHTML(t.Name??t.name)}</a></td></tr>`).join("")}</tbody></table></div>`:`<div class="empty"><p>Schema ini belum memiliki tabel.</p></div>`}
      </section>
    </div>`:`<div class="empty"><p>Database ini belum memiliki schema yang dapat ditampilkan.</p></div>`}
  `);
}
