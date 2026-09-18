/** Real Chromium acceptance test. Isolated profile + localhost fixture, no external sites. */
import {app,BaseWindow,WebContentsView,webContents,ipcMain} from 'electron';
import assert from 'node:assert/strict';
import http from 'node:http';
import net from 'node:net';
import crypto from 'node:crypto';
import {spawn} from 'node:child_process';
import fs from 'node:fs/promises';
import fsSync from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {startBrowserServer} from '../browser-tool.mjs';
import {testViewport} from './test-browser-viewport-live.mjs';
async function main(){
const root=fsSync.mkdtempSync(path.join(os.tmpdir(),'aic-ui-live-'));
const sid='ui-test-'+crypto.randomUUID(),sessionDir=path.join(os.homedir(),'.aic','sessions',sid);
fsSync.mkdirSync(path.join(root,'profile'));app.setPath('userData',path.join(root,'profile'));
app.on('window-all-closed',()=>{});
let exitCode=0;
const fixture=`<!doctype html><meta charset="utf-8"><title>UI fixture</title>
<label>Email<input aria-label="Email" id="email"></label><button id="save">Save</button>
<label><input type="checkbox" id="check">Agree</label><select aria-label="Color"><option value="red">Red</option><option value="blue">Blue</option></select>
<input type="range" min="0" max="10" step="2" aria-label="Volume"><div id="status">Ready</div>
<input type="file" aria-label="Upload"><a href="/file" download="hello.txt">Download</a>
<button id="alert" onclick="window.alertCount=(window.alertCount||0)+1;alert('sync alert');window.alertDone=true">Alert</button>
<button onclick="window.confirmValue=confirm('sync confirm')">Confirm</button>
<button onclick="window.promptValue=prompt('sync prompt','default')">Prompt</button>
<button onclick="alert('first');alert('second');window.nestedDone=true">Nested</button>
<button onclick="window.asyncTimer=setTimeout(()=>{alert('async alert');window.asyncDone=true},500)">Async</button>
<input aria-label="Modal input" oninput="window.inputCount=(window.inputCount||0)+1;alert('input alert');window.inputDone=this.value">
<div id="scroller" aria-label="Scroller" role="region" style="height:100px;width:300px;overflow:auto"><div style="height:1500px;width:1500px">Nested scroll</div></div>
<div style="height:5000px;width:3000px">Page scroll</div>
<script>window.clicks=0;document.getElementById('save').onclick=e=>{window.clicks++;document.getElementById('status').textContent='Saved '+clicks;window.trusted=e.isTrusted};console.log('fixture-console');</script>`;
const viewerSource=process.env.AIC_PLATFORM_UI ? path.join(process.env.AIC_PLATFORM_UI,'os/browser-viewer.js') : new URL('../../../aic/ui/os/browser-viewer.js',import.meta.url);
const vhtmlBundle = new URL('../../../vhtml/dist/vhtml.min.js',import.meta.url);
const platformUI = process.env.AIC_PLATFORM_UI || new URL('../../../aic/ui/',import.meta.url).pathname;
const server=http.createServer((req,res)=>{
 const pathname = new URL(req.url,'http://localhost').pathname;
 const files = {'/vhtml.js':vhtmlBundle,'/os/browser-viewer.js':viewerSource,
  '/os/wincontent.js':path.join(platformUI,'os/wincontent.js'),'/page/local/browser.html':path.join(platformUI,'page/local/browser.html')};
 if(files[pathname]) {res.setHeader('Content-Type',pathname.endsWith('.html')?'text/html':'text/javascript');res.end(fsSync.readFileSync(files[pathname]));return;}
 if(['/env.js','/routes.js','/langs.json'].includes(pathname)) {res.writeHead(404);res.end();return;}
 if(pathname==='/component') {res.setHeader('Content-Type','text/html');res.end(`<!doctype html><style>html,body{margin:0;width:100%;height:100%}</style><body><page-local-browser></page-local-browser><script type="module">import VHTML from '/vhtml.js';window.$vhtml=new VHTML(document.body);await $vhtml.ready;window.componentReady=true;</script>`);return;}
 if(req.url==='/viewer.js') {res.setHeader('Content-Type','text/javascript');res.end(fsSync.readFileSync(viewerSource));return;}
 if(req.url==='/viewer') {res.setHeader('Content-Type','text/html');res.end(`<!doctype html><body style="margin:0;background:#cbd5e1"><div id="viewer" style="position:absolute;left:20px;top:30px;width:640px;height:500px;background:white;overflow:hidden"></div><script type="module">import {createBrowserViewer} from '/viewer.js';const bridge=window.aicDesktop.nativeWin;window.viewer=createBrowserViewer({el:document.getElementById('viewer'),bridge});bridge.onChanged(s=>viewer.setState(s));viewer.setState(await bridge.getState());</script>`);return;}
 if(req.url==='/viewport') {res.setHeader('Content-Type','text/html');res.end(`<!doctype html><style>body{margin:0;height:1800px}input{position:absolute;left:80px;top:30px;width:200px;height:40px}</style><input id="edit"><script>addEventListener('mousedown',()=>window.presses=(window.presses||0)+1)</script>`);return;}
 if(req.url==='/file'){res.writeHead(200,{'Content-Type':'text/plain','Content-Disposition':'attachment; filename="hello.txt"'});res.end('download fixture');}else{res.writeHead(200,{'Content-Type':'text/html'});res.end(fixture)}});
await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
let win,provider;
try{
 await app.whenReady();win=new BaseWindow({show:false,width:900,height:700});const platformView=new WebContentsView({webPreferences:{preload:path.resolve('remote-preload.js')}});win.contentView.addChildView(platformView);
 ipcMain.on('allowed:hosts',e=>{e.returnValue=['127.0.0.1:'+server.address().port]});
 provider=await startBrowserServer({host:{win,platformView,
  onFrame:frame=>platformView.webContents.send('native:frame',frame),
  onChanged:state=>platformView.webContents.send('native:changed',state),
 }});const {adapter}=provider;
 ipcMain.handle('native:state',()=>adapter.tabControl.getState());
 ipcMain.handle('native:layout',(_e,state)=>adapter.tabControl.applyLayout(state));
 ipcMain.on('native:frame-ack',(_e,seq)=>adapter.tabControl.acknowledgeFrame(seq));
 ipcMain.on('native:input-focus',(_e,focused)=>platformView.webContents.setIgnoreMenuShortcuts(focused));
 for(const kind of ['mouse','wheel','key','text','edit','reset']) ipcMain.on('native:'+kind,(_e,payload)=>Promise.resolve(adapter.tabControl.input[kind](payload)).catch(console.error));
 await testViewport({win,platformView,adapter,base:'http://127.0.0.1:'+server.address().port,root:process.env.AIC_BROWSER_TEST_ARTIFACTS || root,hasViewerSource:fsSync.existsSync(viewerSource),hasVhtml:fsSync.existsSync(vhtmlBundle)});
 await fs.mkdir(sessionDir,{recursive:true});
 // Exercise the authenticated provider, including its scheduling, over separate sockets.
 async function run(argv,extra={}){const response=await new Promise((resolve,reject)=>{
  const socket=net.createConnection({host:'127.0.0.1',port:provider.port});let data='';
  socket.setTimeout(6000,()=>socket.destroy(Error('provider response timed out: '+argv.join(' '))));
  socket.on('error',reject);socket.on('connect',()=>socket.write(JSON.stringify({token:provider.token,session_id:sid,session_dir:sessionDir,msg_id:crypto.randomUUID(),granted_level:extra.grantedLevel??3,authorized_file:extra.authorizedFile,download_dir:extra.downloadDir,argv:[...argv,'--format','json']})+'\n'));
  socket.on('data',chunk=>{data+=chunk;if(data.includes('\n')){socket.end();try{resolve(JSON.parse(data.trim()))}catch(e){reject(e)}}});
 });const r=JSON.parse(response.content);return {...r,images:response.attrs};}
 async function ok(argv,extra){const r=await run(argv,extra);assert.equal(r.state,'completed',JSON.stringify(r));return r;}
 const opened=await ok(['open',`http://127.0.0.1:${server.address().port}/`]);assert.ok(opened.target.id);
 await ok(['wait','--load','complete']);
 let snap=await ok(['snapshot']);assert.ok(snap.observation.elements.some(e=>e.name==='Email'));
 const input=snap.observation.elements.find(e=>e.name==='Email').ref;
 await ok(['fill',input,'--text','中文 protocol@example.test']);
 assert.equal((await ok(['get','value','--label','Email'])).data.value,'中文 protocol@example.test');
 assert.equal((await run(['click',input])).error.code,'stale_ref');
 await ok(['press','--label','Email','--key','End']);await ok(['type','--text','!']);
 assert.equal((await ok(['get','value','--label','Email'])).data.value,'中文 protocol@example.test!');
 await ok(['click','--role','button','--name','Save']);
 const clicked=await ok(['eval','--code','({clicks:window.clicks,trusted:window.trusted,handler:typeof document.getElementById("save").onclick})']);assert.equal(clicked.data.trusted,true);
 await ok(['set','--label','Agree','--value','true']);assert.equal((await ok(['get','checked','--label','Agree'])).data.checked,true);
 await ok(['set','--label','Color','--value','"blue"']);assert.equal((await ok(['get','value','--label','Color'])).data.value,'blue');
 const before=(await ok(['get','value','--label','Volume'])).data.value;const invalid=await run(['set','--label','Volume','--value','3']);assert.equal(invalid.state,'error');assert.equal((await ok(['get','value','--label','Volume'])).data.value,before);
 await ok(['set','--label','Volume','--value','4']);
 const shot=await ok(['screenshot']);assert.ok(shot.images.image_data.startsWith('data:image/'));assert.ok(shot.observation.image.width>0);
 // External document mutation invalidates snapshot refs through actual CDP DOM events.
 snap=await ok(['snapshot']);const old=snap.observation.elements.find(e=>e.name==='Save').ref;
 const native=(await adapter.tabs.list())[0].id;await webContents.fromId(native).executeJavaScript(`document.querySelector('button').setAttribute('aria-label','Changed')`);
 const stale=await run(['click',old]);assert.equal(stale.error?.code,'stale_ref',JSON.stringify(stale));
 const upload=path.join(root,'upload.txt');await fs.writeFile(upload,'upload fixture');await ok(['upload','--label','Upload','--file',upload],{authorizedFile:upload});
 assert.equal((await ok(['eval','--code','document.querySelector("input[type=file]").files[0].name'])).data,'upload.txt');
 const downloaded=await ok(['download','--role','link','--name','Download'],{downloadDir:path.join(root,'downloads')});assert.equal(await fs.readFile(downloaded.artifacts[0].path,'utf8'),'download fixture');
 assert.ok((await ok(['network','list'])).data.length>0);assert.ok((await ok(['console','list'])).data.some(x=>JSON.stringify(x).includes('fixture-console')));
 const other=await ok(['open','about:blank']);await ok(['target','use',opened.target.id]);
 const value=async code=>(await ok(['eval','--code',code])).data;
 for(const [name,control,field,expected] of [['Alert',['dismiss'],'alertDone',true],['Confirm',['dismiss'],'confirmValue',false],['Confirm',['accept'],'confirmValue',true]]){
  const pending=await run(['click','--role','button','--name',name]);assert.equal(pending.error?.code,'dialog_open',JSON.stringify(pending));assert.equal(pending.action.performed,'unknown');
  assert.ok((await ok(['target','list'])).data.length);await ok(['target','current']);
  assert.equal((await run(['snapshot'])).error?.code,'browser_busy');
  assert.equal((await run(['dialog',...control],{grantedLevel:1})).state,'rejected');
  assert.equal((await run(['dialog',...control,'--target',other.target.id])).error?.code,'not_found');
  const resumed=await ok(['dialog',...control]);assert.equal(resumed.data?.resumed.state,'completed',JSON.stringify(resumed));
  assert.equal(await value('window.'+field),expected);
 }
 assert.equal(await value('window.alertCount'),1);
 // Electron overrides window.prompt to throw. This is a backend limitation,
 // not a successful prompt dialog test; CDP prompt forwarding is tested separately.
 await ok(['click','--role','button','--name','Prompt']);
 assert.ok((await ok(['console','list'])).data.some(x=>x.type==='exception'&&x.text.includes('prompt() is not supported')));
 let pending=await run(['fill','--label','Modal input','--text','中文同步输入']);assert.equal(pending.error?.code,'dialog_open');
 await ok(['dialog','accept']);assert.equal(await value('window.inputDone'),'中文同步输入');assert.equal(await value('window.inputCount'),1);
 pending=await run(['click','--role','button','--name','Nested']);assert.equal(pending.error?.code,'dialog_open');
 const nested=await run(['dialog','accept']);assert.equal(nested.error?.code,'dialog_open');assert.equal(nested.data.dialog.message,'second');
 await ok(['dialog','dismiss']);assert.equal(await value('window.nestedDone'),true);
 await ok(['click','--role','button','--name','Async']);
 await new Promise((resolve,reject)=>{if(adapter.dialog(native))return resolve();const off=adapter.onDialog(({dialog})=>{if(dialog){clearTimeout(timer);off();resolve()}});const timer=setTimeout(()=>{off();reject(Error('async dialog missing'))},2000)});
 await ok(['dialog','accept']);assert.equal(await value('window.asyncDone'),true);
 assert.equal((await run(['dialog','dismiss'])).error?.code,'not_found');
 const query=await ok(['snapshot','--query','enabled']);assert.equal(query.observation.elements.length,0);
 assert.ok((await ok(['snapshot','--query','modal INPUT'])).observation.elements.some(e=>e.name==='Modal input'));
 // No wait command between scrolling and observing its result.
 for(let i=0;i<3;i++){
  const before=await value('scrollY'),scrolled=await ok(['scroll','--dy','300']);
  assert.ok(scrolled.observation.viewport.scrollY>before,JSON.stringify(scrolled.observation.viewport));
  assert.equal(scrolled.observation.viewport.scrollY,await value('scrollY'));
 }
 await ok(['scroll','--label','Scroller','--dy','200','--dx','100']);
 const nestedScroll=await value('({x:document.getElementById("scroller").scrollLeft,y:document.getElementById("scroller").scrollTop})');assert.ok(nestedScroll.x>0&&nestedScroll.y>0,JSON.stringify(nestedScroll));
 await new Promise((resolve,reject)=>{
  const child=spawn('go',['test','./libs/host','-run','^TestBrowserScriptLive$','-count=1','-v'],{cwd:path.resolve('..'),env:{...process.env,AIC_UI_TEST_BROWSER_ADDR:'127.0.0.1:'+provider.port,AIC_UI_TEST_BROWSER_TOKEN:provider.token,AIC_UI_TEST_BROWSER_TARGET:opened.target.id,AIC_UI_TEST_BROWSER_SID:sid},stdio:['ignore','pipe','pipe']});
  const timer=setTimeout(()=>{child.kill('SIGKILL');reject(Error('host script acceptance timed out'))},45000);
  child.stdout.on('data',chunk=>process.stdout.write(chunk));child.stderr.on('data',chunk=>process.stderr.write(chunk));child.on('error',reject);child.on('exit',code=>{clearTimeout(timer);code===0?resolve():reject(Error('host script acceptance failed: '+code))});
 });
 assert.equal(await value('document.getElementById("email").value'),'批量脚本@example.test');assert.equal(await value('window.confirmValue'),true);
 await ok(['close','--target',other.target.id]);
 await ok(['close']);assert.deepEqual((await ok(['target','list'])).data,[]);
 console.log('PASS: real Chromium via TCP: input, state, stale refs, screenshots, files, logs, synchronous/nested/asynchronous dialogs, query and scroll');
}catch(e){console.error(e);exitCode=1;}
finally{server.close();provider?.server.close();if(win&&!win.isDestroyed()){for(const child of win.contentView.children){child.webContents?.close()}win.destroy()}await fs.rm(root,{recursive:true,force:true}).catch(()=>{});await fs.rm(sessionDir,{recursive:true,force:true}).catch(()=>{});app.exit(exitCode)}

}
main().catch(e=>{console.error(e);app.exit(1)});
