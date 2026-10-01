import test from 'node:test';
import assert from 'node:assert/strict';
import { BrowserDirectory, BrowserView } from './browser.js';
import { containPoint } from './browser-viewer.js';
const tick = () => new Promise(r => setImmediate(r));
const host = id => ({id,status:'enabled',caps:{transports:{rtc:{enabled:true,protocol:"hosts_rtc/2"}},tool_protocols:['hosts_rtc/2']}});
test('discovery reads tool pages without creating browser state', async () => {
  const calls = [], closed = [];
  const directory = new BrowserDirectory({directory:{list:async () => ['a','b'].map(host)},openTools:async id => ({
    execCall:async script => {calls.push(script); return {content:JSON.stringify([{page_id:'same',width:800,height:600}]),attrs:{exit_code:'0'}};},
    close:async () => closed.push(id),
  })});
  const rows = await directory.refresh();
  assert.deepEqual(rows.map(r => r.windows[0].key),['a/same','b/same']);
  assert.ok(calls.every(s => s === 'browser page.list --json'));
  await directory.close(); assert.deepEqual(closed.sort(),['a','b']);
});
test('late discovery is closed after disposal', async () => {
  let finish, closed = 0, changes = 0;
  const directory = new BrowserDirectory({directory:{list:async () => [host('a')]},openTools:() => new Promise(r => {finish=r;})},() => changes++);
  const pending=directory.refresh(); await tick(); await directory.close();
  finish({close:async () => closed++}); await pending;
  assert.equal(closed,1); assert.equal(changes,0);
});
test('viewport scaling ignores margins and clamps held input', () => {
  const r={x:20,y:30,width:640,height:500},v={width:1280,height:720};
  assert.equal(containPoint(r,v,40,40),null);
  assert.deepEqual(containPoint(r,v,80,125),{x:120,y:50,scale:0.5});
  assert.deepEqual(containPoint(r,v,700,550,true),{x:1279,y:719,scale:0.5});
});
function fixture() {
  const calls=[], inputs=[];
  let finish, end, closed=0;
  const frames={recv:() => new Promise((resolve,reject) => {finish=resolve;end=reject;}),close:async () => {closed++;end?.(new Error('closed'));}};
  const input={send:async raw => inputs.push(JSON.parse(new TextDecoder().decode(raw))),close:async () => {input.closed=true;}};
  const session={execCall:async script => {calls.push(['exec',script]);return {content:'{}',attrs:{exit_code:'0'}};},openStream:async endpoint => {calls.push(['stream',endpoint]);return endpoint==='page.frames'?frames:input;}};
  const view=new BrowserView(session,{page_id:'p',document_id:'doc',viewport:{width:800,height:600}});
  return {view,calls,inputs,input,sendFrame:item=>finish(encodeFrame(item)),closed:()=>closed};
}
test('viewing sends no input; the first real input opens a channel without control calls', async () => {
  const f=fixture(); await f.view.start(); await tick();
  assert.equal(f.inputs.length,0); assert.equal(f.view.controls,undefined);
  f.view.send('text','once'); await f.view.controls.flush();
  assert.deepEqual(f.inputs,[{seq:1,document_id:'doc',events:[{type:'text',text:'once'}]}]);
  assert.deepEqual(f.calls.map(c=>c[1]),['page.frames','page.input']);
  f.view.setVisible(false); await tick(); assert.equal(f.input.closed,true);
  assert.equal(f.view.interactive,false); await f.view.close();
  assert.ok(!f.calls.some(c=>c[1].startsWith('page.control.')));
});
test('late frame stream is closed after viewer disposal', async () => {
  let finish, closed=0;
  const view=new BrowserView({openStream:()=>new Promise(r=>finish=r)},{page_id:'p'});
  const pending=view.start();await view.close();finish({close:async()=>closed++});await pending;
  assert.equal(closed,1);
});
test('frame chunks assemble once and update document identity for input', async () => {
  const f=fixture(), seen=[]; f.view.onFrame=async frame=>seen.push(frame);
  await f.view.start();
  f.sendFrame({metadata:{page_id:'p',frame_seq:1,document_id:'doc2',size:4,width:800,height:600},bytes:btoa('\xff\xd8')});await tick();
  f.sendFrame({bytes:btoa('\xff\xd9'),final:true});await tick();
  assert.equal(seen.length,1);assert.equal(f.view.target.document_id,'doc2');
  assert.deepEqual([...seen[0].data],[255,216,255,217]);await f.view.close();
});
test('late frames cannot rewind the picture or its document identity', async t => {
  const f=fixture(), seen=[]; t.after(()=>f.view.close());
  f.view.onFrame=frame=>seen.push(frame.frame_seq);
  await f.view.start();
  const frame=(frame_seq,document_id)=>({metadata:{page_id:'p',frame_seq,document_id,size:4,width:800,height:600},bytes:btoa('\xff\xd8\xff\xd9'),final:true});
  f.sendFrame(frame(2,'new')); await tick();
  f.sendFrame(frame(1,'old')); await tick();
  f.sendFrame(frame(2,'old')); await tick();
  assert.deepEqual(seen,[2]); assert.equal(f.view.target.document_id,'new');
});
test('an old input failure cannot close a newer input channel', async t => {
  const f=fixture(); t.after(()=>f.view.close()); await f.view.start();
  let reject;
  f.input.send=()=>new Promise((_r,j)=>reject=j);
  f.view.send('text','old'); const pending=f.view.controls.flush(); await tick();
  await f.view.closeInput();
  const sent=[], input={send:async raw=>sent.push(JSON.parse(new TextDecoder().decode(raw))),close:async()=>{input.closed=true;}};
  f.view.connection.openStream=async()=>input;
  f.view.send('text','new'); await f.view.controls.flush();
  reject(new Error('old connection failed')); await pending;
  assert.equal(input.closed,undefined); assert.equal(f.view.interactive,true);
  assert.equal(sent[0].seq,1); assert.equal(sent[0].events[0].text,'new');
});
test('closing while input opens discards queued input and closes the late channel', async () => {
  const f=fixture(); await f.view.start();
  let finish;
  f.view.connection.openStream=()=>new Promise(r=>finish=r);
  f.view.send('text','stale'); const pending=f.view.controls.flush();
  await f.view.close(); finish(f.input); await pending;
  assert.equal(f.input.closed,true); assert.deepEqual(f.inputs,[]);
});
test('the first input survives channel setup and navigation needs no release call', async t => {
  const f=fixture(); t.after(()=>f.view.close()); await f.view.start();
  let finish;
  f.view.connection.openStream=()=>new Promise(r=>finish=r);
  f.view.send('mouse',{type:'mousedown',x:1,y:2,button:0});
  f.view.send('mouse',{type:'mouseup',x:1,y:2,button:0});
  const pending=f.view.controls.flush(); await tick();
  assert.deepEqual(f.inputs,[]);
  finish(f.input); await pending;
  assert.deepEqual(f.inputs[0].events.map(e=>e.type),['pointer.down','pointer.up']);
  await f.view.mutate('reload');
  assert.equal(f.input.closed,true);
  assert.deepEqual(f.calls.map(c=>c[1]),['page.frames','browser page.reload --json -- p']);
});
test('focus loss resets pressed state without closing the input channel', async t => {
  const f=fixture(); t.after(()=>f.view.close()); await f.view.start();
  f.view.send('key',{type:'keyDown',key:'Shift'}); await f.view.controls.flush();
  f.view.send('reset'); await f.view.controls.flush();
  f.view.send('text','again'); await f.view.controls.flush();
  assert.deepEqual(f.inputs.map(b=>b.events[0].type),['key.down','reset','text']);
  assert.equal(f.input.closed,undefined);
  assert.deepEqual(f.calls.map(c=>c[1]),['page.frames','page.input']);
});

