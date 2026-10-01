import test from 'node:test';
import assert from 'node:assert/strict';
import { createBrowserWheel, containPoint } from './browser-viewer.js';
function fixture() {
  const sent=[]; let time=0, owner={};
  const rect={x:0,y:0,width:500,height:500}, viewport={width:1000,height:600};
  const wheel=createBrowserWheel({point:e=>containPoint(rect,viewport,e.clientX,e.clientY),
    send:(_kind,data)=>sent.push(data),viewport:()=>viewport,enabled:()=>owner,now:()=>time});
  function event(x,y,deltaMode=0) {
    return {clientX:x,clientY:y,deltaX:2,deltaY:4,deltaMode,
      preventDefault(){this.prevented=true;},stopPropagation(){this.stopped=true;}};
  }
  return {sent,wheel,event,time:t=>{time=t;},owner:v=>{owner=v;}};
}
test('wheel gesture stays on its first remote target across image and window edges',()=>{
  const f=fixture();
  f.wheel.handle(f.event(499,399));
  for (const [x,y] of [[499,401],[501,399],[498,398],[-1,0]]) {
    f.time(50); const e=f.event(x,y); f.wheel.handle(e);
    assert.equal(e.prevented,true); assert.equal(e.stopped,true);
  }
  assert.equal(f.sent.length,5);
  assert.ok(f.sent.every(e=>e.x===998 && e.y===598 && e.dy===8));
  f.time(250); const outside=f.event(500,400); f.wheel.handle(outside);
  assert.equal(outside.prevented,undefined); assert.equal(f.sent.length,5);
  f.wheel.handle(f.event(20,120)); assert.equal(f.sent.at(-1).x,40);
});
test('inactive viewers and gestures starting in letterboxing do not capture scrolling',()=>{
  const f=fixture(), blank=f.event(20,20);
  f.wheel.handle(blank); assert.equal(blank.prevented,undefined);
  f.owner(false); const inside=f.event(20,120); f.wheel.handle(inside);
  assert.equal(inside.prevented,undefined); assert.deepEqual(f.sent,[]);
});
test('line and page units use the remote viewport and new ownership clears the anchor',()=>{
  const f=fixture(); f.wheel.handle(f.event(10,110,1));
  assert.equal(f.sent[0].dy,320);
  f.wheel.reset(); f.wheel.handle(f.event(10,110,2));
  assert.equal(f.sent[1].dy,2400); assert.equal(f.sent[1].dx,2000);
  f.owner({}); f.wheel.handle(f.event(30,130));
  assert.equal(f.sent[2].x,60); assert.equal(f.sent[2].y,60);
});
test('small pointer jitter does not interrupt scrolling, but deliberate movement retargets it',()=>{
  const f=fixture(); f.wheel.handle(f.event(100,200));
  assert.equal(f.wheel.jitter(f.event(102,198)),true);
  assert.equal(f.wheel.jitter(f.event(120,200)),false);
  f.wheel.handle(f.event(120,200));
  assert.equal(f.sent.at(-1).x,240);
});
