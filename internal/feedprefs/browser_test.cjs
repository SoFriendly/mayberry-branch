// Export fixtures with MAYBERRY_SETTINGS_FIXTURE=/tmp/mayberry-settings.html go test ./internal/feedprefs.
// Run with Playwright on NODE_PATH; set CHROME_PATH when using an installed Chrome.
const fs = require('node:fs');
const http = require('node:http');
const assert = require('node:assert/strict');
const { chromium } = require('playwright');
const card = '123456789'; // Disposable reader-only fixture, never a real card.
const authorization = 'Basic ' + Buffer.from(card + ':' + card).toString('base64');
const fixture = process.env.MAYBERRY_SETTINGS_FIXTURE || '/tmp/mayberry-settings.html';
const defaults = {include_branches:[],exclude_branches:[],languages:[],include_unknown_size:true,include_unknown_language:true};
let preferences, sharing, guestCount;
const server = http.createServer(async (req, res) => {
  if (req.headers.authorization !== authorization) {
    res.writeHead(401, {'WWW-Authenticate':'Basic realm="Mayberry"'}).end(); return;
  }
  if (req.url === '/settings') {
    res.writeHead(200, {'Content-Type':'text/html'}).end(fs.readFileSync(fixture+'.local')); return;
  }
  let body = ''; for await (const chunk of req) body += chunk;
  res.setHeader('Content-Type','application/json');
  if (req.url === '/api/feed-preferences') {
    if (req.method === 'PUT') preferences = JSON.parse(body);
    res.end(JSON.stringify({preferences,branches:[{id:'11111111-1111-4111-8111-111111111111',name:'Shared library',subdomain:'shared',online:true}]}));
  } else if (req.url === '/api/sharing') {
    if (req.method === 'POST') sharing = JSON.parse(body).shared_users;
    res.end(JSON.stringify({shared_users:sharing}));
  } else if (req.url === '/api/guest-card') {
    guestCount++; sharing.push('222222222');
    res.end(JSON.stringify({shared_users:sharing,user_id:'222222222'}));
  } else res.writeHead(404).end();
});
(async () => {
  let browser;
  try {
    await new Promise(resolve => server.listen(0,'127.0.0.1',resolve));
    const host = '127.0.0.1:'+server.address().port;
    browser = await chromium.launch({headless:true,...(process.env.CHROME_PATH ? {executablePath:process.env.CHROME_PATH} : {})});
    for (const embedded of [true,false]) {
      preferences = {...defaults}; sharing = ['987654321']; guestCount = 0;
      const context = await browser.newContext(embedded ? {} : {httpCredentials:{username:card,password:card}});
      const page = await context.newPage();
      await page.goto('http://'+(embedded ? card+':'+card+'@' : '')+host+'/settings');
      await page.waitForFunction(() => !document.getElementById('fields').disabled && !document.getElementById('sharing-fields').disabled);
      assert.equal(await page.locator('#branches input').count(),1);
      // Confirm this fixture reproduces the original relative-fetch failure.
      if (embedded) assert.equal(await page.evaluate(async () => {try {await fetch('/api/feed-preferences');return false;} catch (e) {return e instanceof TypeError;}}),true);
      await page.locator('#mirrors').check();
      await page.locator('#save').click();
      await page.waitForFunction(() => document.getElementById('status').textContent.startsWith('Saved'));
      assert.equal(preferences.exclude_mirrored,true);
      await page.locator('#shared-users').fill('987654321, 333333333');
      await page.locator('#sharing-form button[type=submit]').click();
      await page.waitForFunction(() => document.getElementById('sharing-status').textContent.startsWith('Sharing saved'));
      assert.deepEqual(sharing,['987654321','333333333']);
      await page.locator('#guest-card').click();
      await page.locator('#new-card').waitFor({state:'visible'});
      assert.equal(guestCount,1);
      assert.equal(await page.evaluate(() => {try {settingsFetch('https://untrusted.example/api/sharing');return false;} catch {return true;}}),true);
      await context.close();
    }
    console.log('Chrome: credential URLs and normal Basic login load branches, save filters/sharing, create guests, and reject cross-origin requests.');
  } finally {
    if (browser) await browser.close();
    await new Promise(resolve => server.close(resolve));
  }
})().catch(err => {console.error(err);process.exitCode=1;});
