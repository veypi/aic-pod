import test from 'node:test';
import assert from 'node:assert/strict';
import { BrowserInput } from './browser-input.js';
const wheel = (dy=1, extra={}) => ({type:'wheel',x:100,y:100,delta_x:0,delta_y:dy,modifiers:0,...extra});
function fixture(t, send) {
  const sent=[], errors=[];
  const stream={send:async raw=>{const b=JSON.parse(new TextDecoder().decode(raw));return send ? send(b) : sent.push(b);},close:async()=>{stream.closed=true;}};
  const input=new BrowserInput(stream,()=> 'doc',e=>errors.push(e));
  t.after(()=>input.close());
  return {input,stream,sent,errors};
}
test('a touchpad burst keeps only the latest unsent wheel behind local backpressure', async t => {
  let ack, inFlight=0, peak=0; const sent=[];
  const f=fixture(t, b=>{
    sent.push(b); peak=Math.max(peak,++inFlight);
    return new Promise(r=>{ack=()=>{inFlight--;r();};});
  });
  f.input.enqueue(wheel()); const pending=f.input.flush();
  for(let n=0;n<2000;n++) f.input.enqueue(wheel(0.25,{delta_x:0.125}));
  assert.equal(f.input.queue.length,1);
  assert.equal(f.stream.closed,undefined); assert.deepEqual(f.errors,[]);
  ack(); await new Promise(r=>setImmediate(r));
  ack(); await pending;
  assert.equal(peak,1); assert.equal(sent.length,2);
  const events=sent.flatMap(b=>b.events);
  assert.deepEqual(events,[wheel(),wheel(0.25,{delta_x:0.125})]);
});
test('latest scroll replaces obsolete direction and target, while modifiers and discrete events fence it', async t => {
  const {input,sent}=fixture(t);
  const events=[wheel(2),wheel(3),wheel(-4),wheel(-1),wheel(5,{x:101}),
    wheel(6,{modifiers:8}),{type:'pointer.down',button:'left',x:1,y:1},
    wheel(7),{type:'pointer.up',button:'left',x:1,y:1},
    {type:'key.down',key:'a'},{type:'text',text:'a'},{type:'key.up',key:'a'}];
  for(const e of events) input.enqueue(e);
  await input.flush();
  assert.deepEqual(sent[0].events,events.slice(4));
});
test('pointer motion is coalesced before transport without dropping button transitions', async t => {
  const {input,sent}=fixture(t);
  input.enqueue({type:'pointer.down',x:0,y:0,button:'left'});
  for(let n=0;n<500;n++) input.enqueue({type:'pointer.move',x:n,y:n,button:'none'});
  input.enqueue({type:'pointer.up',x:499,y:499,button:'left'});
  await input.flush();
  assert.deepEqual(sent[0].events.map(e=>[e.type,e.x]),[['pointer.down',0],['pointer.move',499],['pointer.up',499]]);
});
test('batches bind the document at enqueue and respect event and UTF-8 byte limits', async t => {
  const {input,sent}=fixture(t);
  let doc='old'; input.document=()=>doc;
  input.enqueue({type:'text',text:'old'}); doc='new';
  for(let n=0;n<65;n++) input.enqueue({type:'key.up',key:'a'});
  for(let n=0;n<4;n++) input.enqueue({type:'text',text:'好'.repeat(4000)});
  await input.flush();
  assert.equal(sent[0].document_id,'old');
  assert.ok(sent.slice(1).every(b=>b.document_id==='new'));
  assert.ok(sent.every(b=>b.events.length<=32 && new TextEncoder().encode(JSON.stringify(b)).length<=24*1024));
  assert.equal(sent.flatMap(b=>b.events).length,70);
  assert.deepEqual(sent.map(b=>b.seq),sent.map((_,n)=>n+1));
});
test('scheduled input is discarded on release and a late send error stays local', async t => {
  let reject;
  const {input,errors,sent}=fixture(t,()=>new Promise((_r,j)=>reject=j));
  input.enqueue(wheel()); const pending=input.flush();
  input.enqueue({type:'text',text:'stale'}); await input.close();
  reject(new Error('late')); await pending;
  assert.equal(input.queue.length,0); assert.deepEqual(errors,[]); assert.deepEqual(sent,[]);
});
test('normal input waits for the short batching window', async t => {
  const {input,sent}=fixture(t);
  input.enqueue(wheel()); input.enqueue(wheel());
  assert.equal(sent.length,0);
  await new Promise(r=>setTimeout(r,25));
  assert.equal(sent.length,1); assert.equal(sent[0].events[0].delta_y,1);
});

test('an idle input channel emits no heartbeat or synthetic activity', async t => {
  const {input,sent}=fixture(t);
  t.mock.timers.enable({apis:['setTimeout','setInterval']});
  t.mock.timers.tick(30000);
  await Promise.resolve();
  assert.deepEqual(sent,[]); assert.equal(input.queue.length,0);
});

test('interleaved move and wheel have only two latest slots before a click', async t => {
  const {input,sent}=fixture(t);
  for(let n=0;n<2000;n++) {
    input.enqueue({type:'pointer.move',x:n%500,y:1,modifiers:0});
    input.enqueue(wheel(n%2?3:-4,{x:n%500}));
  }
  assert.equal(input.queue.length,2);
  input.enqueue({type:'pointer.down',x:499,y:1,button:'left'});
  input.enqueue(wheel(9)); input.enqueue(wheel(-2));
  input.enqueue({type:'pointer.up',x:499,y:1,button:'left'});
  await input.flush();
  assert.deepEqual(sent[0].events.map(e=>e.type),['pointer.move','wheel','pointer.down','wheel','pointer.up']);
  assert.equal(sent[0].events[1].delta_y,3); assert.equal(sent[0].events[3].delta_y,-2);
});
test('motion remains replaceable while its input channel is still opening', async t => {
  let open; const sent=[];
  const input=new BrowserInput(new Promise(r=>open=r),()=> 'doc',e=>{throw e;});
  t.after(()=>input.close());
  input.enqueue(wheel(50));const pending=input.flush();
  input.enqueue(wheel(-2));
  open({send:async raw=>sent.push(JSON.parse(new TextDecoder().decode(raw))),close:async()=>{}});
  await pending;
  assert.equal(sent.length,1);assert.deepEqual(sent[0].events,[wheel(-2)]);
});