function encodeFrame({bytes, ...header}) {
 const meta=new TextEncoder().encode(JSON.stringify(header));
 const data=Uint8Array.from(atob(bytes||''),c=>c.charCodeAt(0));
 const out=new Uint8Array(4+meta.length+data.length);
 new DataView(out.buffer).setUint32(0,meta.length);out.set(meta,4);out.set(data,4+meta.length);
 return out;
}

test('a slow decoder consumes the current and latest frame without replaying the backlog', async t => {
  const f=fixture(), seen=[]; t.after(()=>f.view.close());
  let finish;
  f.view.onFrame=async frame=>{
    seen.push(frame.frame_seq);
    if(frame.frame_seq===1)await new Promise(r=>finish=r);
    return true;
  };
  await f.view.start();
  const frame=n=>({metadata:{page_id:'p',frame_seq:n,document_id:'doc',size:4,width:800,height:600},bytes:btoa('\xff\xd8\xff\xd9'),final:true});
  f.sendFrame(frame(1));await tick();
  for(let n=2;n<=100;n++) {f.sendFrame(frame(n));await tick();}
  assert.deepEqual(seen,[1]);
  finish();await tick();
  assert.deepEqual(seen,[1,100]);assert.equal(f.view.frameSeq,100);
});
import { browserCall } from './browser.js';
test('browserCall parses the full json response directly (RTC: no truncation, no background)', async () => {
  const conn={execCall:async script=>({content:'[{"page_id":"p2"}]',attrs:{exit_code:'0'}})};
  assert.deepEqual(await browserCall(conn,'browser page.list --json'),[{page_id:'p2'}]);
  await assert.rejects(()=>browserCall({execCall:async ()=>({content:'',attrs:{exit_code:'1',stderr:'boom'}})},'browser page.list --json'),/boom/);
});
test('dialog accept is always explicit and navigate uses a positional url', async t => {
  const f=fixture(); t.after(()=>f.view.close()); await f.view.start();
  f.view.target.dialog={id:'d1'};
  await f.view.mutate('dialog',{accept:true});
  await f.view.mutate('dialog',{accept:false});
  await f.view.mutate('navigate',{url:'https://a.b/c d'});
  const scripts=f.calls.filter(c=>c[0]==='exec').map(c=>c[1]);
  assert.ok(scripts.includes('browser page.dialog.resolve --accept --json -- p d1'),scripts.join('\n'));
  assert.ok(scripts.includes('browser page.dialog.resolve --accept=false --json -- p d1'),scripts.join('\n'));
  assert.ok(scripts.includes("browser page.navigate --json -- p 'https://a.b/c d'"),scripts.join('\n'));
});
test('user-controlled positionals sit behind a literal -- boundary', async t => {
  const f=fixture(); t.after(()=>f.view.close()); await f.view.start();
  f.view.target.dialog={id:'--accept=false'};
  // `-` 开头的 dialog_id/url/text 不被 shellQuote 防护（契约）——必须落在
  // `--` 后按位置参数解析，或经 = 形式作旗标值，不能改写旗标语义。
  await f.view.mutate('dialog',{accept:true,text:'--accept=false'});
  await f.view.mutate('navigate',{url:'--json'});
  await f.view.mutate('click',{ref:'-x'});
  const scripts=f.calls.filter(c=>c[0]==='exec').map(c=>c[1]);
  assert.ok(scripts.includes('browser page.dialog.resolve --accept --text=--accept=false --json -- p --accept=false'),scripts.join('\n'));
  assert.ok(scripts.includes('browser page.navigate --json -- p --json'),scripts.join('\n'));
  assert.ok(scripts.includes('browser page.click --ref -x --json -- p'),scripts.join('\n'));
});
