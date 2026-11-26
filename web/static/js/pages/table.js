import { request } from "../api/client.js";
import { escapeHTML } from "../router.js";
import { loading } from "../components/layout.js";
export async function tablePage(root,shell,user,{id,schema,table}) {
  root.innerHTML=shell(user,`${schema}.${table}`,loading);
  const [detail,data] = await Promise.all([
    request(`/api/databases/${id}/tables/${encodeURIComponent(schema)}/${encodeURIComponent(table)}`),
    request(`/api/databases/${id}/tables/${encodeURIComponent(schema)}/${encodeURIComponent(table)}/data?page=1`)
  ]);
  const columns=detail.Columns||detail.columns||[];
  const dataObj=data.data;
  root.innerHTML=shell(user,`${schema}.${table}`,`
    <div class="tabs"><a href="#structure" data-tab="structure" aria-current="page">Struktur</a><a href="#data" data-tab="data">Data</a></div>
    <section id="table-panel"><h2>Kolom</h2><div class="table-wrap"><table><thead><tr><th>Nama</th><th>Tipe</th><th>Nullable</th></tr></thead><tbody>${columns.map(c=>`<tr><td>${escapeHTML(c.Name??c.name)}</td><td>${escapeHTML(c.data_type??c.DataType??c.Type??c.type)}</td><td>${escapeHTML(c.is_nullable??c.IsNullable??c.Nullable??c.nullable)}</td></tr>`).join("")}</tbody></table></div>
    <h2>Data tabel (halaman ${dataObj.Page??dataObj.page})</h2>${renderRows(dataObj)}</section>`);
  root.querySelectorAll("[data-tab]").forEach(a=>a.addEventListener("click",e=>{
    e.preventDefault(); root.querySelectorAll("[data-tab]").forEach(x=>x.removeAttribute("aria-current"));a.setAttribute("aria-current","page");
    const panel=root.querySelector("#table-panel");
    if(a.dataset.tab==="structure") panel.innerHTML=`<h2>Kolom</h2><pre>${escapeHTML(JSON.stringify(columns,null,2))}</pre>`;
    else panel.innerHTML=`<h2>Data tabel</h2>${renderRows(dataObj)}`;
  }));
}
function renderRows(d){
 const cols=d.Columns||d.columns||[], rows=d.Rows||d.rows||[];
 return `<div class="table-wrap"><table><thead><tr>${cols.map(c=>`<th>${escapeHTML(c)}</th>`).join("")}</tr></thead><tbody>${rows.map(row=>`<tr>${row.map(cell=>`<td>${cell.Null||cell.null?'<span class="muted">NULL</span>':escapeHTML(cell.Text??cell.text)}</td>`).join("")}</tr>`).join("")}</tbody></table></div>`;
}
