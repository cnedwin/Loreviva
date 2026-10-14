import {chromium,expect} from '@playwright/test';
import assert from 'node:assert/strict';
import {mkdir,writeFile,readFile} from 'node:fs/promises';
import {createHash} from 'node:crypto';
import path from 'node:path';
const env=process.env,base=env.LOREVIVA_STUDIO_URL,output=env.LOREVIVA_STUDIO_EVIDENCE,kind=env.LOREVIVA_STUDIO_SETTINGS_KIND;
if(!base||!output||!env.LOREVIVA_STUDIO_SETTINGS_BEARER||!env.LOREVIVA_TEST_CHROMIUM)throw Error('explicit fixture required');
await mkdir(output,{recursive:true});
const browser=await chromium.launch({executablePath:env.LOREVIVA_TEST_CHROMIUM,headless:true,chromiumSandbox:true});
const context=await browser.newContext({viewport:{width:1100,height:900}}),page=await context.newPage();
page.setDefaultTimeout(6000);const errors=[],csp=[],steps=[];
page.on('pageerror',e=>errors.push(e.message));await page.exposeFunction('recordCSP',e=>csp.push(e));
await page.addInitScript(()=>document.addEventListener('securitypolicyviolation',e=>window.recordCSP({directive:e.violatedDirective,blocked:e.blockedURI})));
const headers={Authorization:'Bearer '+env.LOREVIVA_STUDIO_SETTINGS_BEARER};
try{
 const settingsPath='/world/v1/admin/settings?scope_type='+kind+'&tenant_id=demo-local'+(kind==='PRODUCT'?'&product_id=coffee':'');
 const settings=await context.request.get(base+settingsPath,{headers});assert.equal(settings.status(),200);const snapshot=await settings.json();assert.equal(snapshot.basis.scope.scope_type,kind);
 const denied=await context.request.get(base+'/world/v1/admin/content-drafts?product_id=coffee',{headers});assert.equal(denied.status(),env.LOREVIVA_STUDIO_SETTINGS_CASE==='TENANT_DISABLED_HOME'?401:403);
 const invalid=await context.request.get(base+settingsPath,{headers:{Authorization:'Bearer synthetic-invalid-not-registered'}});assert.equal(invalid.status(),401);steps.push('actual settings 200 / content denied '+denied.status()+' / invalid credential 401');
 await page.goto(base+'/studio/');
 if(env.LOREVIVA_STUDIO_SETTINGS_CASE!=='TENANT_DISABLED_HOME'){
  await page.getByLabel('操作员凭据').fill(env.LOREVIVA_STUDIO_BEARER);await page.getByRole('button',{name:'连接',exact:true}).click();await page.locator('.draft-card').first().waitFor();
  await page.locator('.draft-card').first().getByRole('button',{name:'打开草稿 →'}).click();await page.getByRole('heading',{name:'对象属性'}).waitFor();await page.getByRole('button',{name:'退出凭据'}).click();
 }
 await page.getByLabel('操作员凭据').fill(env.LOREVIVA_STUDIO_SETTINGS_BEARER);await page.getByRole('button',{name:'连接',exact:true}).click();
 await page.getByRole('heading',{name:'七模块默认设置'}).waitFor();assert(new URL(page.url()).pathname.endsWith('/settings'));assert.equal(await page.getByRole('combobox',{name:'范围',exact:true}).inputValue(),kind);assert.equal(await page.locator('.object-tree').count(),0);steps.push('actual App enters authorized settings scope and hides prior private editor');
 await page.getByRole('link',{name:/工作空间$/}).click();await page.getByRole('alert').first().waitFor();assert.equal(await page.locator('.draft-card').count(),0);steps.push('settings entry does not grant content access');
 await page.getByRole('link',{name:/设置$/}).click();await page.getByRole('heading',{name:'七模块默认设置'}).waitFor();await page.screenshot({path:path.join(output,'settings-entry.png'),fullPage:true});
 await page.getByRole('button',{name:'退出凭据'}).click();await page.getByLabel('操作员凭据').fill('synthetic-invalid-not-registered');await page.getByRole('button',{name:'连接',exact:true}).click();await page.getByRole('alert').first().waitFor();assert.equal(await page.getByRole('heading',{name:'七模块默认设置'}).count(),0);
 assert.deepEqual(errors,[]);assert.deepEqual(csp,[]);
 const manifest_sha256=createHash('sha256').update(await readFile(new URL('../internal/app/studio_dist/manifest.json',import.meta.url))).digest('hex');
 await writeFile(path.join(output,'result.json'),JSON.stringify({status:'PASS',case:env.LOREVIVA_STUDIO_SETTINGS_CASE,steps,errors,csp,manifest_sha256},null,2)+'\n');console.log(JSON.stringify({status:'PASS',case:env.LOREVIVA_STUDIO_SETTINGS_CASE,steps}));
}catch(e){await page.screenshot({path:path.join(output,'failure.png'),fullPage:true});console.log(JSON.stringify({status:'FAIL',case:env.LOREVIVA_STUDIO_SETTINGS_CASE,steps,error:e.message}));throw e}finally{await browser.close()}
