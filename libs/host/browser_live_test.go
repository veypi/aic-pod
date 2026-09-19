package host

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/aic-pod/libs/rtc"
	"github.com/veypi/aic-pod/protocol/hosts"
)

// Optional real Chromium -> SDK -> Pion -> device test, including input latency. Only signaling and
// ticket delivery use this isolated HTTP fixture; business traffic uses RTC.
func TestBrowserSDKLive(t *testing.T) {
	electron, ui := os.Getenv("AIC_TEST_ELECTRON"), os.Getenv("AIC_TEST_UI")
	if electron == "" || ui == "" {
		t.Skip("set AIC_TEST_ELECTRON and AIC_TEST_UI for the browser integration test")
	}
	root := t.TempDir()
	outside := t.TempDir()
	for n := range 500 {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("%04d_%s.txt", n, strings.Repeat("x", 140))), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Register the local provider before constructing the runtime. Its socket is
	// installed by the isolated Electron fixture before any device command runs.
	providersMu.Lock()
	previous, hadPrevious := providers["browser"]
	providers["browser"] = Provider{Direct: &ShellChannel{Addr: "127.0.0.1:1", Token: "pending"}}
	providersMu.Unlock()
	t.Cleanup(func() {
		providersMu.Lock()
		defer providersMu.Unlock()
		if hadPrevious {
			providers["browser"] = previous
		} else {
			delete(providers, "browser")
		}
	})
	backend, err := New(Options{Key: "host_1.1.secret.owner", WorkDir: root}).NewCommandService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close(context.Background())
	signals := make(chan *proto.RtcSignal, 256)
	service, err := rtc.New(rtc.Config{HostID: "host_1", Commands: backend, Send: func(signal *proto.RtcSignal) {
		select {
		case signals <- signal:
		default:
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	key, _ := hosts.DirectKey("secret", "host_1")
	mux := http.NewServeMux()
	mux.HandleFunc("/fixture-file-paths", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"workspace": filepath.ToSlash(root), "outside": filepath.ToSlash(outside)})
	})
	mux.HandleFunc("/browser-provider", func(w http.ResponseWriter, r *http.Request) {
		var ch ShellChannel
		if err := json.NewDecoder(r.Body).Decode(&ch); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		providersMu.Lock()
		providers["browser"] = Provider{Direct: &ch}
		providersMu.Unlock()
	})
	mux.HandleFunc("/device-page", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html><title>AI device window</title><style>body{margin:0;background:rgb(78,90,123);height:1800px}input{position:absolute;left:80px;top:30px;width:200px;height:40px}</style><input id="edit"><div id="clock" style="position:fixed;right:10px;top:10px;color:white"></div><script>window.presses=0;window.wheelTotal=0;window.aiCount=0;addEventListener('mousedown',()=>window.presses++);addEventListener('wheel',e=>window.wheelTotal+=Math.abs(e.deltaY));function animate(){document.querySelector('#clock').textContent=Date.now();requestAnimationFrame(animate)}animate()</script>`)
	})
	mux.HandleFunc("/vhtml.js", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join(ui, "../../vhtml/dist/vhtml.min.js"))
	})
	mux.HandleFunc("/env.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		fmt.Fprint(w, `export default async($mod,all)=>{all.define('$hosts',window.fixtureHosts);all.define('$message',{error:message=>(window.fixtureMessages||=[]).push(message)});$mod.$i18n.load(await(await fetch('/langs.json')).json());$mod.$i18n.setLocale('en-US')}`)
	})
	mux.HandleFunc("/routes.js", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	mux.HandleFunc("/signal", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			var signal proto.RtcSignal
			if json.NewDecoder(r.Body).Decode(&signal) != nil {
				w.WriteHeader(400)
				return
			}
			service.HandleSignal(&signal)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case signal := <-signals:
				raw, _ := json.Marshal(signal)
				fmt.Fprintf(w, "data: %s\n\n", raw)
				w.(http.Flusher).Flush()
			}
		}
	})
	mux.HandleFunc("/api/hosts/host_1/direct-tickets", func(w http.ResponseWriter, r *http.Request) {
		var args struct {
			PCID        string `json:"pc_id"`
			Fingerprint string `json:"fingerprint"`
			Connection  string `json:"connection_id"`
		}
		if json.NewDecoder(r.Body).Decode(&args) != nil {
			w.WriteHeader(400)
			return
		}
		ticket, err := hosts.SignTicket(key, hosts.Ticket{HostID: "host_1", UserID: "owner", CredentialVersion: 1, PCID: args.PCID, Fingerprint: args.Fingerprint, ConnectionID: args.Connection}, time.Now())
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"ticket": ticket})
	})
	mux.HandleFunc("/test.html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<!doctype html><title>hosts integration test</title>")
	})
	mux.Handle("/", http.FileServer(http.Dir(ui)))
	server := httptest.NewServer(mux)
	defer server.Close()
	script := filepath.Join(t.TempDir(), "browser.cjs")
	browserScript, _ := json.Marshal(browserTest)
	modulePath, _ := filepath.Abs("../../desktop/browser-tool.mjs")
	moduleURL, _ := json.Marshal("file://" + modulePath)
	main := `const {app,BrowserWindow,BaseWindow,webContents,nativeImage}=require(process.argv[2]);
const fs=require('node:fs'),net=require('node:net'),os=require('node:os'),path=require('node:path');
app.setPath('userData',process.argv[3]);
app.whenReady().then(async()=>{
 const window=new BrowserWindow({show:true,width:1100,height:800,webPreferences:{backgroundThrottling:false}});
 const device=new BaseWindow({show:false,width:700,height:500});
 const {startBrowserServer}=await import(` + string(moduleURL) + `);
 const provider=await startBrowserServer({host:{win:device}});
 await fetch(process.argv[4]+'/browser-provider',{method:'POST',body:JSON.stringify({Addr:'127.0.0.1:'+provider.port,Token:provider.token})});
 // Actual AI command over the authenticated provider channel; no frontend tab creation.
 const sid='hosts-browser-live';
 const ai=argv=>new Promise((resolve,reject)=>{const socket=net.createConnection({host:'127.0.0.1',port:provider.port});let data='';socket.on('error',reject);socket.on('connect',()=>socket.write(JSON.stringify({token:provider.token,session_id:sid,session_dir:path.join(os.homedir(),'.aic','sessions',sid),granted_level:3,argv:[...argv,'--after','none','--format','json']})+'\n'));socket.on('data',chunk=>{data+=chunk;if(data.includes('\n')){socket.end();const response=JSON.parse(data);response.state==='completed'?resolve():reject(Error(data))}})});
 await ai(['open',process.argv[4]+'/device-page']);
 const perfDispatch=[];let perfDispatched=0;
 const originalInputFor=provider.adapter.inputFor;
 provider.adapter.inputFor=id=>{const input=originalInputFor(id),wheel=input.wheel;input.wheel=async value=>{const at=Date.now(),id=value.dy>=1000&&value.dy%1000===0?(perfDispatched+=value.dy/1000):null;await wheel(value);if(id!==null)perfDispatch.push({id,at,done:Date.now()})};return input};
 await webContents.fromId((await provider.adapter.tabs.list())[0].id).executeJavaScript("(()=>{const bar=document.createElement('div');bar.style.cssText='position:fixed;left:0;top:0;width:256px;height:32px;display:flex;z-index:2147483647;pointer-events:none';for(let i=0;i<8;i++){const cell=document.createElement('div');cell.style.cssText='width:32px;height:32px;background:black';bar.append(cell)}document.body.append(bar);window.perfEvents=[];let perfCount=0;addEventListener('wheel',e=>{if(e.deltaY<1000||e.deltaY%1000)return;const n=perfCount+=e.deltaY/1000;if(n>255)return;e.preventDefault();window.perfEvents.push({id:n,at:Date.now()});[...bar.children].forEach((cell,i)=>cell.style.background=(n&(1<<i))?'white':'black')},{passive:false})})()");

 let captures=0;
 const cdpSend=provider.adapter.cdp.send;
 provider.adapter.cdp.send=(id,method,args)=>{if(method==='Page.captureScreenshot')captures++;return cdpSend(id,method,args)};
 // Inject two invalid first frames into each new static blank window. A
 // reconnect restarts this sequence, so only in-stream repaint recovery passes.
 const watchFrames=provider.adapter.watchFrames,badFrames=new Map();
 let injectedFrames=0,repaints=0,expectedBlankPixel;
 provider.adapter.watchFrames=(id,frame,closed)=>{
   const blank=webContents.fromId(id)?.getURL()==='about:blank';
   const state={remaining:2,waiting:false};
   if(blank)badFrames.set(id,state);
   const off=watchFrames(id,bytes=>{
     if(blank && state.waiting)return;
     if(blank && state.remaining>0){
       state.remaining--;state.waiting=true;injectedFrames++;
       frame(state.remaining===1?Buffer.alloc(0):Buffer.from([255,216,255,217]));
     }else {
       if(blank&&!expectedBlankPixel){
         const image=nativeImage.createFromBuffer(bytes),bitmap=image.toBitmap(),index=(360*image.getSize().width+640)*4;
         expectedBlankPixel=[bitmap[index+2],bitmap[index+1],bitmap[index],bitmap[index+3]];
       }
       frame(bytes);
     }
   },closed);
   return ()=>{if(badFrames.get(id)===state)badFrames.delete(id);off()};
 };
 const invalidateFrame=provider.adapter.invalidateFrame;
 provider.adapter.invalidateFrame=id=>{repaints++;const state=badFrames.get(id);if(state)state.waiting=false;return invalidateFrame(id)};
 let aiFailure;
 const aiTimer=setInterval(()=>ai(['eval','--code','window.aiCount++']).catch(e=>aiFailure=e),100);

 window.webContents.on('console-message',event=>console.log(event.message));
 try {
  await window.loadURL(process.argv[4]+'/test.html');
  const result=await window.webContents.executeJavaScript(` + string(browserScript) + `);
  if(!expectedBlankPixel||result.blankPixel.some((v,i)=>Math.abs(v-expectedBlankPixel[i])>1))throw Error('blank canvas differs from device frame: '+JSON.stringify({actual:result.blankPixel,expected:expectedBlankPixel}));
  fs.writeFileSync('/private/tmp/aic-browser-device.png',(await window.webContents.capturePage()).toPNG());
  window.setSize(560,680);await new Promise(r=>setTimeout(r,250));
  const overflow=await window.webContents.executeJavaScript('document.querySelector("header").scrollWidth>document.querySelector("header").clientWidth');
  if(overflow)throw Error('compact header overflows');
  fs.writeFileSync('/private/tmp/aic-browser-compact.png',(await window.webContents.capturePage()).toPNG());
  await window.webContents.executeJavaScript('window.fixtureComponent.destroy()');
  const tabs=await provider.adapter.tabs.list();if(tabs.length!==1)throw Error('viewer disposed device window');
  const wc=webContents.fromId(tabs[0].id);
  const state=await wc.executeJavaScript('({value:document.querySelector("input").value,size:[innerWidth,innerHeight],presses:window.presses,wheelTotal:window.wheelTotal,aiCount:window.aiCount})');
  if(state.value!=='Hello 中文'||state.size[0]!==1280||state.size[1]!==720||state.presses!==1)throw Error('device state: '+JSON.stringify(state));
  clearInterval(aiTimer);if(aiFailure)throw aiFailure;if(injectedFrames!==2||repaints<2)throw Error('missing bad-frame recovery: '+JSON.stringify({injectedFrames,repaints}));if(captures!==0||state.wheelTotal<780||!state.aiCount)throw Error('live/AI concurrency: '+JSON.stringify({captures,state}));

  const events=await wc.executeJavaScript('window.perfEvents');
  const percentiles=values=>{const a=values.sort((a,b)=>a-b);return {n:a.length,p50:a[Math.floor((a.length-1)*.5)],p95:a[Math.floor((a.length-1)*.95)],max:a.at(-1)}};
  const metrics=result.perf.groups.map(group=>({...group,inputQueueMs:percentiles(perfDispatch.filter(d=>d.id>=group.first&&d.id<=group.last).map(d=>d.at-result.perf.sent[d.id])),cdpMs:percentiles(perfDispatch.filter(d=>d.id>=group.first&&d.id<=group.last).map(d=>d.done-d.at)),pageInputMs:percentiles(events.filter(d=>d.id>=group.first&&d.id<=group.last).map(d=>d.at-result.perf.sent[d.id])),inputToCanvasMs:percentiles(result.perf.displayed.filter(d=>d.id>=group.first&&d.id<=group.last).map(d=>d.latency)),decodeMs:percentiles(result.perf.displayed.filter(d=>d.id>=group.first&&d.id<=group.last).map(d=>d.decode))}));
  for(const metric of metrics)if(metric.inputToCanvasMs.p95>500)throw Error('input-to-canvas latency regression: '+JSON.stringify(metric));
  console.log('PERF '+JSON.stringify({metrics,network:result.perf.network}));delete result.perf;
  console.log(JSON.stringify({...result,browser:state,captures,injectedFrames,repaints,deviceHidden:!device.isVisible()}));app.exit(0);
 }catch(error){fs.writeFileSync('/private/tmp/aic-browser-live-failure.png',(await window.webContents.capturePage()).toPNG());console.error(error.stack||error);app.exit(1)}
});`
	if err = os.WriteFile(script, []byte(main), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
	defer cancel()
	// Electron's main-process module is provided by the binary itself.
	cmd := exec.CommandContext(ctx, electron, script, "electron", t.TempDir(), server.URL)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err = cmd.Run(); err != nil {
		t.Fatalf("browser integration: %v\n%s", err, output.String())
	}
	if !strings.Contains(output.String(), `"ok":true`) {
		t.Fatalf("missing browser result: %s", output.String())
	}
	t.Log(output.String())
}

const browserTest = `(async()=>{
 const {HostConnection,collect}=await import('/hosts/connection.js');
 const {HostSession}=await import('/hosts/session.js');
 const {HostContent}=await import('/hosts/content.js');
 const assert=(ok,message)=>{if(!ok)throw new Error(message)};
 let events;
 const nc={sub(_subject,callback){events=new EventSource('/signal');events.onmessage=event=>callback(null,{string:()=>event.data});return ()=>events.close()},pub(_subject,body){fetch('/signal',{method:'POST',body:JSON.stringify(body)}).catch(()=>{})}};
 const connection=new HostConnection({id:'host_1',caps:{transports:{rtc:{enabled:true,protocol:'hosts/1',commands:['fs','browser']}}}},{nc,fetch:async(url,opts)=>{const r=await fetch(url,{method:opts.method,headers:{'Content-Type':'application/json'},body:JSON.stringify(opts.body)});if(!r.ok)throw new Error(await r.text());return r.json()}});
 const content=new HostContent(),session=new HostSession(connection,content);
 try{
  await session.ensure();await connection.renew();assert((await session.catalog()).commands.some(c=>c.name==='fs'),'command catalog');
  const root=await session.fs.home();
  const {default:createFS}=await import('/assets/libs/fs.js');
  const paths=await(await fetch('/fixture-file-paths')).json();
  const hid='a'.repeat(32),files=createFS({$hosts:{async open(id){assert(id===hid,'host routing changed');const s=new HostSession(connection,content);await s.ensure();return s}}});
  try {
   const base='/'+hid+paths.outside,full=base+'/direct 中文%2F.txt';
   assert(await files.home('/'+hid)==='/'+hid+paths.workspace,'workspace must only set initial directory');
   await files.mkdir(base+'/new/nested',{parents:true,existOk:true});
   const saved=await files.put(full,'outside workspace\r\n',{createOnly:true});
   const read=await files.get(full);assert(read.content==='outside workspace\r\n','full path RTC read outside workspace');
   assert((await files.ls(base)).items.some(e=>e.path===full),'listing rewrote full native path');
   const preview=await files.resolve(full,{purpose:'preview'});
   try{assert(await(await fetch(preview.url)).text()===read.content,'full path preview bytes')}finally{await preview.close()}
   const moved=base+'/moved 中文%2F.txt';await files.mv(full,moved,{ifVersion:saved.version});
   await files.rm(moved,{ifVersion:(await files.stat(moved)).version});
  } finally {files.reset()}

  const listing=await session.fs.list(root,{limit:500});assert(listing.entries.length===500,'large JSON byte-source result');
  const path=root.child('live 中文%.txt'),body='\ufeff'+('line\r\n'.repeat(20000))+'last';
  const saved=await session.fs.write(path,body,{createOnly:true});
  const read=await session.fs.readText(path);assert(read.text===body,'text bytes/BOM/CRLF changed');
  const next=await session.fs.write(path,body+'!',{ifVersion:saved.version});
  let conflict=false;try{await session.fs.write(path,'stale',{ifVersion:saved.version})}catch(e){conflict=e.code==='version_conflict'}assert(conflict,'stale write accepted');
  const source=await session.fs.read(path),lease=await source.url({purpose:'download'});
  const ranged=await fetch(lease.url,{headers:{Range:'bytes=3-9'}});assert(ranged.status===206,'HTTP Range status');assert((await ranged.arrayBuffer()).byteLength===7,'HTTP Range length');
  const data=new Uint8Array(await (await fetch(lease.url)).arrayBuffer());assert(new TextDecoder('utf-8',{ignoreBOM:true}).decode(data)===body+'!','service worker full download');
  const empty=root.child('empty');await session.fs.write(empty,new Uint8Array(),{createOnly:true});assert((await session.fs.readText(empty)).text==='','empty source handshake');
  const copy=await session.fs.copy(path,root.child('copy'),{ifVersion:next.version});assert(copy.completed===1,'copy count');
  await session.fs.remove(copy.entry.path,{ifVersion:copy.entry.version});
  const browser=await (` + deviceBrowserTest + `);
  const held=await session.fs.read(path),reader=held.stream().getReader();await reader.read();
  const start=Date.now();await session.close();assert(Date.now()-start<2000,'session close blocked on paused reader');
  return {ok:true,entries:listing.entries.length,bytes:data.length,rtc:connection.state,...browser};
 }finally{content.reset();await session.close();connection.close();events?.close()}
})()`

const deviceBrowserTest = `(async()=>{
 const {HostSession}=await import('/hosts/session.js');
 const browser=session.command('browser');
 const windows=(await browser.request('list')).windows;
 assert(windows.length===1 && windows[0].title==='AI device window','AI window discovery');
 const target=windows[0],args={id:target.id,surface_epoch:target.surface_epoch};
 const until=async(fn,label)=>{const end=Date.now()+15000;while(Date.now()<end){if(await fn())return;await new Promise(r=>setTimeout(r,10))}throw Error('Timed out: '+label)};
 let frames=0, lastFrame, streamError;
 const live=await browser.live('view',args,{item:(metadata,bytes)=>{assert(bytes[0]===255&&bytes[1]===216,'raw JPEG live frame');frames++;lastFrame=metadata},closed:e=>{streamError=e}});
 await until(()=>frames>0,'first pushed frame');
 const second=new HostSession(connection,content);await second.ensure();
 const other=await second.command('browser').live('view',args);
 const started=performance.now(), baseline=frames;
 // Consecutive wheel input while earlier events change the page. No frame token
 // or control lease is involved, and a second viewer can send simultaneously.
 for(let i=0;i<40;i++) {
  await (i%2?live:other).send({events:[{kind:'wheel',value:{x:500,y:400,dy:i<20?20:-20,dx:0,mode:0}}]});
  await new Promise(r=>setTimeout(r,16));
 }
 await new Promise(r=>setTimeout(r,250));
 const frameRate=Math.round((frames-baseline)*1000/(performance.now()-started));
 assert(!streamError,'continuous input failed: '+streamError?.message);
 assert(frameRate>=40,'live frame rate too low: '+frameRate);
 await live.close();await second.close();

 // Paint cumulative wheel displacement as binary pixels, then decode the actual
 // returned JPEG. This measures input-to-picture latency, not command send time.
 const perfCanvas=new OffscreenCanvas(1280,720),perfContext=perfCanvas.getContext('2d',{willReadFrequently:true});
 const perfSent={},perfDisplayed=[],perfArrivals=[];
 let lastDisplayed=0;
 const perfLive=await browser.live('view',args,{delivery:'latest',item:async(_meta,bytes)=>{
   const received=performance.now(),image=await createImageBitmap(new Blob([bytes],{type:'image/jpeg'}));
   perfContext.drawImage(image,0,0);image.close();
   let id=0;for(let i=0;i<8;i++)if(perfContext.getImageData(i*32+16,16,1,1).data[0]>127)id|=1<<i;
   if(perfSent[id]&&lastDisplayed!==id){lastDisplayed=id;perfDisplayed.push({id,latency:Date.now()-perfSent[id],decode:performance.now()-received})}
   perfArrivals.push(performance.now());
 }});
 const perfGroups=[];
 let serial=0;
 for(const hz of [60,120]){
   const first=serial+1,started=performance.now(),arrivalStart=perfArrivals.length;
   for(let i=0;i<hz;i++){
     const id=++serial;perfSent[id]=Date.now();
     await perfLive.send({events:[{kind:'wheel',value:{x:500,y:400,dx:0,dy:1000,mode:0}}]});
     const wait=started+(i+1)*1000/hz-performance.now();if(wait>0)await new Promise(r=>setTimeout(r,wait));
   }
   const queuedFor=Math.round(performance.now()-started);
   await until(()=>lastDisplayed===serial,'last high-rate wheel displayed');
   const total=Math.round(performance.now()-started);
   assert(total-queuedFor<500,'high-rate input accumulated a stale backlog: '+JSON.stringify({hz,queuedFor,total}));
   perfGroups.push({hz,first,last:serial,queuedFor,total,frames:perfArrivals.length-arrivalStart});
 }
 const stats=await connection.pc.getStats();
 const network=[...stats.values()].filter(s=>s.type==='candidate-pair'&&s.state==='succeeded'&&s.nominated).map(s=>({rttMs:s.currentRoundTripTime===undefined?null:s.currentRoundTripTime*1000,localType:stats.get(s.localCandidateId)?.candidateType,remoteType:stats.get(s.remoteCandidateId)?.candidateType}));
 await perfLive.close();
 const perf={groups:perfGroups,sent:perfSent,displayed:perfDisplayed,network};
 window.fixtureHosts={directory:{list:async()=>[{id:'host_1',name:'Test device',status:'enabled',caps:{transports:{rtc:{enabled:true,protocol:'hosts/1',commands:['fs','browser']}}}}]},open:async()=>{const s=new HostSession(connection,content);await s.ensure();return s}};
 // A non-desktop frontend has no native bridge. Reading one is a test failure.
 Object.defineProperty(window,'aicDesktop',{get(){throw Error('Desktop browser fallback accessed')}});
 document.head.insertAdjacentHTML('beforeend','<link rel="stylesheet" href="/assets/libs/font-awesome/css/all.min.css"><style>html,body{margin:0;width:100%;height:100%;background:#f7f7f8;color:#222;--bg-color:#f7f7f8;--bg-color-secondary:#fff;--text-color:#222;--text-color-secondary:#666;--border-color:#ddd;--color-primary:#3875cf}page-local-browser{height:100%;display:block}</style>');
 document.body.innerHTML='<page-local-browser></page-local-browser>';
 const {default:VHTML}=await import('/vhtml.js');window.fixtureComponent=new VHTML(document.body);await fixtureComponent.ready;
 await until(()=>document.querySelector('button.window'),'device window in Browser UI');
 document.querySelector('button.window').click();
 await until(()=>document.querySelector('.frames canvas')?.width===1280,'device canvas frame');
 assert(![...document.querySelectorAll('button')].some(b=>/Take control|Release control/.test(b.textContent)),'exclusive control UI remains');
 const canvas=document.querySelector('.frames canvas'),r=canvas.getBoundingClientRect(),scale=Math.min(r.width/1280,r.height/720),x=r.x+(r.width-1280*scale)/2+120*scale,y=r.y+(r.height-720*scale)/2+50*scale;
 canvas.dispatchEvent(new MouseEvent('mousedown',{bubbles:true,clientX:x,clientY:y,button:0,buttons:1}));
 canvas.dispatchEvent(new MouseEvent('mouseup',{bubbles:true,clientX:x,clientY:y,button:0,buttons:0}));
 const keyboard=document.querySelector('textarea');
 keyboard.dispatchEvent(new InputEvent('beforeinput',{bubbles:true,cancelable:true,inputType:'insertText',data:'Hello '}));
 keyboard.dispatchEvent(new CompositionEvent('compositionstart',{bubbles:true}));keyboard.dispatchEvent(new CompositionEvent('compositionend',{bubbles:true,data:'中文'}));
 await new Promise(r=>setTimeout(r,600));
 assert(!document.querySelector('iframe'),'iframe fallback exists');
 assert(!(window.__vhtml_dev?.errors||[]).length,JSON.stringify(window.__vhtml_dev?.errors));
 assert(!(window.fixtureMessages||[]).length,JSON.stringify(window.fixtureMessages));
 const mainWidth=document.querySelector('main').getBoundingClientRect().width;
 document.querySelector('.sidebar-toggle').click();
 await until(()=>document.querySelector('main').getBoundingClientRect().width>mainWidth,'sidebar collapse');
 document.querySelector('.sidebar-toggle').click();
 await until(()=>document.querySelector('aside').getBoundingClientRect().width>0,'sidebar expansion');
 // Device-scoped add replaces the old device selector. Closing this new window
 // leaves the original AI window and its input state intact.
 document.querySelector('.new-window').click();
 await until(()=>document.querySelectorAll('button.window').length===2 && document.querySelector('.address').value==='about:blank' && document.querySelector('.frames canvas')?.width===1280 && !document.querySelector('.window-row.active .window-close')?.disabled,'new window selected');
 const blankPixel=document.querySelector('.frames canvas').getContext('2d').getImageData(640,360,1,1).data;
 assert(blankPixel[3]===255,'blank window did not recover an opaque frame: '+[...blankPixel]);
 // Row deletion also works without selecting or reconnecting to that window.
 await browser.call('create',{url:'about:blank',viewport:{width:1920,height:1080}});
 document.querySelector('.aside-heading button').click();
 await until(()=>document.querySelectorAll('button.window').length===3,'background window discovered');
 const selectedButton=document.querySelector('.window-row.active .window'),selectedCanvas=document.querySelector('.frames canvas');
 const backgroundButton=[...document.querySelectorAll('button.window')].find(b=>b.title.endsWith('1920 × 1080'));
 backgroundButton.parentElement.querySelector('.window-close').click();
 await until(()=>document.querySelectorAll('button.window').length===2&&!document.querySelector('.window-close').disabled,'background window deleted');
 assert(document.querySelector('.window-row.active .window')===selectedButton&&document.querySelector('.frames canvas')===selectedCanvas,'deleting background window changed the viewer');
 assert((await browser.request('list')).windows.length===2,'row deletion did not close the device window');
 document.querySelector('.window-row.active .window-close').click();
 await until(()=>document.querySelectorAll('button.window').length===1 && document.querySelector('.address').disabled,'new window closed');
 document.querySelector('button.window').click();
 await until(()=>document.querySelector('.frames canvas')?.width===1280,'original device canvas restored');
 assert(!(window.fixtureMessages||[]).length,JSON.stringify(window.fixtureMessages));
 const resized=(await browser.request('list')).windows[0];assert(resized.viewport.width===1280&&resized.viewport.height===720,'viewer resized device');
 return {windows:windows.length,frameRate,blankPixel:[...blankPixel],perf};
})()`
