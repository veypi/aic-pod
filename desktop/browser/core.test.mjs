import {test} from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import {createBrowserHandler} from './core.mjs';
import {CDPBrowser,keyEvent} from './cdp.mjs';
async function fixture(t) {
 const root=await fs.mkdtemp(path.join(os.tmpdir(),'aic-browser-test-'));t.after(()=>fs.rm(root,{recursive:true,force:true}));
 const listeners=new Set(),state={revision:1,calls:[],live:true,failObserve:false,controls:[]};
 state.emitDialog=dialog=>{state.dialog=dialog;for(const listener of listeners)listener({tab:1,dialog})};
 const adapter={tabs:{list:async()=>state.live?[{id:1,title:'Test',url:'http://test/'}]:[],remove:async()=>{state.live=false;state.onClose?.()}},revision:()=>state.revision,dialog:()=>state.dialog,onDialog:listener=>{listeners.add(listener);return ()=>listeners.delete(listener)},cdp:{},encodeImage:async()=>({bytes:Buffer.from('image'),width:100,height:50,mime:'image/png'})};
 class Driver {
  async observe(){await state.onObserve?.();if(state.failObserve)throw Error('capture failed');return {elements:[{role:'button',name:'Save',backendNodeId:10},{role:'textbox',name:'Name',backendNodeId:11}],page:{title:'Test',url:'http://test/',width:200,height:100,scrollX:0,scrollY:0}}}
  async inspect(){return {text:'text',value:'old'}}
  async act(_t,op,e,args){state.calls.push({op,e,args});await state.onAction?.();return {delivery:'cdp_input'}}
  async send(_t,method,args){assert.equal(method,'Page.handleJavaScriptDialog');state.controls.push(args);return state.onControl?.(args)}
  async evaluate(){return {width:200,height:100,scrollX:0,scrollY:0}}
  async screenshot(){return {bytes:Buffer.from('image'),logical:{width:200,height:100}}}
 }
 const handler=createBrowserHandler(adapter,Driver);
 t.after(()=>handler.dispose());
 const run=async(argv,sid='one',level=3,extra={})=>JSON.parse((await handler({sessionID:sid,sessionDir:root,grantedLevel:level,...extra},{argv:[...argv,'--format','json']})).content);
 async function bind(sid='one'){const list=await run(['target','list'],sid);return run(['target','use',list.data[0].id],sid)}
 return {run,bind,state,handler};
}
test('refs bind to session, target and latest observation; denied input never runs',async t=>{
 const {run,bind,state}=await fixture(t);const first=await bind(),ref=first.observation.elements[0].ref;
 assert.equal((await run(['click',ref],'one',1)).state,'rejected');assert.equal(state.calls.length,0);
 assert.equal((await run(['click',ref,'--after','none'])).state,'completed');assert.equal(state.calls.length,1);
 assert.equal((await run(['click',ref])).error.code,'stale_ref');
 const next=await run(['snapshot']);await bind('two');
 assert.equal((await run(['click',next.observation.elements[0].ref])).error.code,'stale_ref');
 const other=await run(['snapshot'],'two');assert.equal((await run(['click',other.observation.elements[0].ref])).error.code,'stale_ref');
});
test('external revision and closed targets fail before input',async t=>{
 const {run,bind,state}=await fixture(t);const r=await bind();state.revision++;
 assert.equal((await run(['click',r.observation.elements[0].ref])).error.code,'stale_ref');assert.equal(state.calls.length,0);
 state.live=false;assert.equal((await run(['snapshot'])).error.code,'target_required');
});
test('coordinates are mapped from compressed screenshot; observation failure keeps action success',async t=>{
 const {run,bind,state}=await fixture(t);await bind();const shot=await run(['screenshot']);
 const r=await run(['click','--at','50','25','--snapshot',shot.observation.snapshot,'--after','none']);
 assert.equal(r.state,'completed');assert.deepEqual(state.calls[0].e.at,[100,50]);
 await run(['snapshot']);state.failObserve=true;
 const action=await run(['click','--role','button','--name','Save']);assert.equal(action.state,'error'); // semantic resolution failed before action
 state.failObserve=false;const s=await run(['snapshot']);state.failObserve=true;
 const performed=await run(['click',s.observation.elements[0].ref]);assert.equal(performed.state,'completed');assert.equal(performed.action.performed,true);assert.equal(performed.warnings[0].code,'observation_failed');
});
test('expired work and ended sessions do not execute',async t=>{
 const {run,bind,state,handler}=await fixture(t);const r=await bind();
 assert.equal((await run(['click',r.observation.elements[0].ref],'one',3,{deadline:new Date(0).toISOString()})).error.code,'timeout');
 handler.endSession('one');assert.equal((await run(['click',r.observation.elements[0].ref])).error.code,'target_required');assert.equal(state.calls.length,0);
});
test('keys use portable modifier chords and reject unknown combinations',()=>{
 assert.equal(keyEvent('ControlOrMeta+A','darwin').modifiers,4);assert.equal(keyEvent('ControlOrMeta+A','linux').modifiers,2);
 assert.throws(()=>keyEvent('Meta+Meta+A'));assert.throws(()=>keyEvent('Danger'));assert.equal(keyEvent('Enter').text,'\r');
});
test('query matches name, role and value, excluding metadata keys and ref IDs',async t=>{
 const {run,bind}=await fixture(t);await bind();
 const named=await run(['snapshot','--query','nAME']);assert.deepEqual(named.observation.elements.map(e=>e.name),['Name']);
 assert.equal((await run(['snapshot','--query','BUTTON'])).observation.elements[0].name,'Save');
 for(const query of ['role','enabled','backendNodeId','ref',named.observation.snapshot]){
  assert.equal((await run(['snapshot','--query',query])).observation.elements.length,0,query);
 }
});

