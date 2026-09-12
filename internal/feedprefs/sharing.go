package feedprefs

import (
	"html"
	"strings"
)

// LocalPage is retained for Branch callers.
func LocalPage(endpoint, userID string, sharedUsers []string) string {
	return SharingPage(endpoint, userID, sharedUsers)
}

// SharingPage is shared by local and hosted settings.
func SharingPage(endpoint, userID string, sharedUsers []string) string {
	sharing := `<section aria-labelledby="sharing-heading"><h2 id="sharing-heading">Friends and sharing</h2>
<p>Sharing is mutual: add a friend's library card to share your library and see theirs.</p>
<label>Your library card</label><div><code id="my-card">` + html.EscapeString(userID) + `</code> <button type="button" id="copy-card">Copy card</button></div>
<small>Share this card with friends you trust.</small>
<form id="sharing-form"><fieldset id="sharing-fields">
<label for="shared-users">People with access</label><small>Enter library card numbers separated by commas. Remove a card to stop sharing from your library. If they also added your card, that mutual connection remains.</small>
<input id="shared-users" type="text" autocomplete="off" value="` + html.EscapeString(strings.Join(sharedUsers, ", ")) + `">
<button type="submit">Save sharing</button><button type="button" id="guest-card">Create guest card</button>
</fieldset></form><p id="sharing-status" role="status"></p><p id="new-card" hidden></p>
<small>A guest card is added to your sharing list automatically. Send it to a friend who doesn't have a card yet. Save and refresh your catalog to see sharing changes.</small>
</section>
<script>
async function sharingRequest(path,body){
 const r=await fetch(path,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});
 const data=await r.json().catch(()=>({}));if(!r.ok)throw Error(data.error||'Could not save sharing. Try again.');return data;
}
el('copy-card').onclick=async()=>{try{await navigator.clipboard.writeText(el('my-card').textContent);el('sharing-status').textContent='Card copied.'}catch(e){el('sharing-status').textContent='Select your card number above to copy it.'}};
el('sharing-fields').disabled=true;
fetch('/api/sharing',{cache:'no-store'}).then(async r=>{if(!r.ok)throw Error('Could not load sharing. Reload to try again.');const d=await r.json();el('shared-users').value=(d.shared_users||[]).join(', ');el('sharing-fields').disabled=false;}).catch(e=>{el('sharing-status').textContent=e.message});
el('sharing-form').onsubmit=async e=>{
 e.preventDefault();el('sharing-fields').disabled=true;el('sharing-status').textContent='Saving sharing…';
 try{const d=await sharingRequest('/api/sharing',{shared_users:el('shared-users').value.toLowerCase().split(',').map(x=>x.trim()).filter(Boolean)});el('shared-users').value=(d.shared_users||[]).join(', ');el('sharing-status').textContent='Sharing saved. Refresh your catalog to see the changes.';}
 catch(e){el('sharing-status').textContent=e.message}finally{el('sharing-fields').disabled=false}
};
el('guest-card').onclick=async()=>{
 el('sharing-fields').disabled=true;el('sharing-status').textContent='Creating guest card…';
 try{const d=await sharingRequest('/api/guest-card',{});el('shared-users').value=(d.shared_users||[]).join(', ');el('new-card').textContent='New guest card: '+d.user_id;el('new-card').hidden=false;el('sharing-status').textContent='Guest card created and added to sharing.';}
 catch(e){el('sharing-status').textContent=e.message}finally{el('sharing-fields').disabled=false}
};
</script>`
	return strings.Replace(Page(endpoint), "</body>", sharing+"</body>", 1)
}
