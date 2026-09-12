package feedprefs

import "encoding/json"

// Page is shared by the hosted reader settings and the local Branch settings.
func Page(endpoint string) string {
	encoded, _ := json.Marshal(endpoint)
	return `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>My catalog — Mayberry</title>
<style>
:root{color-scheme:light dark;font:16px system-ui,sans-serif}body{max-width:680px;margin:3rem auto;padding:0 1.2rem;line-height:1.5}h1{margin-bottom:.3rem}p{color:light-dark(#52525b,#b8b8c2)}fieldset{border:0;padding:0}label{display:block;margin-top:1.3rem;font-weight:600}input[type=text],input[type=number],select{box-sizing:border-box;width:100%;padding:.65rem;border:1px solid #888;border-radius:6px;font:inherit}select{min-height:9rem}small{display:block;color:light-dark(#52525b,#b8b8c2);margin:.3rem 0}.check{font-weight:400;margin-top:.8rem}.check input{margin-right:.5rem}button{font:inherit;padding:.7rem 1rem;border:1px solid #888;border-radius:6px;cursor:pointer;margin:1.5rem .5rem 0 0}button[type=submit]{background:#275a44;color:white;border-color:#275a44}#status{min-height:1.5em}a{color:inherit}
</style></head><body><a href="/settings">Settings</a><h1>My catalog</h1>
<p>Choose what appears in your catalog, search results, and downloads. These preferences apply everywhere you use this library card. They do not change your library or anyone else's catalog.</p>
<form id="filters"><fieldset id="fields" disabled>
<label for="include">Include branches</label><small>Select none to include all available branches. Hold Ctrl or Command to select multiple branches.</small><select id="include" multiple aria-label="Include branches"></select>
<label for="exclude">Exclude branches</label><small>Excluded branches are hidden even when also included above.</small><select id="exclude" multiple aria-label="Exclude branches"></select>
<label class="check"><input id="mirrors" type="checkbox">Exclude mirrored copies</label><small>A book can still appear if an original copy meets your filters.</small>
<label for="size">Maximum file size (MB)</label><small>Leave blank for no limit. A book can appear if at least one eligible copy fits.</small><input id="size" type="number" min="0" step="any" placeholder="No limit">
<label class="check"><input id="unknown-size" type="checkbox" checked>Include files with unknown size when a limit is set</label>
<label for="languages">Languages</label><small>Comma-separated codes, such as en, fr, de, es, ja. Leave blank for all languages. Regional variants match their main language.</small><input id="languages" type="text" placeholder="All languages" autocomplete="off">
<label class="check"><input id="unknown-language" type="checkbox" checked>Include books with unknown language</label>
<button type="submit" id="save">Save catalog filters</button><button type="button" id="reset">Reset filters</button>
</fieldset></form><p id="status" role="status">Loading catalog settings…</p>
<script>
const endpoint=` + string(encoded) + `;
const el=id=>document.getElementById(id);
const defaults={include_branches:[],exclude_branches:[],exclude_mirrored:false,max_size_bytes:0,languages:[],include_unknown_size:true,include_unknown_language:true};
let branches=[];
function show(p){
 for(const [id,key] of [['include','include_branches'],['exclude','exclude_branches']]){
  const selected=new Set(p[key]||[]),options=[...branches];
  for(const missing of selected)if(!options.some(b=>b.id===missing))options.push({id:missing,name:'Previously selected branch',subdomain:'unavailable'});
  el(id).replaceChildren();for(const b of options){const o=document.createElement('option');o.value=b.id;o.textContent=b.name+' ('+b.subdomain+')'+(b.online?'':' — offline');o.selected=selected.has(b.id);el(id).append(o)}
 }
 el('mirrors').checked=p.exclude_mirrored;el('size').value=p.max_size_bytes?p.max_size_bytes/1000000:'';el('languages').value=(p.languages||[]).join(', ');el('unknown-size').checked=p.include_unknown_size;el('unknown-language').checked=p.include_unknown_language;
}
async function load(){try{const r=await fetch(endpoint,{cache:'no-store'});if(!r.ok)throw Error('Could not load settings. Check your library card and try again.');const d=await r.json();branches=d.branches;show(d.preferences);el('fields').disabled=false;el('status').textContent='';}catch(e){el('status').textContent=e.message}}
el('reset').onclick=()=>{show(defaults);el('status').textContent='Filters reset. Save to apply.'};
el('filters').onsubmit=async e=>{e.preventDefault();const selected=id=>Array.from(el(id).selectedOptions,o=>o.value);const size=el('size').value.trim()===''?0:Math.round(Number(el('size').value)*1000000);if(!Number.isSafeInteger(size)||size<0){el('status').textContent='Enter a valid file size.';return}
 const p={include_branches:selected('include'),exclude_branches:selected('exclude'),exclude_mirrored:el('mirrors').checked,max_size_bytes:size,languages:el('languages').value.split(',').map(x=>x.trim()).filter(Boolean),include_unknown_size:el('unknown-size').checked,include_unknown_language:el('unknown-language').checked};
 el('fields').disabled=true;el('status').textContent='Saving…';try{const r=await fetch(endpoint,{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify(p)});if(!r.ok)throw Error((await r.text()).trim()||'Could not save settings.');const d=await r.json();show(d.preferences);el('status').textContent='Saved. Refresh your reader’s catalog to see the changes.'}catch(e){el('status').textContent=e.message}finally{el('fields').disabled=false}};
load();
</script></body></html>`
}
