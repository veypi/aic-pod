import {test} from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import {parse, required, render, result, envelope} from './protocol.mjs';
const cases = JSON.parse(fs.readFileSync(new URL('../../protocol/ui/testdata/cases.json',import.meta.url)));
for(const c of cases) test(`${c.domain} ${JSON.stringify(c.argv)}`,()=>{
 if(c.error) return assert.throws(()=>parse(c.domain,c.argv),e=>e.code===c.error);
 const o=parse(c.domain,c.argv);assert.equal(o.op,c.op);assert.equal(required(o),c.level);
 if(c.args) assert.deepEqual(o.args,c.args);
 if(c.locator) assert.deepEqual(o.locator,c.locator);
});
test('result metadata lives in content; only images live in attrs',()=>{
 const r=result(parse('browser',['snapshot']));r.observation={snapshot:'s1',text:'中文\n[error]\npage content'};
 const e=envelope(r,'json',{image_data:'data:image/png;base64,x'});
 assert.deepEqual(Object.keys(e.attrs),['image_data']);assert.deepEqual(JSON.parse(e.content),r);
 assert.match(render(r),/^\[ui\/1\] browser.snapshot/);
});