test('synchronous prompt yields the response while retaining execution ownership',async t=>{
 const {run,bind,state}=await fixture(t);await bind();
 const entered=Promise.withResolvers(),resume=Promise.withResolvers();
 state.onAction=()=>{entered.resolve();return resume.promise};
 const pending=run(['click','--role','button','--name','Save']);await entered.promise;
 const queued=run(['fill','--label','Name','--text','must not run']);
 state.emitDialog({type:'prompt',message:'Choose',default_prompt:'default'});
 const interrupted=await pending;assert.equal(interrupted.error.code,'dialog_open');assert.equal(interrupted.action.performed,'unknown');
 assert.equal((await queued).error.code,'browser_busy');assert.equal(state.calls.length,1);
 assert.equal((await run(['target','list'])).state,'completed');assert.equal((await run(['target','current'])).state,'completed');
 assert.equal((await run(['dialog','accept'],'one',1)).state,'rejected');assert.equal(state.controls.length,0);
 state.onControl=()=>{state.emitDialog(null);resume.resolve()};
 const resolved=await run(['dialog','accept','--text','中文答复']);assert.equal(resolved.state,'completed');assert.equal(resolved.data.resumed.op,'click');assert.equal(resolved.data.resumed.action.performed,true);
 assert.deepEqual(state.controls,[{accept:true,promptText:'中文答复'}]);assert.equal(state.calls.length,1);
 assert.equal((await run(['snapshot'])).state,'completed');
});

test('nested dialogs yield controls without releasing the original input lane',async t=>{
 const {run,bind,state}=await fixture(t);await bind();const resume=Promise.withResolvers();
 state.onAction=()=>{state.emitDialog({type:'alert',message:'first'});return resume.promise};
 assert.equal((await run(['click','--role','button','--name','Save'])).error.code,'dialog_open');
 state.onControl=()=>{state.emitDialog(null);state.emitDialog({type:'confirm',message:'second'})};
 const nested=await run(['dialog','accept']);assert.equal(nested.error.code,'dialog_open');assert.equal(nested.data.dialog.message,'second');
 assert.equal((await run(['snapshot'])).error.code,'browser_busy');
 state.onControl=()=>{state.emitDialog(null);resume.resolve()};
 assert.equal((await run(['dialog','dismiss'])).state,'completed');
 assert.equal((await run(['snapshot'])).state,'completed');assert.equal(state.calls.length,1);
});

test('deadline returns unknown without releasing pending input; close can recover',async t=>{
 const {run,bind,state}=await fixture(t);await bind();const resume=Promise.withResolvers();
 state.onAction=()=>resume.promise;state.onClose=()=>resume.resolve();
 const expired=await run(['click','--role','button','--name','Save','--timeout','20ms']);
 assert.equal(expired.error.code,'timeout');assert.equal(expired.action.performed,'unknown');
 assert.equal((await run(['click','--role','button','--name','Save'])).error.code,'browser_busy');
 assert.equal((await run(['close'])).state,'completed');assert.equal(state.calls.length,1);
 assert.deepEqual((await run(['target','list'])).data,[]);
});

test('input releases remain possible after a modal outlives the action deadline',async()=>{
 const events=[];
 const driver=new CDPBrowser({cdp:{attach:async()=>{},send:async(_tab,_method,args)=>{events.push(args.type);if(['keyDown','mousePressed'].includes(args.type))throw Error('deadline exceeded after dispatch')},cleanup:async(_tab,_method,args)=>{events.push(args.type)}}});
 await assert.rejects(driver.press(1,'Enter'),/deadline/);assert.deepEqual(events,['keyDown','keyUp']);
 events.length=0;driver.point=async()=>[10,20];
 await assert.rejects(driver.act(1,'click',{},{}),/deadline/);assert.deepEqual(events,['mouseMoved','mousePressed','mouseReleased']);
});

test('a later dialog during control observation can be resolved after input completed',async t=>{
 const {run,bind,state}=await fixture(t);await bind();const input=Promise.withResolvers(),observe=Promise.withResolvers();
 state.onAction=()=>{state.emitDialog({type:'alert',message:'input'});return input.promise};
 await run(['click','--role','button','--name','Save']);
 state.onControl=()=>{state.emitDialog(null);input.resolve()};
 state.onObserve=async()=>{state.onObserve=null;state.emitDialog({type:'alert',message:'later'});await observe.promise};
 const interrupted=await run(['dialog','accept']);assert.equal(interrupted.error?.code,'dialog_open');assert.equal(interrupted.data.dialog.message,'later');
 assert.equal((await run(['target','list'])).state,'completed');
 state.onControl=()=>{state.emitDialog(null);observe.resolve()};
 assert.equal((await run(['dialog','dismiss'])).state,'completed');
 assert.equal((await run(['snapshot'])).state,'completed');
});
